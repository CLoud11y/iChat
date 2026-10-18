package application

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"iChat/config"
	"iChat/messaging/rabbitmq"
	"iChat/workers"
	"sync/atomic"
	"testing"
	"time"
)

type persistenceFactoryConnector struct{ connects atomic.Int64 }

func (c *persistenceFactoryConnector) Driver() driver.Driver { return nil }
func (c *persistenceFactoryConnector) Connect(context.Context) (driver.Conn, error) {
	c.connects.Add(1)
	return persistenceFactoryConn{}, nil
}

type persistenceFactoryConn struct{}

func (persistenceFactoryConn) Prepare(string) (driver.Stmt, error) { panic("unexpected SQL") }
func (persistenceFactoryConn) Begin() (driver.Tx, error)           { panic("unexpected transaction") }
func (persistenceFactoryConn) Close() error                        { return nil }
func (persistenceFactoryConn) Ping(context.Context) error          { return nil }

type persistenceFactorySession struct{ done chan struct{} }

func (s *persistenceFactorySession) Done() <-chan struct{} { return s.done }
func (*persistenceFactorySession) OpenConsumer(context.Context, string, int) (*rabbitmq.ConsumerChannel, error) {
	panic("factory must remain idle")
}
func (*persistenceFactorySession) Verify(context.Context) error { panic("factory must not verify") }

func TestPersistenceFactoryIdleOwnershipAndRoleGate(t *testing.T) {
	connector := &persistenceFactoryConnector{}
	pool := sql.OpenDB(connector)
	defer pool.Close()
	factory, err := NewPersistenceFactory(pool, workers.DefaultPersistenceOptions())
	if err != nil || connector.connects.Load() != 0 {
		t.Fatal("factory creation accessed storage", err)
	}
	inputs := RoleDependencies{Consumers: []PreparedConsumer{{Queue: rabbitmq.PersistQueue, Prefetch: 16,
		Session: &persistenceFactorySession{done: make(chan struct{})}}}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	service, err := factory(ctx, inputs)
	if err != nil || connector.connects.Load() != 0 || service.(RuntimeConsumerReadiness).ConsumerReady() {
		t.Fatal("construction activated the role", err)
	}
	inputs.Consumers[0].Session = nil
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pool.PingContext(ctx); err != nil || connector.connects.Load() != 1 {
		t.Fatal("service closed the caller's shared pool", err)
	}
	conf := config.Config{}
	conf.RUNTIME.Mode = config.ModeWorker
	_, err = (RuntimeFactories{Persistence: factory}).Check(conf)
	var missing *RuntimeAssemblyError
	if !errors.As(err, &missing) {
		t.Fatal("missing quarantine/Outbox gate was bypassed", err)
	}
	roles := missing.MissingRoles()
	if len(roles) != 2 || roles[0] != RoleQuarantine || roles[1] != RoleOutbox {
		t.Fatal("wrong remaining roles", roles)
	}
}

func TestPersistenceFactoryRejectsInvalidDependenciesWithoutIO(t *testing.T) {
	connector := &persistenceFactoryConnector{}
	pool := sql.OpenDB(connector)
	defer pool.Close()
	o := workers.DefaultPersistenceOptions()
	if _, err := NewPersistenceFactory(nil, o); err == nil {
		t.Fatal("nil pool accepted")
	}
	bad := o
	bad.CloseTimeout = 0
	if _, err := NewPersistenceFactory(pool, bad); err == nil {
		t.Fatal("invalid options accepted")
	}
	bad = o
	bad.Retry.MaxAttempts = 99
	if _, err := NewPersistenceFactory(pool, bad); err == nil {
		t.Fatal("invalid retry policy accepted")
	}
	factory, err := NewPersistenceFactory(pool, o)
	if err != nil {
		t.Fatal(err)
	}
	input := PreparedConsumer{Queue: rabbitmq.PersistQueue, Prefetch: 1, Session: &persistenceFactorySession{done: make(chan struct{})}}
	for _, inputs := range []RoleDependencies{
		{}, {Consumers: []PreparedConsumer{{Queue: rabbitmq.QuarantineQueue, Prefetch: 1, Session: input.Session}}},
		{Consumers: []PreparedConsumer{{Queue: rabbitmq.PersistQueue, Prefetch: 1, Session: (*persistenceFactorySession)(nil)}}},
		{Consumers: []PreparedConsumer{input, input}},
	} {
		if service, err := factory(context.Background(), inputs); service != nil || err == nil {
			t.Fatal("invalid role input accepted", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if service, err := factory(ctx, RoleDependencies{Consumers: []PreparedConsumer{input}}); service != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled construction accepted", err)
	}
	if connector.connects.Load() != 0 {
		t.Fatal("invalid input reached SQL")
	}
}
