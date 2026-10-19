package application

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"iChat/config"
	"iChat/health"
	"iChat/messaging/rabbitmq"
	"iChat/workers"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These default tests execute the real executor, source and supervisor. Only
// SQL/AMQP edges and the unimplemented companion roles are local fixtures.
type shutdownSQLConnector struct {
	ping   func(context.Context) error
	closes atomic.Int64
}

func (c *shutdownSQLConnector) Driver() driver.Driver { return nil }
func (c *shutdownSQLConnector) Connect(context.Context) (driver.Conn, error) {
	return &shutdownSQLConn{owner: c}, nil
}

type shutdownSQLConn struct {
	persistenceFactoryConn
	owner *shutdownSQLConnector
}

func (c *shutdownSQLConn) Ping(ctx context.Context) error { return c.owner.ping(ctx) }
func (c *shutdownSQLConn) Close() error                   { c.owner.closes.Add(1); return nil }

type shutdownConsumer struct {
	*joinedTestConsumer
	open func(context.Context) error
}

func (s *shutdownConsumer) OpenConsumer(ctx context.Context, _ string, _ int) (*rabbitmq.ConsumerChannel, error) {
	if s.open != nil {
		return nil, s.open(ctx)
	}
	return nil, rabbitmq.ErrConsumerChannelSetup
}
func newShutdownConsumer() *shutdownConsumer {
	return &shutdownConsumer{joinedTestConsumer: newJoinedTestConsumer()}
}

type shutdownCapture struct {
	supervisor *workers.PersistenceSupervisor
	health     health.RuntimeHealth
	mq         *MQResources
	stop       context.CancelFunc
	done       chan struct{}
	err        error
}

func shutdownEventually(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !check() {
		select {
		case <-deadline.C:
			t.Fatal("shutdown condition not observed")
		case <-tick.C:
		}
	}
}

func startShutdownRuntime(t *testing.T, ping func(context.Context) error,
	dial func(context.Context, config.ConsumerConfig) (ownedConsumer, error), graceful bool) (*shutdownCapture, *sql.DB, *shutdownSQLConnector) {
	t.Helper()
	connector := &shutdownSQLConnector{ping: ping}
	pool := sql.OpenDB(connector)
	t.Cleanup(func() { pool.Close() }) // Registered before owner join cleanup below.
	o := workers.DefaultPersistenceSupervisorOptions()
	o.BaseDelay, o.MaxDelay, o.SetupTimeout = 10*time.Millisecond, 20*time.Millisecond, time.Second
	factory, err := NewSupervisedPersistenceFactory(pool, o)
	if err != nil {
		t.Fatal(err)
	}
	h := newRuntimeHarness()
	factories := h.factories()
	r := &shutdownCapture{done: make(chan struct{})}
	captured := make(chan struct{})
	factories.Persistence = func(ctx context.Context, inputs RoleDependencies) (RuntimeService, error) {
		service, err := factory(ctx, inputs)
		if err == nil {
			r.supervisor = service.(*workers.PersistenceSupervisor)
			r.health = inputs.Health
			close(captured)
		}
		return service, err
	}
	options := DefaultRuntimeOptions()
	options.DrainTimeout, options.CloseTimeout = 30*time.Millisecond, 50*time.Millisecond
	if graceful {
		options.DrainTimeout, options.CloseTimeout = time.Second, time.Second
	}
	process, stop := context.WithCancel(context.Background())
	r.stop = stop
	go func() {
		r.err = runRuntime(process, mqTestConfig(config.ModeWorker), factories, options,
			func(ctx context.Context, c config.Config, o MQOptions) (*MQResources, error) {
				resources, err := prepareMQ(ctx, c, o, mqFactories{consumer: dial})
				r.mq = resources
				return resources, err
			})
		close(r.done)
	}()
	t.Cleanup(func() {
		stop()
		sourceTestAwait(t, r.done)
		if r.supervisor != nil {
			sourceTestAwait(t, r.supervisor.Done())
		}
		if r.mq != nil {
			sourceTestAwait(t, r.mq.persistence.closed)
		}
	})
	select {
	case <-captured:
	case <-r.done:
		t.Fatal("shutdown runtime failed to construct", r.err)
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown runtime not constructed")
	}
	shutdownEventually(t, func() bool { return r.health.RuntimeReadiness().Consumers == health.ConsumerUnavailable })
	return r, pool, connector
}

func shutdownPending(t *testing.T, r *shutdownCapture, pool *sql.DB, connector *shutdownSQLConnector) {
	t.Helper()
	r.stop()
	sourceTestAwait(t, r.done)
	if !errors.Is(r.err, ErrRuntimeShutdown) || !errors.Is(r.err, workers.ErrPersistenceSupervisorPending) || !errors.Is(r.err, context.DeadlineExceeded) {
		t.Fatal("executor concealed pending supervisor work", r.err)
	}
	if readinessDone(r.supervisor.Done()) || r.health.RuntimeReadiness().Consumers != health.Stopping || r.supervisor.ConsumerReady() {
		t.Fatal("observer timeout declared ownership released or revived readiness")
	}
	if err := pool.PingContext(context.Background()); err != nil || connector.closes.Load() != 0 {
		t.Fatal("runtime closed externally owned SQL", err)
	}
}

func TestPersistenceShutdownCancelDuringRecoveredSubscriptionSetup(t *testing.T) {
	var healthy atomic.Bool
	entered := make(chan context.Context, 1)
	dial := func(context.Context, config.ConsumerConfig) (ownedConsumer, error) {
		s := newShutdownConsumer()
		s.open = func(ctx context.Context) error { entered <- ctx; <-ctx.Done(); return ctx.Err() }
		return s, nil
	}
	r, pool, _ := startShutdownRuntime(t, func(context.Context) error {
		if !healthy.Load() {
			return io.EOF
		}
		return nil
	}, dial, true)
	healthy.Store(true)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("recovered subscription did not enter setup")
	}
	r.stop()
	sourceTestAwait(t, r.done)
	if r.err != nil || r.supervisor.Err() != nil || !readinessDone(r.supervisor.Done()) || r.supervisor.Status().Generation != 1 {
		t.Fatal("normal cancellation of unadmitted subscription became a runtime failure", r.err, r.supervisor.Err())
	}
	if err := pool.PingContext(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPersistenceShutdownLateProbeRetainsSharedPoolUntilSupervisorDone(t *testing.T) {
	entered, release := make(chan context.Context, 1), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var probes atomic.Int64
	ping := func(ctx context.Context) error {
		switch probes.Add(1) {
		case 1:
			return io.EOF
		case 2:
			entered <- ctx
			<-release
			return nil
		}
		return nil
	}
	r, pool, connector := startShutdownRuntime(t, ping, func(context.Context, config.ConsumerConfig) (ownedConsumer, error) { return newShutdownConsumer(), nil }, false)
	var operation context.Context
	select {
	case operation = <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("probe not entered")
	}
	shutdownPending(t, r, pool, connector)
	if operation.Err() == nil || pool.Stats().InUse == 0 {
		t.Fatal("pending probe did not retain its canceled call and SQL lease")
	}
	once.Do(func() { close(release) })
	sourceTestAwait(t, r.supervisor.Done())
	if r.supervisor.Status().Generation != 0 || r.health.RuntimeReadiness().Consumers != health.Stopping {
		t.Fatal("late probe acquired consumers after shutdown")
	}
	if err := pool.Close(); err != nil || connector.closes.Load() == 0 {
		t.Fatal("SQL owner could not release after supervisor join", err)
	}
}

func TestPersistenceShutdownGroupJoinOutlivesRuntimeObserver(t *testing.T) {
	var healthy atomic.Bool
	var calls atomic.Int64
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	first := newShutdownConsumer()
	first.joined = release
	r, pool, connector := startShutdownRuntime(t, func(context.Context) error {
		if !healthy.Load() {
			return io.EOF
		}
		return nil
	},
		func(context.Context, config.ConsumerConfig) (ownedConsumer, error) {
			if calls.Add(1) == 1 {
				return first, nil
			}
			return newShutdownConsumer(), nil
		}, false)
	healthy.Store(true)
	shutdownEventually(t, func() bool { return first.joins.Load() == 1 })
	shutdownPending(t, r, pool, connector)
	if readinessDone(r.mq.persistence.closed) || calls.Load() != 4 { // Three persistence + one quarantine initially.
		t.Fatal("group join was discarded or replacement overtook it", calls.Load())
	}
	once.Do(func() { close(release) })
	sourceTestAwait(t, r.supervisor.Done())
	sourceTestAwait(t, r.mq.persistence.closed)
	if first.calls.Load() != 1 || r.supervisor.Status().Generation != 1 {
		t.Fatal("late cleanup repeated close or started another generation")
	}
}

func TestPersistenceShutdownLateReplacementAcquisitionCannotRevive(t *testing.T) {
	var healthy atomic.Bool
	var calls atomic.Int64
	entered, release := make(chan context.Context, 1), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	late := newShutdownConsumer()
	dial := func(ctx context.Context, _ config.ConsumerConfig) (ownedConsumer, error) {
		// The plan has three persistence connections plus one quarantine.
		if calls.Add(1) == 6 {
			entered <- ctx
			<-release
			return late, nil
		}
		return newShutdownConsumer(), nil
	}
	r, pool, connector := startShutdownRuntime(t, func(context.Context) error {
		if !healthy.Load() {
			return io.EOF
		}
		return nil
	}, dial, false)
	healthy.Store(true)
	var acquisition context.Context
	select {
	case acquisition = <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("partial replacement did not enter second dial")
	}
	shutdownPending(t, r, pool, connector)
	if acquisition.Err() == nil || late.calls.Load() != 0 || readinessDone(r.mq.persistence.closed) {
		t.Fatal("unreturned factory handle was detached")
	}
	once.Do(func() { close(release) })
	sourceTestAwait(t, r.supervisor.Done())
	sourceTestAwait(t, r.mq.persistence.closed)
	if late.calls.Load() != 1 || late.joins.Load() != 1 || calls.Load() != 6 || r.supervisor.Status().Generation != 2 || r.supervisor.ConsumerReady() {
		t.Fatal("late replacement leaked or revived after runtime returned")
	}
}
