//go:build integration

package testenv

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"iChat/application"
	"iChat/database"
	"iChat/health"
	"iChat/messaging/rabbitmq"
	"iChat/workers"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"
)

type shutdownProbeConnector struct {
	base    driver.Connector
	armed   atomic.Bool
	entered chan context.Context
	release chan struct{}
}

func (c *shutdownProbeConnector) Driver() driver.Driver { return c.base.Driver() }
func (c *shutdownProbeConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return shutdownProbeConn{Conn: conn, owner: c}, nil
}

type shutdownProbeConn struct {
	driver.Conn
	owner *shutdownProbeConnector
}

func (c shutdownProbeConn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, o)
}
func (c shutdownProbeConn) Ping(ctx context.Context) error {
	err := c.Conn.(driver.Pinger).Ping(ctx)
	if err == nil && c.owner.armed.CompareAndSwap(true, false) {
		c.owner.entered <- ctx
		<-c.owner.release
	}
	return err
}

func shutdownReplaced(t *testing.T, r *persistenceRuntimeCapture, ctx context.Context) {
	t.Helper()
	if err := r.inputs.Consumers[0].Session.(*rabbitmq.ConsumerSession).Close(ctx); err != nil {
		t.Fatal(err)
	}
	lifecycleEventually(t, func() bool {
		return r.service.Status().Generation == 2 && r.inputs.Health.RuntimeReadiness().Consumers == health.Ready
	})
}
func shutdownOperation(t *testing.T, entered <-chan context.Context) context.Context {
	t.Helper()
	select {
	case ctx := <-entered:
		return ctx
	case <-time.After(15 * time.Second):
		t.Fatal("SQL shutdown barrier not entered")
		return nil
	}
}
func shutdownRuntimePending(t *testing.T, r *persistenceRuntimeCapture, pool *sql.DB, ctx context.Context) {
	t.Helper()
	r.stop()
	lifecycleAwait(t, r.done)
	if !errors.Is(r.err, application.ErrRuntimeShutdown) || !errors.Is(r.err, workers.ErrPersistenceSupervisorPending) || !errors.Is(r.err, context.DeadlineExceeded) {
		t.Fatal("runtime did not report pending supervisor work", r.err)
	}
	if persistenceSupervisorDone(r.service.Done()) || r.service.ConsumerReady() || r.inputs.Health.RuntimeReadiness().Consumers != health.Stopping {
		t.Fatal("runtime observer timeout released ownership or revived health")
	}
	if pool.Stats().InUse == 0 {
		t.Fatal("blocked SQL callback lost its connection lease")
	}
	if err := pool.PingContext(ctx); err != nil {
		t.Fatal("pending supervisor lost shared SQL", err)
	}
}

func (f *persistenceLifecycleFixture) shutdownRecoverFirst(t *testing.T, receipt rabbitmq.PublishResult) {
	t.Helper()
	session := f.session(t, f.broker)
	stream, err := session.OpenConsumer(f.ctx, rabbitmq.PersistQueue, 1)
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := stream.Next(f.ctx)
	if err != nil || delivery.Message.CorrelationID != receipt.PublishAttemptID || delivery.Message.Redelivered {
		t.Fatal("unadmitted original attempt was changed", err)
	}
	repo, err := database.NewMessageRepository(f.pool)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := workers.NewPersistenceHandler(repo)
	if err != nil {
		t.Fatal(err)
	}
	result, err := handler.Handle(f.ctx, delivery)
	if err != nil || result.Disposition != database.CommandApplied {
		t.Fatal("original pending command did not recover", err)
	}
	if err := stream.Close(f.ctx); err != nil {
		t.Fatal(err)
	}
}

func TestPersistenceShutdownIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	f := newPersistenceLifecycleFixture(t, ctx)
	base, err := mysql.NewConnector(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	connector := &persistenceSupervisorConnector{base: base, faults: make(chan struct{}, 1)}
	shared := sql.OpenDB(connector)
	t.Cleanup(func() { shared.Close() })
	f.pool = shared
	connections := func() int {
		var records []struct{ VHost string }
		if _, err := f.env.rabbitAPI(ctx, http.MethodGet, "/api/connections", nil, &records); err != nil {
			return -1
		}
		n := 0
		for _, record := range records {
			if record.VHost == f.broker.VHost {
				n++
			}
		}
		return n
	}
	lifecycleEventually(t, func() bool { return connections() == 2 })
	run := func(name string, test func(*testing.T)) {
		if !t.Run(name, test) {
			t.FailNow()
		}
		f.pool = shared
		f.checkEmpty(t)
		lifecycleEventually(t, func() bool { return connections() == 2 })
	}
	run("replaced_generation_gracefully_finishes_real_commit_and_ack", func(t *testing.T) {
		pool, entered, release := f.barrierPool(t)
		defer release()
		f.pool = pool
		r := f.supervisedRuntime(t, "")
		shutdownReplaced(t, r, ctx)
		command, _ := f.publish(t)
		operation := shutdownOperation(t, entered)
		if f.count(t, command) != 1 {
			t.Fatal("COMMIT barrier preceded real persistence")
		}
		r.stop()
		short, stop := context.WithTimeout(ctx, 20*time.Millisecond)
		err := r.service.Drain(short)
		stop()
		if !errors.Is(err, workers.ErrPersistenceSupervisorPending) || operation.Err() != nil || r.inputs.Health.RuntimeReadiness().Consumers != health.Stopping {
			t.Fatal("graceful drain canceled admitted SQL/ACK or kept readiness", err)
		}
		release()
		r.close(t)
		f.checkDurableSet(t, command)
		if err := pool.PingContext(ctx); err != nil {
			t.Fatal("graceful close released caller pool", err)
		}
		if r.service.Status().Generation != 2 {
			t.Fatal("drain started another generation")
		}
	})
	run("runtime_returns_pending_but_real_committed_callback_still_owns_sql", func(t *testing.T) {
		pool, entered, release := f.barrierPool(t)
		defer release()
		f.pool = pool
		o := application.DefaultRuntimeOptions()
		o.DrainTimeout, o.CloseTimeout = 150*time.Millisecond, 150*time.Millisecond
		r := f.supervisedRuntimeWithOptions(t, "", o, true)
		shutdownReplaced(t, r, ctx)
		command, receipt := f.publish(t)
		operation := shutdownOperation(t, entered)
		shutdownRuntimePending(t, r, pool, ctx)
		if operation.Err() == nil || f.count(t, command) != 1 {
			t.Fatal("forced shutdown lost committed state or failed to cancel work")
		}
		release()
		lifecycleAwait(t, r.service.Done())
		if !errors.Is(r.service.Err(), context.Canceled) || r.service.Status().Generation != 2 {
			t.Fatal("late callback lost cancellation or revived recovery", r.service.Err())
		}
		f.recover(t, receipt, database.CommandReplayed)
		f.checkDurableSet(t, command)
		// The explicit owner releases storage only after supervisor join and
		// reconciliation, not when RunRuntime's bounded observer returned.
		if err := pool.Close(); err != nil {
			t.Fatal(err)
		}
		if err := pool.PingContext(ctx); err == nil {
			t.Fatal("explicit SQL owner did not release pool")
		}
		f.pool = shared
	})
	run("stop_during_partial_real_replacement_handshake_does_not_reconnect", func(t *testing.T) {
		proxy := startSessionProxy(t, f.broker.Address)
		r := f.supervisedRuntime(t, proxy.address)
		proxy.stallAfter.Store(4)
		if err := r.inputs.Consumers[0].Session.(*rabbitmq.ConsumerSession).Close(ctx); err != nil {
			t.Fatal(err)
		}
		lifecycleEventually(t, func() bool { return proxy.accepted.Load() == 5 })
		lifecycleEventually(t, func() bool { return connections() == 4 }) // Two fixture + quarantine + partial first worker.
		command, receipt := f.publish(t)
		r.close(t)
		proxy.stallAfter.Store(0)
		if proxy.accepted.Load() != 5 || r.service.Status().Generation != 2 || r.inputs.Health.RuntimeReadiness().Consumers != health.Stopping {
			t.Fatal("stopped replacement retried or became ready")
		}
		if f.count(t, command) != 0 {
			t.Fatal("partial group admitted a business transaction")
		}
		f.shutdownRecoverFirst(t, receipt)
		f.checkDurableSet(t, command)
	})
	run("late_successful_real_ping_cannot_join_early_or_revive_ready", func(t *testing.T) {
		base, err := mysql.NewConnector(f.dsn)
		if err != nil {
			t.Fatal(err)
		}
		probe := &shutdownProbeConnector{base: base, entered: make(chan context.Context, 1), release: make(chan struct{})}
		var once sync.Once
		release := func() { once.Do(func() { close(probe.release) }) }
		defer release()
		pool := sql.OpenDB(probe)
		t.Cleanup(func() { pool.Close() })
		f.pool = pool
		o := application.DefaultRuntimeOptions()
		o.DrainTimeout, o.CloseTimeout = 150*time.Millisecond, 150*time.Millisecond
		r := f.supervisedRuntimeWithOptions(t, "", o, true)
		shutdownReplaced(t, r, ctx)
		probe.armed.Store(true)
		operation := shutdownOperation(t, probe.entered)
		shutdownRuntimePending(t, r, pool, ctx)
		if operation.Err() == nil {
			t.Fatal("late SQL probe was not canceled")
		}
		release()
		lifecycleAwait(t, r.service.Done())
		if r.service.ConsumerReady() || r.inputs.Health.RuntimeReadiness().Consumers != health.Stopping || r.service.Status().Generation != 2 {
			t.Fatal("late probe revived a stopped generation")
		}
		if err := pool.PingContext(ctx); err != nil {
			t.Fatal(err)
		}
		if err := pool.Close(); err != nil {
			t.Fatal(err)
		}
		f.pool = shared
	})
	run("actual_ack_loss_then_graceful_runtime_exit_preserves_original_attempt", func(t *testing.T) {
		proxy := startSessionProxy(t, f.broker.Address)
		proxy.dropConsumerAcks.Store(true)
		r := f.supervisedRuntime(t, proxy.address)
		shutdownReplaced(t, r, ctx)
		command, receipt := f.publish(t)
		lifecycleEventually(t, func() bool { return proxy.consumerAcksDropped.Load() == 1 })
		f.checkDurableSet(t, command)
		r.close(t)
		if r.service.Status().Generation != 2 {
			t.Fatal("ACK loss triggered automatic replay")
		}
		f.recover(t, receipt, database.CommandReplayed)
		f.checkDurableSet(t, command)
	})
	run("stopping_real_database_backoff_never_resumes_after_storage_returns", func(t *testing.T) {
		r := f.supervisedRuntime(t, "")
		if err := f.env.SetMySQLRunning(ctx, false); err != nil {
			t.Fatal(err)
		}
		restored := false
		restart := func() {
			if err := f.env.SetMySQLRunning(ctx, true); err != nil {
				t.Fatal(err)
			}
			conf, err := f.env.Config()
			if err != nil {
				t.Fatal(err)
			}
			f.dsn = f.dsn.Clone()
			f.dsn.Addr = net.JoinHostPort(conf.MYSQL.Host, conf.MYSQL.Port)
			base, err := mysql.NewConnector(f.dsn)
			if err != nil {
				t.Fatal(err)
			}
			connector.mu.Lock()
			connector.base = base
			connector.mu.Unlock()
			restored = true
		}
		t.Cleanup(func() {
			if !restored {
				r.stop()
				restart()
			}
		})
		lifecycleEventually(t, func() bool { return r.service.Status().Failures >= 2 })
		command, receipt := f.publish(t)
		f.awaitQueueMessages(t, 1)
		r.close(t)
		restart()
		probe, stop := context.WithTimeout(ctx, 30*time.Second)
		defer stop()
		for shared.PingContext(probe) != nil {
			select {
			case <-probe.Done():
				t.Fatal("owned MySQL did not recover")
			case <-time.After(200 * time.Millisecond):
			}
		}
		if r.service.Status().Generation != 1 || r.service.ConsumerReady() || r.inputs.Health.RuntimeReadiness().Consumers != health.Stopping {
			t.Fatal("closed backoff supervisor resumed after SQL recovered")
		}
		f.shutdownRecoverFirst(t, receipt)
		f.checkDurableSet(t, command)
	})
	var gaps int
	if err := shared.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation c WHERE
		c.last_seq<>(SELECT COUNT(*) FROM message m WHERE m.conversation_id=c.id) OR
		c.last_seq<>(SELECT COALESCE(MAX(m.seq),0) FROM message m WHERE m.conversation_id=c.id)`).Scan(&gaps); err != nil || gaps != 0 {
		t.Fatal("shutdown recovery left sequence gaps", err)
	}
}
