package application

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iChat/config"
	"iChat/health"
	"iChat/workers"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type joinedTestConsumer struct {
	*fakeMQConsumer
	verifies atomic.Int64
	joins    atomic.Int64
	joined   chan struct{}
	verify   func(context.Context) error
}

func newJoinedTestConsumer() *joinedTestConsumer {
	joined := make(chan struct{})
	close(joined)
	return &joinedTestConsumer{fakeMQConsumer: &fakeMQConsumer{&fakeMQResource{done: make(chan struct{})}}, joined: joined}
}
func (c *joinedTestConsumer) Verify(ctx context.Context) error {
	c.verifies.Add(1)
	if c.verify != nil {
		return c.verify(ctx)
	}
	return ctx.Err()
}
func (c *joinedTestConsumer) WaitClosed(ctx context.Context) error {
	c.joins.Add(1)
	select {
	case <-c.joined:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func sourceTestResources(t *testing.T) (*MQResources, []*joinedTestConsumer) {
	t.Helper()
	var consumers []*joinedTestConsumer
	c := mqTestConfig(config.ModeWorker)
	c.RABBITMQ.ConsumerWorkers = 2
	r, err := prepareMQ(context.Background(), c, DefaultMQOptions(), mqFactories{consumer: func(context.Context, config.ConsumerConfig) (ownedConsumer, error) {
		s := newJoinedTestConsumer()
		consumers = append(consumers, s)
		return s, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = r.Close(ctx)
	})
	return r, consumers
}

func sourceTestClaim(t *testing.T, r *MQResources) workers.PersistenceConsumerFactory {
	t.Helper()
	inputs, err := runtimeRoleInputs(r, mustSourcePlan(t))
	if err != nil || inputs[RolePersistence].PersistenceConsumers != r.persistence || inputs[RoleQuarantine].PersistenceConsumers != nil {
		t.Fatal("replaceable capability escaped its role", err)
	}
	factory, err := r.persistence.claim(inputs[RolePersistence].Consumers)
	if err != nil {
		t.Fatal(err)
	}
	return factory
}

func mustSourcePlan(t *testing.T) config.MQRuntimePlan {
	t.Helper()
	c := mqTestConfig(config.ModeWorker)
	c.RABBITMQ.ConsumerWorkers = 2
	plan, err := c.MQRuntime()
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func sourceTestAwait(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("source did not join")
	}
}

func TestPersistenceConsumerSourceReplacesSnapshotAndHealth(t *testing.T) {
	r, initial := sourceTestResources(t)
	factory := sourceTestClaim(t, r)
	if initial[0].verifies.Load() != 0 || len(r.Consumers()) != 1 {
		t.Fatal("claim performed I/O or exposed an inactive generation")
	}
	role := &observedRuntimeService{scriptedRuntimeService: &scriptedRuntimeService{done: make(chan struct{})}}
	role.consumer.Store(true)
	h := &runtimeHealth{ctx: context.Background(), mode: config.ModeWorker}
	h.activate(r, []*runtimeEntry{{role: RolePersistence, service: role, done: role.Done()}, {role: RoleQuarantine, service: role, done: role.Done()}})
	if h.RuntimeReadiness().Consumers == health.Ready {
		t.Fatal("missing active persistence group was ready")
	}
	setup, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	group, err := factory(setup)
	if err != nil || h.RuntimeReadiness().Consumers != health.Ready || initial[0].verifies.Load() != 1 {
		t.Fatal("initial verified group did not become ready", err)
	}
	copy := group.Consumers()
	copy[0] = workers.PersistenceConsumer{}
	if len(group.Consumers()) != 2 || group.Consumers()[0].Session == nil {
		t.Fatal("consumer copy mutated cleanup inputs")
	}
	if err := group.Close(setup); err != nil || h.RuntimeReadiness().Consumers == health.Ready {
		t.Fatal("retired group retained readiness", err)
	}
	var fresh []*joinedTestConsumer
	r.persistence.dial = func(context.Context, config.ConsumerConfig) (ownedConsumer, error) {
		s := newJoinedTestConsumer()
		fresh = append(fresh, s)
		return s, nil
	}
	group, err = factory(setup)
	if err != nil || len(fresh) != 2 || h.RuntimeReadiness().Consumers != health.Ready {
		t.Fatal("fresh generation remained tied to initial sessions", err)
	}
	if !readinessDone(initial[0].Done()) || r.Consumers()[1].Session == initial[0] || initial[2].calls.Load() != 0 {
		t.Fatal("old session revived or quarantine was replaced")
	}
	if err := r.Close(setup); err != nil || h.RuntimeReadiness().Consumers != health.Stopping {
		t.Fatal("MQ cleanup failed or health revived", err)
	}
	for _, s := range append(initial, fresh...) {
		if s.calls.Load() != 1 {
			t.Fatal("session lost or cleanup repeated", s.calls.Load())
		}
	}
}

func TestPersistenceConsumerSourcePartialAcquisitionAndCommonDeadline(t *testing.T) {
	for _, mode := range []string{"error_handle", "nil", "duplicate", "verify", "panic"} {
		t.Run(mode, func(t *testing.T) {
			r, _ := sourceTestResources(t)
			factory := sourceTestClaim(t, r)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			initial, err := factory(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := initial.Close(ctx); err != nil {
				t.Fatal(err)
			}
			cause := errors.New("private connection credentials")
			var acquired []*joinedTestConsumer
			var deadlines []time.Time
			r.persistence.dial = func(ctx context.Context, _ config.ConsumerConfig) (ownedConsumer, error) {
				deadline, _ := ctx.Deadline()
				deadlines = append(deadlines, deadline)
				if mode == "duplicate" && len(acquired) == 1 {
					return acquired[0], nil
				}
				if mode == "nil" && len(acquired) == 1 {
					return (*joinedTestConsumer)(nil), nil
				}
				if mode == "panic" && len(acquired) == 1 {
					panic("private connection credentials")
				}
				s := newJoinedTestConsumer()
				acquired = append(acquired, s)
				s.verify = func(ctx context.Context) error {
					deadline, _ := ctx.Deadline()
					deadlines = append(deadlines, deadline)
					return cause
				}
				if mode == "error_handle" && len(acquired) == 2 {
					return s, cause
				}
				return s, nil
			}
			group, err := factory(ctx)
			if err == nil || group == nil || len(r.Consumers()) != 1 {
				t.Fatal("partial group was lost or declared ready", err)
			}
			if strings.Contains(fmt.Sprintf("%v %#v", err, err), "private connection") {
				t.Fatal("source error exposed credentials")
			}
			for _, deadline := range deadlines {
				if deadline.IsZero() || !deadline.Equal(deadlines[0]) {
					t.Fatal("factory reset setup deadline per connection/verify")
				}
			}
			if err := group.Close(ctx); err != nil {
				t.Fatal(err)
			}
			for _, s := range acquired {
				if s.calls.Load() != 1 || s.joins.Load() != 1 {
					t.Fatal("partial handle not closed and joined exactly once")
				}
			}
		})
	}
}

func TestPersistenceConsumerSourceJoinOutlivesCloseObserver(t *testing.T) {
	r, initial := sourceTestResources(t)
	factory := sourceTestClaim(t, r)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	group, err := factory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	initial[0].joined = release
	var once sync.Once
	defer once.Do(func() { close(release) })
	closed := make(chan error, 1)
	go func() { closed <- group.Close(ctx) }()
	deadline := time.After(time.Second)
	for initial[0].joins.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("group did not join transport")
		case <-time.After(time.Millisecond):
		}
	}
	if _, err := factory(ctx); !errors.Is(err, ErrPersistenceConsumers) {
		t.Fatal("new generation overtook old transport")
	}
	short, stop := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err = r.Close(short)
	stop()
	if !errors.Is(err, ErrMQClose) || !errors.Is(err, context.DeadlineExceeded) || readinessDone(r.persistence.closed) {
		t.Fatal("MQ observer timeout discarded the source cleanup owner", err)
	}
	once.Do(func() { close(release) })
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	sourceTestAwait(t, r.persistence.closed)
	if err := r.Close(context.Background()); err != nil || initial[0].calls.Load() != 1 {
		t.Fatal("repeated MQ close repeated persistence cleanup", err)
	}
}

func TestPersistenceConsumerSourceLateAcquisitionRemainsOwned(t *testing.T) {
	r, _ := sourceTestResources(t)
	factory := sourceTestClaim(t, r)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	g, err := factory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	g.Close(ctx)
	entered, release := make(chan struct{}), make(chan struct{})
	late := newJoinedTestConsumer()
	var once sync.Once
	defer once.Do(func() { close(release) })
	r.persistence.dial = func(context.Context, config.ConsumerConfig) (ownedConsumer, error) {
		close(entered)
		<-release
		return late, nil
	}
	returned := make(chan struct{})
	go func() { _, _ = factory(ctx); close(returned) }()
	sourceTestAwait(t, entered)
	short, stop := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err = r.Close(short)
	stop()
	if !errors.Is(err, context.DeadlineExceeded) || late.calls.Load() != 0 || readinessDone(r.persistence.closed) {
		t.Fatal("unreturned factory handle was discarded", err)
	}
	once.Do(func() { close(release) })
	sourceTestAwait(t, returned)
	sourceTestAwait(t, r.persistence.closed)
	if late.calls.Load() != 1 || late.joins.Load() != 1 || len(r.Consumers()) != 0 {
		t.Fatal("late handle leaked or became ready")
	}
}

func TestSupervisedPersistenceFactoryIdleClaimAndGate(t *testing.T) {
	r, initial := sourceTestResources(t)
	connector := &persistenceFactoryConnector{}
	pool := sql.OpenDB(connector)
	defer pool.Close()
	factory, err := NewSupervisedPersistenceFactory(pool, workers.DefaultPersistenceSupervisorOptions())
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := runtimeRoleInputs(r, mustSourcePlan(t))
	if err != nil {
		t.Fatal(err)
	}
	service, err := factory(context.Background(), inputs[RolePersistence])
	if err != nil || connector.connects.Load() != 0 || initial[0].verifies.Load() != 0 || service.(RuntimeConsumerReadiness).ConsumerReady() {
		t.Fatal("idle supervised factory performed I/O or was ready", err)
	}
	if duplicate, err := factory(context.Background(), inputs[RolePersistence]); duplicate != nil || !errors.Is(err, ErrPersistenceConsumers) {
		t.Fatal("two supervisors claimed the same connection source", err)
	}
	if err := service.Close(context.Background()); err != nil || initial[0].calls.Load() != 0 {
		t.Fatal("idle supervisor stole MQ group cleanup", err)
	}
	if _, err := (RuntimeFactories{Persistence: factory}).Check(mqTestConfig(config.ModeWorker)); !errors.Is(err, ErrRuntimeNotAssembled) {
		t.Fatal("supervision bypassed quarantine/Outbox gate", err)
	}
	if err := pool.PingContext(context.Background()); err != nil {
		t.Fatal("supervisor closed shared pool", err)
	}
	for _, input := range []RoleDependencies{{}, {PersistenceConsumers: &PersistenceConsumerSource{}}, {Publisher: &fakeMQPublisher{}}} {
		if service, err := factory(context.Background(), input); service != nil || err == nil {
			t.Fatal("invalid supervised dependencies accepted", err)
		}
	}
}

func TestPersistenceConsumerSourceCleanupFailurePreventsReplacementAndRemainsVisible(t *testing.T) {
	r, initial := sourceTestResources(t)
	factory := sourceTestClaim(t, r)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	g, err := factory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cause := errors.New("private cleanup credentials")
	initial[0].close = func(context.Context) error { return cause }
	if err := g.Close(ctx); !errors.Is(err, cause) || initial[0].joins.Load() != 1 || strings.Contains(fmt.Sprintf("%v %#v", err, err), "private cleanup") {
		t.Fatal("cleanup failure skipped transport join or lost cause", err)
	}
	if replacement, err := factory(ctx); replacement != nil || !errors.Is(err, ErrPersistenceConsumers) {
		t.Fatal("cleanup failure authorized another group", err)
	}
	if err := r.Close(ctx); !errors.Is(err, cause) || !errors.Is(err, ErrMQClose) || strings.Contains(err.Error(), "private cleanup") {
		t.Fatal("resource owner hid or exposed previous cleanup failure", err)
	}
	if err := r.persistence.Close(ctx); !errors.Is(err, cause) || strings.Contains(fmt.Sprintf("%v %#v", err, err), "private cleanup") {
		t.Fatal("source observer exposed its raw child error", err)
	}
}
