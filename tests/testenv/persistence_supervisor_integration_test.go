//go:build integration

package testenv

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"iChat/database"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
	"iChat/workers"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"
)

// Fixture-only source. Production MQResources/aggregate readiness still need a
// replaceable owner; this source does not bypass the default worker/all gate.
type persistenceSupervisorSource struct {
	fixture *persistenceLifecycleFixture
	calls   atomic.Int64
	closed  atomic.Int64
}

type persistenceSupervisorGroup struct {
	source   *persistenceSupervisorSource
	sessions []*rabbitmq.ConsumerSession
}

func (s *persistenceSupervisorSource) prepare(ctx context.Context) (workers.PersistenceConsumerGroup, error) {
	s.calls.Add(1)
	g := &persistenceSupervisorGroup{source: s}
	for i := 0; i < 2; i++ {
		session, err := rabbitmq.DialConsumer(ctx, s.fixture.broker.AMQPURL(), s.fixture.management,
			s.fixture.broker.VHost, rabbitmq.DefaultConnectionOptions())
		if session != nil {
			g.sessions = append(g.sessions, session)
		}
		if err != nil {
			return g, err
		}
	}
	return g, nil
}

func (g *persistenceSupervisorGroup) Consumers() []workers.PersistenceConsumer {
	consumers := make([]workers.PersistenceConsumer, len(g.sessions))
	for i, session := range g.sessions {
		consumers[i] = workers.PersistenceConsumer{Session: session, Prefetch: 2}
	}
	return consumers
}

func (g *persistenceSupervisorGroup) Close(ctx context.Context) error {
	g.source.closed.Add(1)
	var err error
	for _, session := range g.sessions {
		err = errors.Join(err, session.Close(ctx))
	}
	return err
}

func (f *persistenceLifecycleFixture) supervisor(t *testing.T) (*workers.PersistenceSupervisor, *persistenceSupervisorSource) {
	t.Helper()
	source := &persistenceSupervisorSource{fixture: f}
	o := workers.DefaultPersistenceSupervisorOptions()
	o.BaseDelay, o.MaxDelay = 200*time.Millisecond, 800*time.Millisecond
	o.Worker.PollTimeout, o.Worker.ProbeInterval, o.Worker.ProbeTimeout = 100*time.Millisecond, 100*time.Millisecond, 300*time.Millisecond
	s, err := workers.NewPersistenceSupervisor(f.pool, source.prepare, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Close(cleanup); errors.Is(err, workers.ErrPersistenceSupervisorPending) {
			t.Error("supervisor still owns fixture work", err)
		}
	})
	setup, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	err = s.Start(setup, f.ctx)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	return s, source
}

// Docker can assign a different random host port when restarting this run's
// MySQL. Only this test connector changes address; the supervisor retains the
// same shared sql.DB. The optional COMMIT result fault follows a real COMMIT.
type persistenceSupervisorConnector struct {
	mu          sync.Mutex
	base        driver.Connector
	commitFault atomic.Bool
	faults      chan struct{}
	begins      atomic.Int64
}

func (c *persistenceSupervisorConnector) Driver() driver.Driver { return &mysql.MySQLDriver{} }
func (c *persistenceSupervisorConnector) Connect(ctx context.Context) (driver.Conn, error) {
	c.mu.Lock()
	base := c.base
	c.mu.Unlock()
	conn, err := base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return persistenceSupervisorConn{Conn: conn, connector: c}, nil
}

type persistenceSupervisorConn struct {
	driver.Conn
	connector *persistenceSupervisorConnector
}

func (c persistenceSupervisorConn) Ping(ctx context.Context) error {
	return c.Conn.(driver.Pinger).Ping(ctx)
}
func (c persistenceSupervisorConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	c.connector.begins.Add(1)
	return persistenceSupervisorTx{Tx: tx, connector: c.connector}, nil
}

type persistenceSupervisorTx struct {
	driver.Tx
	connector *persistenceSupervisorConnector
}

func (tx persistenceSupervisorTx) Commit() error {
	err := tx.Tx.Commit()
	if err == nil && tx.connector.commitFault.CompareAndSwap(true, false) {
		tx.connector.faults <- struct{}{}
		return io.EOF
	}
	return err
}

func TestPersistenceSupervisorIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()
	f := newPersistenceLifecycleFixture(t, ctx)
	base, err := mysql.NewConnector(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	connector := &persistenceSupervisorConnector{base: base, faults: make(chan struct{}, 1)}
	pool := sql.OpenDB(connector)
	t.Cleanup(func() { pool.Close() })
	f.pool = pool
	restart := func(t *testing.T) {
		t.Helper()
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
	}
	run := func(name string, test func(*testing.T)) {
		if !t.Run(name, test) {
			t.FailNow()
		}
	}
	run("actual_database_outage_pauses_then_recovers_backlog", func(t *testing.T) {
		s, source := f.supervisor(t)
		if !s.ConsumerReady() || source.calls.Load() != 1 {
			t.Fatal("initial consumers did not activate")
		}
		if err := f.env.SetMySQLRunning(ctx, false); err != nil {
			t.Fatal(err)
		}
		restored := false
		t.Cleanup(func() {
			if !restored {
				s.Close(ctx)
				restart(t)
			}
		})
		lifecycleEventually(t, func() bool { return s.Status().Failures >= 2 && source.closed.Load() == 1 })
		var backlog []messaging.Command
		for i := 0; i < 4; i++ {
			command, _ := f.publish(t)
			backlog = append(backlog, command)
			if receipt, err := f.producer.Publish(ctx, command, time.Now()); err != nil || receipt.Outcome != rabbitmq.PublishAccepted {
				t.Fatal("offline backlog duplicate was not accepted", err)
			}
		}
		f.awaitQueueMessages(t, 8)
		if s.ConsumerReady() || source.calls.Load() != 1 || persistenceSupervisorDone(s.Done()) {
			t.Fatal("database outage spun connections or stopped supervision", s.Status())
		}
		restart(t)
		restored = true
		lifecycleEventually(t, func() bool { return s.ConsumerReady() && s.Status().Generation == 2 })
		f.awaitQueueMessages(t, 0)
		for _, command := range backlog {
			f.checkDurableSet(t, command)
		}
		if err := s.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(ctx); err != nil || source.calls.Load() != 2 || source.closed.Load() != 2 {
			t.Fatal("recovery retained old error/group or created extra generations", err, s.Status())
		}
		if err := pool.PingContext(ctx); err != nil {
			t.Fatal("supervisor closed shared SQL", err)
		}
	})
	run("actual_commit_unknown_recovers_by_idempotent_redelivery", func(t *testing.T) {
		before := connector.begins.Load()
		connector.commitFault.Store(true)
		command, _ := f.publish(t)
		f.awaitQueueMessages(t, 1)
		s, source := f.supervisor(t)
		lifecycleAwait(t, connector.faults)
		lifecycleEventually(t, func() bool { return s.ConsumerReady() && s.Status().Generation == 2 })
		f.awaitQueueMessages(t, 0)
		f.checkDurableSet(t, command)
		if err := s.Close(ctx); err != nil || source.calls.Load() != 2 || connector.begins.Load()-before != 2 {
			t.Fatal("commit unknown did not replay exactly one existing terminal", err, s.Status())
		}
	})
	run("actual_constraint_failure_stays_terminal_and_preserves_attempt", func(t *testing.T) {
		var nextSeq uint64
		if err := pool.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq),0)+1 FROM message").Scan(&nextSeq); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, fmt.Sprintf("ALTER TABLE message ADD CONSTRAINT chk_supervisor_fixture CHECK (seq <> %d)", nextSeq)); err != nil {
			t.Fatal(err)
		}
		dropped := false
		t.Cleanup(func() {
			if !dropped {
				pool.ExecContext(context.Background(), "ALTER TABLE message DROP CHECK chk_supervisor_fixture")
			}
		})
		before := connector.begins.Load()
		command, receipt := f.publish(t)
		s, source := f.supervisor(t)
		lifecycleAwait(t, s.Done())
		var server *mysql.MySQLError
		if !errors.As(s.Err(), &server) || server.Number != 3819 || source.calls.Load() != 1 ||
			connector.begins.Load()-before != 1 || f.count(t, command) != 0 {
			t.Fatal("constraint failure reconnected or lost original SQL error", s.Err())
		}
		if _, err := pool.ExecContext(ctx, "ALTER TABLE message DROP CHECK chk_supervisor_fixture"); err != nil {
			t.Fatal(err)
		}
		dropped = true
		f.recover(t, receipt, database.CommandApplied)
		f.checkDurableSet(t, command)
		f.checkEmpty(t)
	})
	run("initial_database_outage_can_be_stopped_without_opening_consumers", func(t *testing.T) {
		if err := f.env.SetMySQLRunning(ctx, false); err != nil {
			t.Fatal(err)
		}
		restored := false
		t.Cleanup(func() {
			if !restored {
				restart(t)
			}
		})
		s, source := f.supervisor(t)
		lifecycleEventually(t, func() bool { return s.Status().Failures >= 2 })
		command, receipt := f.publish(t)
		f.awaitQueueMessages(t, 1)
		if err := s.Close(ctx); err != nil || source.calls.Load() != 0 || s.ConsumerReady() {
			t.Fatal("stopping outage created consumers or failed", err, s.Status())
		}
		restart(t)
		restored = true
		probe, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		for pool.PingContext(probe) != nil {
			select {
			case <-probe.Done():
				t.Fatal("owned storage did not recover")
			case <-time.After(200 * time.Millisecond):
			}
		}
		if s.ConsumerReady() || source.calls.Load() != 0 || s.Status().State != workers.PersistenceStateStopped {
			t.Fatal("stopped supervisor revived after database recovery")
		}
		// No prior subscription existed, so this original attempt is still a
		// first delivery. Process it explicitly after proving no auto revival.
		session := f.session(t, f.broker)
		stream, err := session.OpenConsumer(ctx, rabbitmq.PersistQueue, 1)
		if err != nil {
			t.Fatal(err)
		}
		delivery, err := stream.Next(ctx)
		if err != nil || delivery.Message.CorrelationID != receipt.PublishAttemptID || delivery.Message.Redelivered {
			t.Fatal("stopped outage changed the original attempt", err)
		}
		repo, _ := database.NewMessageRepository(pool)
		handler, _ := workers.NewPersistenceHandler(repo)
		if result, err := handler.Handle(ctx, delivery); err != nil || result.Disposition != database.CommandApplied {
			t.Fatal("original backlog did not recover", err)
		}
		if err := stream.Close(ctx); err != nil {
			t.Fatal(err)
		}
		f.checkDurableSet(t, command)
		f.checkEmpty(t)
	})
	var gaps int
	if err := pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation c WHERE
		c.last_seq<>(SELECT COUNT(*) FROM message m WHERE m.conversation_id=c.id) OR
		c.last_seq<>(SELECT COALESCE(MAX(m.seq),0) FROM message m WHERE m.conversation_id=c.id)`).Scan(&gaps); err != nil || gaps != 0 {
		t.Fatal("supervised recovery left sequence gaps", err)
	}
}

func persistenceSupervisorDone(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}
