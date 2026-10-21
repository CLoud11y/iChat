//go:build integration

package testenv

import (
	"context"
	"database/sql"
	"errors"
	"iChat/application"
	"iChat/health"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
	"iChat/workers"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"
)

type persistenceRuntimeCapture struct {
	service    *workers.PersistenceSupervisor
	inputs     application.RoleDependencies
	quarantine application.MQConsumer
	stop       context.CancelFunc
	done       chan struct{}
	err        error
}

func (f *persistenceLifecycleFixture) supervisedRuntime(t *testing.T, address string) *persistenceRuntimeCapture {
	return f.supervisedRuntimeWithOptions(t, address, application.DefaultRuntimeOptions(), false)
}

func (f *persistenceLifecycleFixture) supervisedRuntimeWithOptions(t *testing.T, address string, runtimeOptions application.RuntimeOptions, allowPending bool, publisher ...workers.QuarantinePublisher) *persistenceRuntimeCapture {
	t.Helper()
	o := workers.DefaultPersistenceSupervisorOptions()
	o.Worker.PollTimeout, o.Worker.ProbeInterval, o.Worker.ProbeTimeout = 100*time.Millisecond, 100*time.Millisecond, 300*time.Millisecond
	o.BaseDelay, o.MaxDelay, o.SetupTimeout = 200*time.Millisecond, 800*time.Millisecond, 8*time.Second
	if len(publisher) > 1 {
		t.Fatal("runtime fixture accepts at most one borrowed quarantine publisher")
	}
	if len(publisher) == 1 {
		o.Worker.QuarantinePublisher = publisher[0]
	}
	factory, err := application.NewSupervisedPersistenceFactory(f.pool, o)
	if err != nil {
		t.Fatal(err)
	}
	r := &persistenceRuntimeCapture{done: make(chan struct{})}
	captured := make(chan struct{}, 1)
	factories := application.RuntimeFactories{
		Persistence: func(ctx context.Context, inputs application.RoleDependencies) (application.RuntimeService, error) {
			service, err := factory(ctx, inputs)
			if err == nil {
				r.service, r.inputs = service.(*workers.PersistenceSupervisor), inputs
				captured <- struct{}{}
			}
			return service, err
		},
		Quarantine: func(ctx context.Context, inputs application.RoleDependencies) (application.RuntimeService, error) {
			r.quarantine = inputs.Consumers[0].Session
			return persistenceFixtureCompanion(ctx, inputs)
		},
		Outbox: persistenceFixtureCompanion,
	}
	conf := persistenceRuntimeConfig(f)
	if address != "" {
		broker := f.broker
		broker.Address = address
		conf.RABBITMQ.URL = broker.AMQPURL()
	}
	process, stop := context.WithCancel(f.ctx)
	r.stop = stop
	go func() {
		r.err = application.RunRuntime(process, conf, factories, runtimeOptions)
		close(r.done)
	}()
	t.Cleanup(func() {
		stop()
		select {
		case <-r.done:
			if r.err != nil && !allowPending {
				t.Error("supervised runtime cleanup failed", r.err)
			}
		case <-time.After(20 * time.Second):
			t.Error("supervised runtime did not join")
		}
		if allowPending && r.service != nil {
			select {
			case <-r.service.Done():
			case <-time.After(20 * time.Second):
				t.Error("pending supervisor still owns fixture storage")
			}
		}
	})
	select {
	case <-captured:
	case <-r.done:
		t.Fatal("supervised factory did not construct", r.err)
	case <-time.After(30 * time.Second):
		t.Fatal("supervised factory construction timed out")
	}
	lifecycleEventually(t, func() bool { return r.inputs.Health.RuntimeReadiness().Consumers == health.Ready })
	return r
}

func (r *persistenceRuntimeCapture) close(t *testing.T) {
	t.Helper()
	r.stop()
	lifecycleAwait(t, r.done)
	if r.err != nil || r.inputs.Health.RuntimeReadiness().Consumers != health.Stopping {
		t.Fatal("supervised runtime did not shut down cleanly", r.err)
	}
}

func TestPersistenceConnectionsIntegration(t *testing.T) {
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
	connections := func() int {
		var records []struct{ VHost string }
		if _, err := f.env.rabbitAPI(ctx, http.MethodGet, "/api/connections", nil, &records); err != nil {
			return -1
		}
		count := 0
		for _, record := range records {
			if record.VHost == f.broker.VHost {
				count++
			}
		}
		return count
	}
	// Management connection records are sampled; wait for both known owners
	// rather than interpreting a missing first sample as an absent connection.
	lifecycleEventually(t, func() bool { return connections() == 2 })
	baseline := 2
	awaitMessage := func(t *testing.T, command messaging.Command) {
		t.Helper()
		lifecycleEventually(t, func() bool {
			var count int
			return pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM message WHERE id=?", command.CommandID).Scan(&count) == nil && count == 1
		})
		f.awaitQueueMessages(t, 0)
		f.checkDurableSet(t, command)
	}
	checkRetired := func(t *testing.T, r *persistenceRuntimeCapture) {
		t.Helper()
		for _, consumer := range r.inputs.Consumers {
			if !persistenceSupervisorDone(consumer.Session.Done()) {
				t.Fatal("initial connection stayed live after replacement")
			}
		}
		if persistenceSupervisorDone(r.quarantine.Done()) || persistenceSupervisorDone(r.done) || !r.service.ConsumerReady() ||
			r.inputs.Health.RuntimeReadiness().Consumers != health.Ready {
			t.Fatal("new readiness remained tied to retired sessions or quarantine was replaced", r.service.Status())
		}
	}
	run := func(name string, test func(*testing.T)) {
		if !t.Run(name, test) {
			t.FailNow()
		}
		f.checkEmpty(t) // Direct broker RPC after all runtime connections joined.
		lifecycleEventually(t, func() bool { return connections() == baseline })
	}
	run("runtime_database_pause_replaces_connections_and_restores_aggregate_readiness", func(t *testing.T) {
		r := f.supervisedRuntime(t, "")
		if err := f.env.SetMySQLRunning(ctx, false); err != nil {
			t.Fatal(err)
		}
		restored := false
		t.Cleanup(func() {
			if !restored {
				r.stop()
				restart(t)
			}
		})
		lifecycleEventually(t, func() bool { return r.service.Status().Failures >= 2 })
		if r.inputs.Health.RuntimeReadiness().Consumers == health.Ready || persistenceSupervisorDone(r.done) || r.service.Status().Generation != 1 {
			t.Fatal("DB outage was ready, reconnected consumers, or killed runtime", r.service.Status())
		}
		var commands []messaging.Command
		for i := 0; i < 3; i++ {
			command, _ := f.publish(t)
			commands = append(commands, command)
			if receipt, err := f.producer.Publish(ctx, command, time.Now()); err != nil || receipt.Outcome != rabbitmq.PublishAccepted {
				t.Fatal(err)
			}
		}
		f.awaitQueueMessages(t, 6)
		restart(t)
		restored = true
		lifecycleEventually(t, func() bool {
			return r.service.Status().Generation == 2 && r.inputs.Health.RuntimeReadiness().Consumers == health.Ready
		})
		f.awaitQueueMessages(t, 0)
		checkRetired(t, r)
		for _, command := range commands {
			f.checkDurableSet(t, command)
		}
		r.close(t)
		if err := pool.PingContext(ctx); err != nil {
			t.Fatal("runtime closed shared storage", err)
		}
	})
	run("idle_transport_retirement_rebuilds_only_persistence_group", func(t *testing.T) {
		r := f.supervisedRuntime(t, "")
		if err := r.inputs.Consumers[0].Session.(*rabbitmq.ConsumerSession).Close(ctx); err != nil {
			t.Fatal(err)
		}
		lifecycleEventually(t, func() bool {
			return r.service.Status().Generation == 2 && r.inputs.Health.RuntimeReadiness().Consumers == health.Ready
		})
		checkRetired(t, r)
		command, _ := f.publish(t)
		awaitMessage(t, command)
		r.close(t)
	})
	run("replacement_topology_drift_stays_unready_until_manual_repair", func(t *testing.T) {
		r := f.supervisedRuntime(t, "")
		if err := f.env.SetMySQLRunning(ctx, false); err != nil {
			t.Fatal(err)
		}
		restored := false
		t.Cleanup(func() {
			if !restored {
				r.stop()
				restart(t)
			}
		})
		lifecycleEventually(t, func() bool { return r.service.Status().Failures >= 1 })
		command, _ := f.publish(t)
		f.awaitQueueMessages(t, 1)
		if err := f.control.QueueUnbind(rabbitmq.PersistQueue, rabbitmq.SubmitRoutingKey, rabbitmq.CommandsExchange, nil); err != nil {
			t.Fatal(err)
		}
		bound := false
		t.Cleanup(func() {
			if !bound {
				f.control.QueueBind(rabbitmq.PersistQueue, rabbitmq.SubmitRoutingKey, rabbitmq.CommandsExchange, false, nil)
			}
		})
		restart(t)
		restored = true
		lifecycleEventually(t, func() bool { return r.service.Status().Generation >= 3 })
		var bindings []any
		if _, err := f.env.rabbitAPI(ctx, http.MethodGet, "/api/exchanges/"+url.PathEscape(f.broker.VHost)+"/"+rabbitmq.CommandsExchange+"/bindings/source", nil, &bindings); err != nil || len(bindings) != 0 {
			t.Fatal("replacement repaired topology instead of staying unready", err)
		}
		if r.service.ConsumerReady() || r.inputs.Health.RuntimeReadiness().Consumers == health.Ready || persistenceSupervisorDone(r.done) || f.count(t, command) != 0 {
			t.Fatal("unverified topology consumed backlog or stopped runtime", r.service.Status(), r.service.Err())
		}
		if err := f.control.QueueBind(rabbitmq.PersistQueue, rabbitmq.SubmitRoutingKey, rabbitmq.CommandsExchange, false, nil); err != nil {
			t.Fatal(err)
		}
		bound = true
		lifecycleEventually(t, func() bool { return r.inputs.Health.RuntimeReadiness().Consumers == health.Ready })
		f.awaitQueueMessages(t, 0)
		checkRetired(t, r)
		f.checkDurableSet(t, command)
		r.close(t)
	})
	run("later_replacement_handshake_timeout_joins_partial_group_before_retry", func(t *testing.T) {
		proxy := startSessionProxy(t, f.broker.Address)
		r := f.supervisedRuntime(t, proxy.address)
		// Initial connections are two persistence + quarantine. Permit only the
		// first connection of the next generation; blackhole its second handshake.
		proxy.stallAfter.Store(4)
		if proxy.accepted.Load() != 3 {
			t.Fatal("unexpected initial proxy connections", proxy.accepted.Load())
		}
		if err := r.inputs.Consumers[0].Session.(*rabbitmq.ConsumerSession).Close(ctx); err != nil {
			t.Fatal(err)
		}
		lifecycleEventually(t, func() bool { return proxy.accepted.Load() == 5 })
		lifecycleEventually(t, func() bool { return connections() == baseline+2 })
		if r.inputs.Health.RuntimeReadiness().Consumers == health.Ready {
			t.Fatal("partial group was ready")
		}
		lifecycleEventually(t, func() bool { return r.service.Status().Generation >= 3 })
		lifecycleEventually(t, func() bool { return connections() == baseline+1 })
		if persistenceSupervisorDone(r.done) || errors.Is(r.service.Err(), application.ErrMQClose) {
			t.Fatal("partial group recovery lost owner", r.service.Err())
		}
		proxy.stallAfter.Store(0)
		lifecycleEventually(t, func() bool { return r.inputs.Health.RuntimeReadiness().Consumers == health.Ready })
		checkRetired(t, r)
		command, _ := f.publish(t)
		awaitMessage(t, command)
		r.close(t)
	})
	var gaps int
	if err := pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation c WHERE
		c.last_seq<>(SELECT COUNT(*) FROM message m WHERE m.conversation_id=c.id) OR
		c.last_seq<>(SELECT COALESCE(MAX(m.seq),0) FROM message m WHERE m.conversation_id=c.id)`).Scan(&gaps); err != nil || gaps != 0 {
		t.Fatal("runtime replacement left sequence gaps", err)
	}
}
