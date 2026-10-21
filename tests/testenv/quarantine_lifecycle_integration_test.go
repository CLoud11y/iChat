//go:build integration

package testenv

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"iChat/application"
	"iChat/health"
	"iChat/messaging/rabbitmq"
	"iChat/migrations"
	"iChat/workers"

	mysql "github.com/go-sql-driver/mysql"
	amqp "github.com/rabbitmq/amqp091-go"
)

type quarantineLifecycleRuntime struct {
	service     *workers.QuarantineSupervisor
	inputs      application.RoleDependencies
	persistence []application.PreparedConsumer
	stop        context.CancelFunc
	done        chan struct{}
	err         error
}

func quarantineLifecycleStart(t *testing.T, f *persistenceLifecycleFixture, pool *sql.DB, options application.RuntimeOptions, allowPending bool) *quarantineLifecycleRuntime {
	t.Helper()
	o := workers.DefaultQuarantineSupervisorOptions()
	o.Worker.PollTimeout, o.Worker.ProbeInterval, o.Worker.ProbeTimeout = 100*time.Millisecond, 100*time.Millisecond, 300*time.Millisecond
	o.BaseDelay, o.MaxDelay, o.SetupTimeout = 200*time.Millisecond, 800*time.Millisecond, 8*time.Second
	factory, err := application.NewSupervisedQuarantineFactory(pool, o)
	if err != nil {
		t.Fatal(err)
	}
	r := &quarantineLifecycleRuntime{done: make(chan struct{})}
	captured := make(chan struct{}, 1)
	factories := application.RuntimeFactories{
		Persistence: func(ctx context.Context, in application.RoleDependencies) (application.RuntimeService, error) {
			r.persistence = in.Consumers
			return persistenceFixtureCompanion(ctx, in)
		},
		Quarantine: func(ctx context.Context, in application.RoleDependencies) (application.RuntimeService, error) {
			service, err := factory(ctx, in)
			if err == nil {
				r.service, r.inputs = service.(*workers.QuarantineSupervisor), in
				captured <- struct{}{}
			}
			return service, err
		},
		Outbox: persistenceFixtureCompanion,
	}
	process, stop := context.WithCancel(f.ctx)
	r.stop = stop
	go func() {
		r.err = application.RunRuntime(process, persistenceRuntimeConfig(f), factories, options)
		close(r.done)
	}()
	t.Cleanup(func() {
		stop()
		select {
		case <-r.done:
			if r.err != nil && !allowPending {
				t.Error("quarantine runtime cleanup", r.err)
			}
		case <-time.After(20 * time.Second):
			t.Error("quarantine runtime still running")
		}
		if r.service != nil {
			select {
			case <-r.service.Done():
			case <-time.After(20 * time.Second):
				t.Error("quarantine SQL callback has not joined")
			}
		}
	})
	select {
	case <-captured:
	case <-r.done:
		t.Fatal("quarantine construction failed", r.err)
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	lifecycleEventually(t, func() bool { return r.service.ConsumerReady() || persistenceSupervisorDone(r.done) })
	if persistenceSupervisorDone(r.done) || r.inputs.Health.RuntimeReadiness().Consumers != health.Ready {
		t.Fatal("quarantine runtime not ready", r.err)
	}
	return r
}

func (r *quarantineLifecycleRuntime) close(t *testing.T) {
	t.Helper()
	r.stop()
	lifecycleAwait(t, r.done)
	if r.err != nil {
		t.Fatal(r.err)
	}
	lifecycleAwait(t, r.service.Done())
}

func TestQuarantineLifecycleIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
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
	publisher, err := rabbitmq.DialQuarantinePublisher(ctx, f.broker.AMQPURL(), rabbitmq.DefaultSessionOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { publisher.Close(context.Background()) })
	var serial atomic.Int64
	expectedACKs := 0
	input := func(t *testing.T) rabbitmq.QuarantineEnvelope {
		t.Helper()
		n := serial.Add(1)
		q, err := rabbitmq.NewQuarantineEnvelope(rabbitmq.ConsumerMessage{Body: []byte{0xff, 0, '{'}, ContentType: "application/json", DeliveryMode: 2, MessageID: fmt.Sprintf("audit-%s-%d", f.env.RunID(), n), CorrelationID: fmt.Sprintf("source-attempt-%d", n), Exchange: rabbitmq.CommandsExchange, RoutingKey: rabbitmq.SubmitRoutingKey, Headers: amqp.Table{"binary": []byte{0, 255}, "large": int64(9007199254740993)}}, "INVALID_DELIVERY", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	put := func(t *testing.T, q rabbitmq.QuarantineEnvelope) {
		t.Helper()
		if _, err := publisher.Publish(ctx, q); err != nil {
			t.Fatal(err)
		}
		expectedACKs++
	}
	count := func(q rabbitmq.QuarantineEnvelope) int {
		var n int
		if pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM message_quarantine WHERE transport_id=?", q.TransportID()).Scan(&n) != nil {
			return -1
		}
		return n
	}
	awaitAudit := func(t *testing.T, q rabbitmq.QuarantineEnvelope) {
		t.Helper()
		lifecycleEventually(t, func() bool { return count(q) == 1 })
		var body, document []byte
		if err := pool.QueryRowContext(ctx, "SELECT raw_body,raw_headers FROM message_quarantine WHERE transport_id=?", q.TransportID()).Scan(&body, &document); err != nil {
			t.Fatal(err)
		}
		stored, err := rabbitmq.DecodeQuarantineAudit(document)
		if err != nil {
			t.Fatal(err)
		}
		source, _ := q.Source()
		a, _ := stored.Envelope().StableContent()
		b, _ := q.StableContent()
		if !bytes.Equal(body, source.Body) || !bytes.Equal(a, b) {
			t.Fatal("audit lost original bytes/content")
		}
	}
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
	lifecycleEventually(t, func() bool { return connections() == 3 }) // fixture control, command producer, quarantine publisher
	awaitAcknowledged := func(t *testing.T) {
		t.Helper()
		lifecycleEventually(t, func() bool {
			var queue struct {
				MessageStats struct{ Ack int } `json:"message_stats"`
			}
			_, err := f.env.rabbitAPI(ctx, http.MethodGet, "/api/queues/"+url.PathEscape(f.broker.VHost)+"/"+rabbitmq.QuarantineQueue, nil, &queue)
			return err == nil && queue.MessageStats.Ack == expectedACKs
		})
	}
	checkEmpty := func(t *testing.T) {
		t.Helper()
		awaitAcknowledged(t)
		for _, queue := range []string{rabbitmq.PersistQueue, rabbitmq.QuarantineQueue} {
			if _, ok, err := f.control.Get(queue, false); err != nil || ok {
				t.Fatal("runtime left queued/unacknowledged audit", queue, err)
			}
		}
		lifecycleEventually(t, func() bool { return connections() == 3 })
	}
	run := func(name string, test func(*testing.T)) {
		if !t.Run(name, test) {
			t.FailNow()
		}
		checkEmpty(t)
	}
	run("two_workers_commit_then_ack_and_deduplicate", func(t *testing.T) {
		a, b := f.session(t, f.broker), f.session(t, f.broker)
		o := workers.DefaultQuarantineOptions()
		o.PollTimeout = 100 * time.Millisecond
		service, err := workers.NewQuarantineService(pool, []workers.QuarantineConsumer{{Session: a, Prefetch: 2}, {Session: b, Prefetch: 2}}, o)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { service.Close(context.Background()) })
		if err := service.Start(ctx, ctx); err != nil {
			t.Fatal(err)
		}
		q := input(t)
		put(t, q)
		put(t, q)
		awaitAudit(t, q)
		awaitAcknowledged(t)
		// The same original command can also arrive through broker DLX. This
		// fixture Reject triggers actual dead-lettering, not production policy.
		source, err := q.Source()
		if err != nil {
			t.Fatal(err)
		}
		if err := f.control.PublishWithContext(ctx, rabbitmq.CommandsExchange, rabbitmq.SubmitRoutingKey, true, false, amqp.Publishing{Body: source.Body, Headers: source.Headers, ContentType: source.ContentType, DeliveryMode: source.DeliveryMode, MessageId: source.MessageID, CorrelationId: source.CorrelationID}); err != nil {
			t.Fatal(err)
		}
		original := getTopologyDelivery(t, ctx, f.control, rabbitmq.PersistQueue)
		expectedACKs++
		if err := original.Reject(false); err != nil {
			t.Fatal(err)
		}
		awaitAcknowledged(t)
		awaitAudit(t, q)
		service.StopAdmission()
		if err := service.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		if err := service.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})
	run("database_outage_retires_audit_group_and_restores_health", func(t *testing.T) {
		r := quarantineLifecycleStart(t, f, pool, application.DefaultRuntimeOptions(), false)
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
		}
		t.Cleanup(func() {
			if !restored {
				r.stop()
				restart()
			}
		})
		lifecycleEventually(t, func() bool { return r.service.Status().Failures >= 2 })
		if r.inputs.Health.RuntimeReadiness().Consumers == health.Ready || persistenceSupervisorDone(r.done) {
			t.Fatal("outage admitted work", r.service.Status())
		}
		// During graceful MySQL shutdown, a probe can still briefly succeed.
		// Once shutdown has completed, failed probes must not acquire groups.
		offlineGeneration := r.service.Status().Generation
		var pending []rabbitmq.QuarantineEnvelope
		for i := 0; i < 3; i++ {
			q := input(t)
			pending = append(pending, q)
			put(t, q)
			put(t, q)
		}
		lifecycleEventually(t, func() bool {
			q, err := f.control.QueueInspect(rabbitmq.QuarantineQueue)
			return err == nil && q.Messages == 6
		})
		if r.service.Status().Generation != offlineGeneration {
			t.Fatal("offline SQL probes acquired new consumers", r.service.Status())
		}
		restart()
		restored = true
		lifecycleEventually(t, func() bool {
			return r.service.Status().Generation > offlineGeneration && r.inputs.Health.RuntimeReadiness().Consumers == health.Ready
		})
		for _, q := range pending {
			awaitAudit(t, q)
		}
		awaitAcknowledged(t)
		for _, c := range r.inputs.Consumers {
			if !persistenceSupervisorDone(c.Session.Done()) {
				t.Fatal("old audit session remained active")
			}
		}
		for _, c := range r.persistence {
			if persistenceSupervisorDone(c.Session.Done()) {
				t.Fatal("audit recovery retired persistence")
			}
		}
		r.close(t)
		if err := pool.PingContext(ctx); err != nil {
			t.Fatal("runtime closed shared SQL", err)
		}
	})
	run("idle_audit_connection_replacement_uses_current_generation", func(t *testing.T) {
		r := quarantineLifecycleStart(t, f, pool, application.DefaultRuntimeOptions(), false)
		if err := r.inputs.Consumers[0].Session.(*rabbitmq.ConsumerSession).Close(ctx); err != nil {
			t.Fatal(err)
		}
		lifecycleEventually(t, func() bool {
			return r.service.Status().Generation == 2 && r.inputs.Health.RuntimeReadiness().Consumers == health.Ready
		})
		for _, c := range r.persistence {
			if persistenceSupervisorDone(c.Session.Done()) {
				t.Fatal("replaced another role's connection")
			}
		}
		q := input(t)
		put(t, q)
		awaitAudit(t, q)
		awaitAcknowledged(t)
		r.close(t)
	})
	run("graceful_drain_keeps_commit_callback_and_ack_opportunity", func(t *testing.T) {
		barrier, entered, release := f.barrierPool(t)
		defer release()
		o := application.DefaultRuntimeOptions()
		o.DrainTimeout = 3 * time.Second
		r := quarantineLifecycleStart(t, f, barrier, o, false)
		q := input(t)
		put(t, q)
		var operation context.Context
		select {
		case operation = <-entered:
		case <-r.done:
			t.Fatal(r.err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		awaitAudit(t, q)
		r.stop()
		short, done := context.WithTimeout(ctx, 50*time.Millisecond)
		defer done()
		if err := r.service.Drain(short); !errors.Is(err, workers.ErrPersistenceSupervisorPending) {
			t.Fatal("drain did not retain commit callback", err)
		}
		if operation.Err() != nil || persistenceSupervisorDone(r.done) {
			t.Fatal("graceful drain canceled admitted SQL")
		}
		release()
		lifecycleAwait(t, r.done)
		if r.err != nil {
			t.Fatal(r.err)
		}
		lifecycleAwait(t, r.service.Done())
	})
	run("pending_runtime_retains_sql_owner_until_late_callback_joins", func(t *testing.T) {
		barrier, entered, release := f.barrierPool(t)
		defer release()
		o := application.DefaultRuntimeOptions()
		o.DrainTimeout, o.CloseTimeout = 100*time.Millisecond, 100*time.Millisecond
		r := quarantineLifecycleStart(t, f, barrier, o, true)
		q := input(t)
		put(t, q)
		var operation context.Context
		select {
		case operation = <-entered:
		case <-r.done:
			t.Fatal(r.err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		awaitAudit(t, q)
		r.stop()
		lifecycleAwait(t, r.done)
		if r.err == nil || persistenceSupervisorDone(r.service.Done()) || operation.Err() == nil {
			t.Fatal("pending runtime pretended audit callback joined", r.err)
		}
		if err := barrier.PingContext(ctx); err != nil {
			t.Fatal("pending SQL owner released", err)
		}
		release()
		lifecycleAwait(t, r.service.Done())
		// Fixture inspection confirms the original wrapper remained unacked;
		// Nack here only restores it for the real recovery worker below.
		d := getTopologyDelivery(t, ctx, f.control, rabbitmq.QuarantineQueue)
		if !d.Redelivered || !bytes.Equal(d.Body, q.Encode()) {
			t.Fatal("forced audit exit lost source wrapper")
		}
		if err := d.Nack(false, true); err != nil {
			t.Fatal(err)
		}
		recovery := quarantineLifecycleStart(t, f, pool, application.DefaultRuntimeOptions(), false)
		awaitAudit(t, q)
		awaitAcknowledged(t)
		recovery.close(t)
		if count(q) != 1 {
			t.Fatal("late committed audit duplicated")
		}
	})
	run("malformed_wrapper_stops_and_retains_original_delivery", func(t *testing.T) {
		r := quarantineLifecycleStart(t, f, pool, application.DefaultRuntimeOptions(), true)
		q := input(t)
		// Explicit malformed fixture publication; production publisher refuses
		// to manufacture this protocol violation.
		if err := f.control.PublishWithContext(ctx, rabbitmq.QuarantineExchange, rabbitmq.QuarantineRoutingKey, true, false, amqp.Publishing{Body: q.Encode(), ContentType: rabbitmq.QuarantineContentType, Type: rabbitmq.QuarantineMessageType, DeliveryMode: 2, MessageId: "tampered", CorrelationId: "0123456789abcdef0123456789abcdef"}); err != nil {
			t.Fatal(err)
		}
		lifecycleAwait(t, r.done)
		lifecycleAwait(t, r.service.Done())
		if !errors.Is(r.err, rabbitmq.ErrQuarantineInput) || r.service.Status().Generation != 1 || count(q) != 0 {
			t.Fatal("malformed wrapper recovered/committed", r.err)
		}
		d := getTopologyDelivery(t, ctx, f.control, rabbitmq.QuarantineQueue)
		if !d.Redelivered || d.MessageId != "tampered" || !bytes.Equal(d.Body, q.Encode()) {
			t.Fatal("malformed input lost")
		}
		if err := d.Ack(false); err != nil {
			t.Fatal(err)
		} // Fixture cleanup only.
		expectedACKs++
	})
	for _, table := range []string{"message", "message_command", "message_outbox", "conversation", "message_delivery_cursor"} {
		var n int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+table+"`").Scan(&n); err != nil || n != 0 {
			t.Fatal("audit worker wrote business state", table, n, err)
		}
	}
	if err := migrations.ApplyReliable(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := rabbitmq.VerifyLocal(ctx, f.management, f.broker.VHost); err != nil {
		t.Fatal(err)
	}
}
