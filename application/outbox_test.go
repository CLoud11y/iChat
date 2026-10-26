package application

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"iChat/config"
	"iChat/database"
	"iChat/health"
	"iChat/messaging/rabbitmq"
	"iChat/workers"
)

func TestOutboxFactoryIdleAndCapabilityIsolation(t *testing.T) {
	connector := &persistenceFactoryConnector{}
	pool := sql.OpenDB(connector)
	defer pool.Close()
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:9", MinIdleConns: 0})
	defer client.Close()
	f, err := NewOutboxFactory(pool, client, workers.DefaultOutboxServiceOptions())
	if err != nil {
		t.Fatal(err)
	}
	s, err := f(context.Background(), RoleDependencies{})
	if err != nil || s.(RuntimeOutboxReadiness).OutboxReady() || connector.connects.Load() != 0 {
		t.Fatal("factory activated dependencies", err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := pool.PingContext(context.Background()); err != nil {
		t.Fatal("borrowed SQL was closed", err)
	}
	if client.PoolStats().TotalConns != 0 {
		t.Fatal("borrowed Redis opened during idle factory")
	}
	for _, input := range []RoleDependencies{{Consumers: []PreparedConsumer{{}}}, {PersistenceConsumers: &PersistenceConsumerSource{}}, {QuarantineConsumers: &QuarantineConsumerSource{}}, {Publisher: &fakeMQPublisher{}}} {
		if s, err := f(context.Background(), input); s != nil || err == nil {
			t.Fatal("foreign capability accepted", err)
		}
	}
	if _, err := f(nil, RoleDependencies{}); err == nil {
		t.Fatal("nil context accepted")
	}
	c, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f(c, RoleDependencies{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := NewOutboxFactory(nil, client, workers.DefaultOutboxServiceOptions()); err == nil {
		t.Fatal("nil SQL accepted")
	}
	if _, err := NewConfiguredOutboxFactory(config.Config{}, workers.DefaultOutboxServiceOptions()); err == nil {
		t.Fatal("invalid config accepted")
	}
	// The full role gate accepts all three concrete factories without starting
	// services or borrowing unrelated MQ capabilities.
	p, _ := NewSupervisedPersistenceFactory(pool, workers.DefaultPersistenceSupervisorOptions())
	q, _ := NewSupervisedQuarantineFactory(pool, workers.DefaultQuarantineSupervisorOptions())
	conf := config.Config{}
	conf.RUNTIME.Mode = config.ModeWorker
	if _, err := (RuntimeFactories{Persistence: p, Quarantine: q, Outbox: f}).Check(conf); err != nil {
		t.Fatal(err)
	}
}

type outboxRuntimeStore struct {
	claim func(context.Context) ([]database.OutboxLease, error)
}

func (s outboxRuntimeStore) Claim(c context.Context, _ database.OutboxClaimOptions) ([]database.OutboxLease, error) {
	return s.claim(c)
}
func (outboxRuntimeStore) Complete(context.Context, database.OutboxLease) (database.OutboxSettlement, error) {
	panic("unexpected completion")
}
func (outboxRuntimeStore) Retry(context.Context, database.OutboxLease, database.OutboxRetryOptions, database.OutboxRetryReason) (database.OutboxSettlement, error) {
	panic("unexpected retry")
}

func TestOutboxRuntimePendingRetainsSourceOwner(t *testing.T) {
	h := newRuntimeHarness()
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	var closes atomic.Int32
	r := workers.OutboxResources{Store: outboxRuntimeStore{claim: func(context.Context) ([]database.OutboxLease, error) { close(entered); <-release; return nil, nil }}, Publisher: &workers.RedisNotificationPublisher{}, ProbeStorage: func(context.Context) error { return nil }, ProbeNotification: func(context.Context) error { return nil }, Close: func() error { closes.Add(1); return nil }}
	factory, _ := NewOutboxSourceFactory(func(context.Context) (workers.OutboxResources, error) { return r, nil }, workers.DefaultOutboxServiceOptions())
	f := h.factories()
	created := make(chan RuntimeService, 1)
	f.Outbox = func(c context.Context, d RoleDependencies) (RuntimeService, error) {
		s, err := factory(c, d)
		created <- s
		return s, err
	}
	o := DefaultRuntimeOptions()
	o.DrainTimeout, o.CloseTimeout = 5*time.Millisecond, 5*time.Millisecond
	c, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- runRuntime(c, mqTestConfig(config.ModeWorker), f, o, h.prepare) }()
	s := <-created
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("outbox did not claim")
	}
	cancel()
	err := runtimeResult(t, result)
	if !errors.Is(err, workers.ErrOutboxServicePending) || closes.Load() != 0 {
		t.Fatal("runtime transferred pending storage owner", err)
	}
	select {
	case <-s.Done():
		t.Fatal("pending service reported Done")
	default:
	}
	close(release)
	select {
	case <-s.Done():
	case <-time.After(time.Second):
		t.Fatal("late callback was not joined")
	}
	if closes.Load() != 1 {
		t.Fatal("source resources not closed once")
	}
}

func TestRuntimeHealthOutboxIsIndependentAndFailsClosed(t *testing.T) {
	for _, missing := range []bool{false, true} {
		h := newReadinessHarness()
		if missing {
			h.missing = RoleOutbox
		}
		cancel, result, observed := startReadinessHarness(t, h, config.ModeAll)
		if missing {
			if status := observed.RuntimeReadiness(); status.Outbox != health.OutboxUnavailable || status.Acceptance != health.Ready || status.Consumers != health.Ready {
				t.Fatal(status)
			}
		} else {
			h.services[RoleOutbox].consumer.Store(false)
			if status := observed.RuntimeReadiness(); status.Outbox != health.OutboxUnavailable || status.Acceptance != health.Ready || status.Consumers != health.Ready {
				t.Fatal(status)
			}
			h.services[RoleOutbox].consumer.Store(true)
			h.publisher.set(rabbitmq.SupervisorStatus{})
			if status := observed.RuntimeReadiness(); status.Outbox != health.Ready || status.Acceptance != health.PublisherUnavailable {
				t.Fatal(status)
			}
		}
		cancel()
		if err := runtimeResult(t, result); err != nil {
			t.Fatal(err)
		}
		if observed.RuntimeReadiness().Outbox != health.Stopping {
			t.Fatal("shutdown retained outbox readiness")
		}
	}
}
