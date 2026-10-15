package application

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"iChat/config"
	"iChat/messaging/rabbitmq"
)

type runtimeTrace struct {
	mu     sync.Mutex
	events []string
}

func (l *runtimeTrace) add(event string) {
	l.mu.Lock()
	l.events = append(l.events, event)
	l.mu.Unlock()
}
func (l *runtimeTrace) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

type scriptedRuntimeService struct {
	role     RuntimeRole
	trace    *runtimeTrace
	done     chan struct{}
	once     sync.Once
	setup    context.Context
	lifetime context.Context
	started  chan<- *scriptedRuntimeService
	start    func(context.Context, context.Context) error
	stop     func()
	drain    func(context.Context) error
	close    func(context.Context) error
	err      error // Publish changes by closing done before the executor reads it.
	nilDone  bool
	starts   atomic.Int32
	stops    atomic.Int32
	drains   atomic.Int32
	closes   atomic.Int32
	closed   atomic.Bool
}

func (s *scriptedRuntimeService) Start(setup, lifetime context.Context) error {
	s.setup, s.lifetime = setup, lifetime
	s.starts.Add(1)
	s.trace.add("start:" + string(s.role))
	if s.start != nil {
		if err := s.start(setup, lifetime); err != nil {
			return err
		}
	}
	if s.started != nil {
		s.started <- s
	}
	return nil
}
func (s *scriptedRuntimeService) StopAdmission() {
	s.stops.Add(1)
	s.trace.add("stop:" + string(s.role))
	if s.stop != nil {
		s.stop()
	}
}
func (s *scriptedRuntimeService) Drain(ctx context.Context) error {
	s.drains.Add(1)
	s.trace.add("drain:" + string(s.role))
	if s.drain != nil {
		return s.drain(ctx)
	}
	return nil
}
func (s *scriptedRuntimeService) Close(ctx context.Context) error {
	defer s.closed.Store(true)
	s.closes.Add(1)
	s.trace.add("close:" + string(s.role))
	s.once.Do(func() { close(s.done) })
	if s.close != nil {
		return s.close(ctx)
	}
	return nil
}
func (s *scriptedRuntimeService) Done() <-chan struct{} {
	if s.nilDone {
		return nil
	}
	return s.done
}
func (s *scriptedRuntimeService) Err() error {
	select {
	case <-s.done:
		return s.err
	default:
		panic("executor read Err before Done")
	}
}

type runtimeHarness struct {
	trace    runtimeTrace
	services map[RuntimeRole]*scriptedRuntimeService
	inputs   map[RuntimeRole]RoleDependencies
	handles  []*fakeMQResource
	mqLife   context.Context
	mq       *MQResources
	started  chan *scriptedRuntimeService
}

func newRuntimeHarness() *runtimeHarness {
	h := &runtimeHarness{services: map[RuntimeRole]*scriptedRuntimeService{}, inputs: map[RuntimeRole]RoleDependencies{}, started: make(chan *scriptedRuntimeService, 4)}
	for _, role := range runtimeExpectedRoles(config.ModeAll) {
		h.services[role] = &scriptedRuntimeService{role: role, trace: &h.trace, done: make(chan struct{}, 1), started: h.started}
	}
	return h
}

func (h *runtimeHarness) factories() RuntimeFactories {
	factory := func(role RuntimeRole) RuntimeFactory {
		return func(_ context.Context, inputs RoleDependencies) (RuntimeService, error) {
			h.trace.add("build:" + string(role))
			h.inputs[role] = inputs
			return h.services[role], nil
		}
	}
	return RuntimeFactories{API: factory(RoleAPI), Persistence: factory(RolePersistence), Quarantine: factory(RoleQuarantine), Outbox: factory(RoleOutbox)}
}

func (h *runtimeHarness) prepare(ctx context.Context, c config.Config, o MQOptions) (*MQResources, error) {
	h.trace.add("prepare:mq")
	r, err := prepareMQ(ctx, c, o, fakeMQFactories(&h.handles, &h.mqLife))
	h.mq = r
	for _, handle := range h.handles {
		handle.close = func(ctx context.Context) error {
			h.trace.add("close:mq")
			for _, role := range runtimeExpectedRoles(c.RUNTIME.Mode) {
				if h.services[role].closes.Load() != 0 && !h.services[role].closed.Load() {
					return errors.New("MQ closed before service Close returned")
				}
			}
			return ctx.Err()
		}
	}
	return r, err
}

func startHarness(t *testing.T, h *runtimeHarness, mode config.RuntimeMode, o RuntimeOptions) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- runRuntime(ctx, mqTestConfig(mode), h.factories(), o, h.prepare) }()
	t.Cleanup(func() { cancel() })
	var last *scriptedRuntimeService
	for range runtimeExpectedRoles(mode) {
		select {
		case last = <-h.started:
		case err := <-result:
			t.Fatal("runtime failed during activation", err)
		case <-time.After(2 * time.Second):
			t.Fatal("activation did not finish")
		}
	}
	// The executor cancels setup only AFTER all starts and handoff checks. This
	// barrier prevents normal-stop tests from racing with startup cancellation.
	select {
	case <-last.setup.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("startup handoff did not complete")
	}
	return cancel, result
}

func runtimeResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("runtime did not join its shutdown")
		return nil
	}
}

func TestRunRuntimeRolesActivationAndGracefulShutdown(t *testing.T) {
	for _, mode := range []config.RuntimeMode{config.ModeAPI, config.ModeWorker, config.ModeAll} {
		t.Run(string(mode), func(t *testing.T) {
			h := newRuntimeHarness()
			roles := runtimeExpectedRoles(mode)
			for _, role := range roles {
				s := h.services[role]
				s.drain = func(ctx context.Context) error {
					for _, expected := range roles {
						if h.services[expected].stops.Load() != 1 {
							return errors.New("drain began before all admissions stopped")
						}
					}
					if ctx.Err() != nil || s.lifetime.Err() != nil {
						return errors.New("drain inherited cancelled process/lifetime")
					}
					for _, handle := range h.handles {
						if handle.calls.Load() != 0 {
							return errors.New("MQ closed before commit/ACK opportunity")
						}
					}
					h.trace.add("commit-and-ack:" + string(role)) // Transport ownership ordering only.
					return nil
				}
				s.close = func(context.Context) error {
					if s.lifetime.Err() == nil {
						return errors.New("forced close did not cancel service lifetime")
					}
					for _, expected := range roles {
						if h.services[expected].drains.Load() != 1 {
							return errors.New("service closed before every drain ran")
						}
					}
					return nil
				}
			}
			cancel, result := startHarness(t, h, mode, DefaultRuntimeOptions())
			for _, role := range roles {
				inputs := h.inputs[role]
				wantConsumers := 0
				queue := ""
				if role == RolePersistence {
					wantConsumers, queue = 3, rabbitmq.PersistQueue
				} else if role == RoleQuarantine {
					wantConsumers, queue = 1, rabbitmq.QuarantineQueue
				}
				if (inputs.Publisher != nil) != (role == RoleAPI) || len(inputs.Consumers) != wantConsumers {
					t.Fatal("role received another role's MQ capability", role)
				}
				for _, consumer := range inputs.Consumers {
					if consumer.Queue != queue || consumer.Prefetch != 7 {
						t.Fatal("consumer got wrong queue/prefetch")
					}
				}
				if h.services[role].lifetime.Err() != nil {
					t.Fatal("setup cancellation ended successful service lifetime")
				}
			}
			cancel()
			if err := runtimeResult(t, result); err != nil {
				t.Fatal(err)
			}
			assertMQClosed(t, h.handles, 1)
			for _, role := range roles {
				s := h.services[role]
				if s.starts.Load() != 1 || s.stops.Load() != 1 || s.drains.Load() != 1 || s.closes.Load() != 1 {
					t.Fatal("service lifecycle did not finish exactly once", role)
				}
			}
			events := h.trace.snapshot()
			var starts []string
			lastBuild, firstStart := -1, len(events)
			for i, event := range events {
				if strings.HasPrefix(event, "build:") {
					lastBuild = i
				}
				if strings.HasPrefix(event, "start:") {
					if firstStart == len(events) {
						firstStart = i
					}
					starts = append(starts, event)
				}
			}
			var wantStarts []string
			for _, role := range []RuntimeRole{RoleOutbox, RoleQuarantine, RolePersistence, RoleAPI} {
				if h.services[role].starts.Load() != 0 {
					wantStarts = append(wantStarts, "start:"+string(role))
				}
			}
			if lastBuild >= firstStart || !reflect.DeepEqual(starts, wantStarts) {
				t.Fatal("construction/activation ordering changed", events)
			}
		})
	}
}

func TestRunRuntimePreflightRejectsBeforeMQ(t *testing.T) {
	for _, name := range []string{"nil_context", "cancelled", "legacy", "missing_role", "conflict", "bad_mq", "bad_options", "missing_preparer"} {
		t.Run(name, func(t *testing.T) {
			ctx, c, o, f := context.Background(), mqTestConfig(config.ModeAll), DefaultRuntimeOptions(), allRuntimeFactories()
			calls := 0
			prepare := mqPreparer(func(context.Context, config.Config, MQOptions) (*MQResources, error) { calls++; return nil, nil })
			switch name {
			case "nil_context":
				ctx = nil
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "legacy":
				c.RUNTIME.Mode = config.ModeLegacy
			case "missing_role":
				f.Outbox = nil
			case "conflict":
				c.LEGACY_WORKER.Enabled = true
			case "bad_mq":
				c.RABBITMQ.ConsumerWorkers = 0
			case "bad_options":
				o.DrainTimeout = 0
			case "missing_preparer":
				prepare = nil
			}
			if err := runRuntime(ctx, c, f, o, prepare); err == nil || calls != 0 {
				t.Fatal("preflight invoked dependency factory", err)
			}
		})
	}
}

func TestRunRuntimeConstructionFailuresRollbackWithoutActivation(t *testing.T) {
	secret := errors.New("amqp://user:SECRET_FACTORY@host/private-message")
	for _, role := range runtimeExpectedRoles(config.ModeAll) {
		t.Run(string(role), func(t *testing.T) {
			h := newRuntimeHarness()
			f := h.factories()
			fail := RuntimeFactory(func(context.Context, RoleDependencies) (RuntimeService, error) { return h.services[role], secret })
			switch role {
			case RoleAPI:
				f.API = fail
			case RolePersistence:
				f.Persistence = fail
			case RoleQuarantine:
				f.Quarantine = fail
			case RoleOutbox:
				f.Outbox = fail
			}
			err := runRuntime(context.Background(), mqTestConfig(config.ModeAll), f, DefaultRuntimeOptions(), h.prepare)
			if !errors.Is(err, ErrRuntimeStart) || !errors.Is(err, secret) || h.services[role].closes.Load() != 1 {
				t.Fatal("partial construction handle was lost", err)
			}
			for _, s := range h.services {
				if s.starts.Load() != 0 || s.stops.Load() != 0 || s.drains.Load() != 0 {
					t.Fatal("construction failure activated/drained an idle service")
				}
			}
			assertMQClosed(t, h.handles, 1)
			for _, format := range []string{"%v", "%+v", "%#v"} {
				if strings.Contains(fmt.Sprintf(format, err), "SECRET_FACTORY") {
					t.Fatal("raw constructor error leaked")
				}
			}
		})
	}
}

func TestRunRuntimeNilAndAliasedServices(t *testing.T) {
	for _, name := range []string{"nil", "typed_nil", "shared"} {
		h := newRuntimeHarness()
		f := h.factories()
		f.Persistence = func(context.Context, RoleDependencies) (RuntimeService, error) {
			switch name {
			case "typed_nil":
				return (*scriptedRuntimeService)(nil), nil
			case "shared":
				return h.services[RoleAPI], nil
			default:
				return nil, nil
			}
		}
		err := runRuntime(context.Background(), mqTestConfig(config.ModeAll), f, DefaultRuntimeOptions(), h.prepare)
		if !errors.Is(err, ErrRuntimeServices) || h.services[RoleAPI].closes.Load() != 1 {
			t.Fatal("invalid/aliased service escaped or closed twice", name, err)
		}
		assertMQClosed(t, h.handles, 1)
	}
}

func TestRunRuntimeActivationFailuresDrainEveryAttemptAndCloseEveryBuiltService(t *testing.T) {
	for _, failed := range []RuntimeRole{RoleOutbox, RoleQuarantine, RolePersistence, RoleAPI} {
		t.Run(string(failed), func(t *testing.T) {
			h := newRuntimeHarness()
			failure := errors.New("controlled activation failure")
			h.services[failed].start = func(context.Context, context.Context) error { return failure }
			err := runRuntime(context.Background(), mqTestConfig(config.ModeAll), h.factories(), DefaultRuntimeOptions(), h.prepare)
			if !errors.Is(err, ErrRuntimeStart) || !errors.Is(err, failure) {
				t.Fatal("activation failure was lost", err)
			}
			for _, s := range h.services {
				if s.closes.Load() != 1 || s.stops.Load() != s.starts.Load() || s.drains.Load() != s.starts.Load() {
					t.Fatal("partially activated/idle service was not cleaned correctly", s.role)
				}
			}
			assertMQClosed(t, h.handles, 1)
		})
	}
}

func TestRunRuntimeUnexpectedServiceTermination(t *testing.T) {
	for _, name := range []string{"clean_exit", "error_exit", "early_exit", "nil_done", "done_value"} {
		t.Run(name, func(t *testing.T) {
			h := newRuntimeHarness()
			s := h.services[RoleOutbox]
			failure := errors.New("controlled runtime failure")
			if name == "early_exit" {
				s.start = func(context.Context, context.Context) error { s.once.Do(func() { close(s.done) }); return nil }
			} else if name == "nil_done" {
				s.nilDone = true
			}
			var err error
			if name == "early_exit" || name == "nil_done" {
				err = runRuntime(context.Background(), mqTestConfig(config.ModeAll), h.factories(), DefaultRuntimeOptions(), h.prepare)
				if h.services[RoleAPI].starts.Load() != 0 {
					t.Fatal("API activated after worker failed during startup")
				}
			} else {
				_, result := startHarness(t, h, config.ModeAll, DefaultRuntimeOptions())
				if name == "done_value" {
					s.done <- struct{}{}
				} else {
					if name == "error_exit" {
						s.err = failure
					}
					s.once.Do(func() { close(s.done) })
				}
				err = runtimeResult(t, result)
			}
			if name == "nil_done" {
				if !errors.Is(err, ErrRuntimeServices) {
					t.Fatal("nil Done accepted", err)
				}
			} else if !errors.Is(err, ErrRuntimeStopped) {
				t.Fatal("unexpected role termination was treated as normal", err)
			}
			if name == "error_exit" && !errors.Is(err, failure) {
				t.Fatal("role exit cause was lost", err)
			}
			assertMQClosed(t, h.handles, 1)
		})
	}
}

func TestRunRuntimeStartupDeadlineCoversAllFactoriesAndStart(t *testing.T) {
	for _, name := range []string{"factories", "activation"} {
		t.Run(name, func(t *testing.T) {
			h := newRuntimeHarness()
			f := h.factories()
			var deadlines []time.Time
			if name == "factories" {
				wrap := func(base RuntimeFactory) RuntimeFactory {
					return func(ctx context.Context, d RoleDependencies) (RuntimeService, error) {
						deadline, _ := ctx.Deadline()
						deadlines = append(deadlines, deadline)
						select {
						case <-time.After(20 * time.Millisecond):
							return base(ctx, d)
						case <-ctx.Done():
							return nil, ctx.Err()
						}
					}
				}
				f.API, f.Persistence, f.Quarantine, f.Outbox = wrap(f.API), wrap(f.Persistence), wrap(f.Quarantine), wrap(f.Outbox)
			} else {
				h.services[RoleQuarantine].start = func(ctx context.Context, _ context.Context) error { <-ctx.Done(); return ctx.Err() }
			}
			o := DefaultRuntimeOptions()
			o.StartupTimeout = 55 * time.Millisecond
			start := time.Now()
			err := runRuntime(context.Background(), mqTestConfig(config.ModeAll), f, o, h.prepare)
			if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
				t.Fatal("startup did not bound the whole sequence", err)
			}
			for _, d := range deadlines {
				if !d.Equal(deadlines[0]) {
					t.Fatal("factory setup deadline was reset")
				}
			}
			assertMQClosed(t, h.handles, 1)
		})
	}
}

func TestRunRuntimeDrainAndCloseUseGroupBudgetsAndStillReachMQ(t *testing.T) {
	for _, stage := range []string{"drain", "close"} {
		t.Run(stage, func(t *testing.T) {
			h := newRuntimeHarness()
			joined := make(chan struct{}, 4)
			for _, s := range h.services {
				wait := func(ctx context.Context) error { <-ctx.Done(); joined <- struct{}{}; return ctx.Err() }
				if stage == "drain" {
					s.drain = wait
				} else {
					s.close = wait
				}
			}
			o := DefaultRuntimeOptions()
			o.DrainTimeout, o.CloseTimeout = 50*time.Millisecond, 50*time.Millisecond
			cancel, result := startHarness(t, h, config.ModeAll, o)
			start := time.Now()
			cancel()
			err := runtimeResult(t, result)
			if !errors.Is(err, ErrRuntimeShutdown) || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second || len(joined) != 4 {
				t.Fatal("shutdown multiplied budgets or abandoned a call", err)
			}
			assertMQClosed(t, h.handles, 1)
		})
	}
}

func TestRunRuntimeDependencyCorruptionAndPartialMQFailure(t *testing.T) {
	for _, name := range []string{"partial_error", "nil_mq", "wrong_mode", "missing_publisher", "wrong_queue", "wrong_prefetch", "missing_consumer", "typed_nil_consumer"} {
		t.Run(name, func(t *testing.T) {
			h := newRuntimeHarness()
			prepare := func(ctx context.Context, c config.Config, o MQOptions) (*MQResources, error) {
				if name == "nil_mq" {
					return nil, nil
				}
				r, err := h.prepare(ctx, c, o)
				if err != nil {
					return r, err
				}
				switch name {
				case "partial_error":
					return r, errors.New("partial MQ factory result")
				case "wrong_mode":
					r.mode = config.ModeWorker
				case "missing_publisher":
					r.publisher = nil
				case "wrong_queue":
					r.consumers[0].Queue = "other.queue"
				case "wrong_prefetch":
					r.consumers[0].Prefetch++
				case "missing_consumer":
					r.consumers = r.consumers[:1]
				case "typed_nil_consumer":
					r.consumers[0].Session = (*fakeMQConsumer)(nil)
				}
				return r, nil
			}
			err := runRuntime(context.Background(), mqTestConfig(config.ModeAll), h.factories(), DefaultRuntimeOptions(), prepare)
			if !errors.Is(err, ErrRuntimeStart) {
				t.Fatal("invalid dependency snapshot reached a service factory", err)
			}
			for _, s := range h.services {
				if s.closes.Load() != 0 || s.starts.Load() != 0 {
					t.Fatal("bad MQ dependencies reached a service")
				}
			}
			assertMQClosed(t, h.handles, 1)
		})
	}
}

func TestRunRuntimeCancellationAtSuccessfulFactoryOrStartHandoff(t *testing.T) {
	for _, stage := range []string{"factory", "start"} {
		t.Run(stage, func(t *testing.T) {
			h := newRuntimeHarness()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := h.factories()
			if stage == "factory" {
				base := f.Outbox
				f.Outbox = func(ctx context.Context, d RoleDependencies) (RuntimeService, error) {
					s, err := base(ctx, d)
					cancel()
					return s, err
				}
			} else {
				h.services[RoleOutbox].start = func(context.Context, context.Context) error { cancel(); return nil }
			}
			err := runRuntime(ctx, mqTestConfig(config.ModeAll), f, DefaultRuntimeOptions(), h.prepare)
			if !errors.Is(err, ErrRuntimeStart) || !errors.Is(err, context.Canceled) {
				t.Fatal("cancelled startup transferred a running assembly", err)
			}
			for _, s := range h.services {
				if s.closes.Load() != 1 || (stage == "factory" && s.starts.Load() != 0) {
					t.Fatal("successful partial handle was not rolled back", s.role)
				}
			}
			assertMQClosed(t, h.handles, 1)
		})
	}
}

func TestRunRuntimeRetainsStartupDrainServiceAndMQCloseErrors(t *testing.T) {
	h := newRuntimeHarness()
	startErr, drainErr := errors.New("SECRET_START"), errors.New("SECRET_DRAIN")
	closeErr, mqErr := errors.New("SECRET_CLOSE"), errors.New("SECRET_MQ")
	h.services[RoleAPI].start = func(context.Context, context.Context) error { return startErr }
	h.services[RolePersistence].drain = func(context.Context) error { return drainErr }
	h.services[RoleOutbox].close = func(context.Context) error { return closeErr }
	prepare := func(ctx context.Context, c config.Config, o MQOptions) (*MQResources, error) {
		r, err := h.prepare(ctx, c, o)
		if len(h.handles) != 0 {
			h.handles[0].close = func(context.Context) error { return mqErr }
		}
		return r, err
	}
	err := runRuntime(context.Background(), mqTestConfig(config.ModeAll), h.factories(), DefaultRuntimeOptions(), prepare)
	for _, want := range []error{ErrRuntimeStart, ErrRuntimeShutdown, ErrMQClose, startErr, drainErr, closeErr, mqErr} {
		if !errors.Is(err, want) {
			t.Fatal("runtime lost an independent failure category", want, err)
		}
	}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		if strings.Contains(fmt.Sprintf(format, err), "SECRET_") {
			t.Fatal("runtime shutdown formatted raw dependency errors")
		}
	}
	assertMQClosed(t, h.handles, 1)
}
