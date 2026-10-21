package application

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"iChat/config"
	"iChat/messaging/rabbitmq"
	"iChat/workers"
)

func TestQuarantineFactoryIdleAndRoleGate(t *testing.T) {
	connector := &persistenceFactoryConnector{}
	pool := sql.OpenDB(connector)
	defer pool.Close()
	factory, err := NewQuarantineFactory(pool, workers.DefaultQuarantineOptions())
	if err != nil {
		t.Fatal(err)
	}
	input := RoleDependencies{Consumers: []PreparedConsumer{{Queue: rabbitmq.QuarantineQueue, Prefetch: 2, Session: &persistenceFactorySession{done: make(chan struct{})}}}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	service, err := factory(ctx, input)
	if err != nil || connector.connects.Load() != 0 || service.(RuntimeConsumerReadiness).ConsumerReady() {
		t.Fatal("construction performed I/O", err)
	}
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}
	input.Consumers[0].Queue = rabbitmq.PersistQueue
	if _, err := factory(ctx, input); !errors.Is(err, workers.ErrQuarantineServiceInput) {
		t.Fatal("wrong queue accepted", err)
	}
	input.Consumers[0].Queue = rabbitmq.QuarantineQueue
	input.PersistenceConsumers = &PersistenceConsumerSource{}
	if _, err := factory(ctx, input); !errors.Is(err, workers.ErrQuarantineServiceInput) {
		t.Fatal("wrong source accepted", err)
	}
	conf := mqTestConfig(config.ModeWorker)
	persistence, err := NewSupervisedPersistenceFactory(pool, workers.DefaultPersistenceSupervisorOptions())
	if err != nil {
		t.Fatal(err)
	}
	quarantine, err := NewSupervisedQuarantineFactory(pool, workers.DefaultQuarantineSupervisorOptions())
	if err != nil {
		t.Fatal(err)
	}
	_, err = (RuntimeFactories{Persistence: persistence, Quarantine: quarantine}).Check(conf)
	var missing *RuntimeAssemblyError
	if !errors.As(err, &missing) || len(missing.MissingRoles()) != 1 || missing.MissingRoles()[0] != RoleOutbox {
		t.Fatal("Outbox gate bypassed", err)
	}
	if connector.connects.Load() != 0 {
		t.Fatal("factory validation accessed SQL")
	}
}

func TestQuarantineSourceClaimsAndReplacesOnlyOwnConnections(t *testing.T) {
	r, initial := sourceTestResources(t)
	inputs, err := runtimeRoleInputs(r, mustSourcePlan(t))
	if err != nil {
		t.Fatal(err)
	}
	q := inputs[RoleQuarantine]
	if q.QuarantineConsumers == nil || q.PersistenceConsumers != nil || inputs[RolePersistence].QuarantineConsumers != nil {
		t.Fatal("role source leaked")
	}
	if _, err := r.quarantine.claim(inputs[RolePersistence].Consumers); !errors.Is(err, ErrPersistenceConsumers) {
		t.Fatal("audit source claimed persistence", err)
	}
	factory, err := r.quarantine.claim(q.Consumers)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.quarantine.claim(q.Consumers); !errors.Is(err, ErrPersistenceConsumers) {
		t.Fatal("source claimed twice", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	group, err := factory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := group.Close(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if initial[i].calls.Load() != 0 {
			t.Fatal("quarantine retired persistence")
		}
	}
	fresh := newJoinedTestConsumer()
	r.quarantine.dial = func(context.Context, config.ConsumerConfig) (ownedConsumer, error) { return fresh, nil }
	group, err = factory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	consumers := r.Consumers()
	found := false
	for _, c := range consumers {
		if c.Queue == rabbitmq.QuarantineQueue {
			if c.Session != fresh {
				t.Fatal("retired audit session remained visible")
			}
			found = true
		}
	}
	if !found || len(consumers) != 3 || fresh.verifies.Load() != 1 {
		t.Fatal("replacement missing from readiness snapshot")
	}
	if err := group.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestQuarantineSupervisedFactorySingleClaimAndIsolation(t *testing.T) {
	r, _ := sourceTestResources(t)
	inputs, err := runtimeRoleInputs(r, mustSourcePlan(t))
	if err != nil {
		t.Fatal(err)
	}
	connector := &persistenceFactoryConnector{}
	pool := sql.OpenDB(connector)
	defer pool.Close()
	factory, err := NewSupervisedQuarantineFactory(pool, workers.DefaultQuarantineSupervisorOptions())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := factory(ctx, inputs[RolePersistence]); !errors.Is(err, workers.ErrQuarantineServiceInput) {
		t.Fatal("wrong role claimed", err)
	}
	service, err := factory(ctx, inputs[RoleQuarantine])
	if err != nil || connector.connects.Load() != 0 {
		t.Fatal(err)
	}
	if _, err := factory(ctx, inputs[RoleQuarantine]); !errors.Is(err, ErrPersistenceConsumers) {
		t.Fatal("duplicate claim accepted", err)
	}
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if r.persistence.claimed || !r.quarantine.claimed {
		t.Fatal("source ownership crossed role")
	}
	if err := pool.PingContext(ctx); err != nil {
		t.Fatal("role closed shared SQL", err)
	}
}

func TestQuarantineSourceJoinOutlivesObserver(t *testing.T) {
	r, initial := sourceTestResources(t)
	inputs, err := runtimeRoleInputs(r, mustSourcePlan(t))
	if err != nil {
		t.Fatal(err)
	}
	factory, err := r.quarantine.claim(inputs[RoleQuarantine].Consumers)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	group, err := factory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	held := initial[len(initial)-1]
	joined := make(chan struct{})
	held.joined = joined
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(joined) }) }
	defer release()
	done := make(chan error, 1)
	go func() { done <- group.Close(ctx) }()
	short, stop := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer stop()
	if err := r.quarantine.Close(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("observer stood in for actual join", err)
	}
	if _, err := factory(ctx); err == nil {
		t.Fatal("replacement acquired before retirement join")
	}
	select {
	case <-done:
		t.Fatal("group returned before transport joined")
	default:
	}
	// Release before the fixture cleanup registered by sourceTestResources.
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
