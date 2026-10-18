package workers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iChat/database"
	"iChat/messaging/rabbitmq"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type serviceNext struct {
	delivery *rabbitmq.ConsumerDelivery
	err      error
}

type serviceStream struct {
	done       chan struct{}
	next       chan serviceNext
	nextCalls  atomic.Int64
	closeCalls atomic.Int64
	closeHook  func(context.Context) error
	nextHook   func(context.Context) (*rabbitmq.ConsumerDelivery, error)
	once       sync.Once
}

func newServiceStream() *serviceStream {
	return &serviceStream{done: make(chan struct{}), next: make(chan serviceNext, 8)}
}
func (s *serviceStream) Done() <-chan struct{} { return s.done }
func (s *serviceStream) Next(ctx context.Context) (*rabbitmq.ConsumerDelivery, error) {
	s.nextCalls.Add(1)
	if s.nextHook != nil {
		return s.nextHook(ctx)
	}
	select {
	case got := <-s.next:
		return got.delivery, got.err
	case <-s.done:
		return nil, rabbitmq.ErrConsumerUnavailable
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (s *serviceStream) Close(ctx context.Context) error {
	s.closeCalls.Add(1)
	if s.closeHook != nil {
		defer s.once.Do(func() { close(s.done) })
		return s.closeHook(ctx)
	}
	s.once.Do(func() { close(s.done) })
	return nil
}

func serviceTestOptions() PersistenceOptions {
	o := DefaultPersistenceOptions()
	o.PollTimeout, o.ProbeInterval, o.ProbeTimeout = 20*time.Millisecond, time.Second, 100*time.Millisecond
	return o
}

func serviceForTest(t *testing.T, streams ...*serviceStream) *PersistenceService {
	t.Helper()
	inputs := make([]persistenceSubscription, len(streams))
	for i, stream := range streams {
		inputs[i] = persistenceSubscription{done: stream.done, open: func(context.Context) (persistenceStream, error) { return stream, nil }}
	}
	s := newPersistenceService(inputs, serviceTestOptions(), func(context.Context) error { return nil },
		func(context.Context, *rabbitmq.ConsumerDelivery) error { return nil })
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.Close(ctx); errors.Is(err, ErrPersistenceServicePending) {
			t.Error("service cleanup still pending", err)
		}
	})
	return s
}

func serviceSetup(t *testing.T, s *PersistenceService) context.CancelFunc {
	t.Helper()
	setup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	if err := s.Start(setup, context.Background()); err != nil {
		cancel()
		t.Fatal(err)
	}
	return cancel
}

func serviceAwait(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("service did not reach expected state")
	}
}

func serviceEventually(t *testing.T, predicate func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !predicate() {
		select {
		case <-ctx.Done():
			t.Fatal("expected state not observed")
		case <-tick.C:
		}
	}
}

func TestPersistenceServiceGracefulDrainRetainsWorkAndSubscriptions(t *testing.T) {
	stream := newServiceStream()
	s := serviceForTest(t, stream)
	entered, release, completed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var handled atomic.Int64
	s.handle = func(ctx context.Context, d *rabbitmq.ConsumerDelivery) error {
		if handled.Add(1) != 1 {
			t.Error("worker admitted another delivery after StopAdmission")
			return nil
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("unbounded processing context")
		}
		close(entered)
		<-release
		defer close(completed)
		return ctx.Err()
	}
	cancelSetup := serviceSetup(t, s)
	cancelSetup()
	if !s.ConsumerReady() {
		t.Fatal("successful setup cancellation ended lifetime")
	}
	stream.next <- serviceNext{delivery: &rabbitmq.ConsumerDelivery{}}
	serviceAwait(t, entered)
	stream.next <- serviceNext{delivery: &rabbitmq.ConsumerDelivery{}}
	s.StopAdmission()
	if s.ConsumerReady() || stream.closeCalls.Load() != 0 {
		t.Fatal("stop did not revoke readiness or closed an active subscription")
	}
	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err := s.Drain(short)
	cancel()
	if !errors.Is(err, ErrPersistenceServicePending) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("drain observer did not report pending work", err)
	}
	select {
	case <-s.Done():
		t.Fatal("drain timeout was mistaken for service completion")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	serviceAwait(t, completed)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Drain(ctx); err != nil || handled.Load() != 1 || stream.closeCalls.Load() != 0 {
		t.Fatal("graceful work was cancelled or stream closed before Close", err)
	}
	if err := s.Close(ctx); err != nil || stream.closeCalls.Load() != 1 {
		t.Fatal("subscription not closed exactly once after drain", err)
	}
	if err := s.Close(ctx); err != nil || stream.closeCalls.Load() != 1 {
		t.Fatal("repeated Close repeated cleanup", err)
	}
	if err := s.Start(ctx, context.Background()); !errors.Is(err, ErrPersistenceServiceStart) {
		t.Fatal("closed service restarted", err)
	}
}

func TestPersistenceServiceForcedCloseWaitsForActualJoin(t *testing.T) {
	stream := newServiceStream()
	s := serviceForTest(t, stream)
	entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	s.handle = func(ctx context.Context, _ *rabbitmq.ConsumerDelivery) error {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		<-release // Simulate a dependency that has not returned after cancellation.
		return ctx.Err()
	}
	defer serviceSetup(t, s)()
	stream.next <- serviceNext{delivery: &rabbitmq.ConsumerDelivery{}}
	serviceAwait(t, entered)
	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err := s.Close(short)
	cancel()
	serviceAwait(t, cancelled)
	if !errors.Is(err, ErrPersistenceServicePending) || stream.closeCalls.Load() != 0 || persistenceDone(s.Done()) {
		t.Fatal("cancelled work was treated as joined", err)
	}
	once.Do(func() { close(release) })
	serviceAwait(t, s.Done())
	if !errors.Is(s.Err(), context.Canceled) || stream.closeCalls.Load() != 1 {
		t.Fatal("join did not retain processing error and close once", s.Err())
	}
}

func TestPersistenceServiceFailureStopsGroupWithoutReopening(t *testing.T) {
	for _, kind := range []PersistenceFailure{InvalidDelivery, PersistenceFailed, CommitUnknown, AcknowledgementFailed} {
		t.Run(string(kind), func(t *testing.T) {
			a, b := newServiceStream(), newServiceStream()
			s := serviceForTest(t, a, b)
			entered, release, failed := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			first := &rabbitmq.ConsumerDelivery{}
			s.handle = func(ctx context.Context, d *rabbitmq.ConsumerDelivery) error {
				if d == first {
					close(entered)
					<-release
					return ctx.Err()
				}
				close(failed)
				return &PersistenceError{Kind: kind, cause: database.ErrCommandCommitUnknown}
			}
			defer serviceSetup(t, s)()
			a.next <- serviceNext{delivery: first}
			serviceAwait(t, entered)
			b.next <- serviceNext{delivery: &rabbitmq.ConsumerDelivery{}}
			serviceAwait(t, failed)
			serviceEventually(t, func() bool { return !s.ConsumerReady() })
			if a.closeCalls.Load() != 0 || b.closeCalls.Load() != 0 || persistenceDone(s.Done()) {
				t.Fatal("sibling failure closed subscriptions before admitted work joined")
			}
			once.Do(func() { close(release) })
			serviceAwait(t, s.Done())
			if a.closeCalls.Load() != 1 || b.closeCalls.Load() != 1 || !errors.Is(s.Err(), database.ErrCommandCommitUnknown) {
				t.Fatal("fail-stop group lost its cause or cleanup")
			}
			if err := s.Start(context.Background(), context.Background()); err == nil {
				t.Fatal("failed group silently reopened")
			}
		})
	}
}

func TestPersistenceServicePartialStartupAndConcurrentClose(t *testing.T) {
	for _, variant := range []string{"error", "handle_and_error", "nil", "typed_nil", "panic", "second_probe"} {
		t.Run(variant, func(t *testing.T) {
			a, b := newServiceStream(), newServiceStream()
			s := serviceForTest(t, a, b)
			original := errors.New("private SQL/password diagnostic")
			if variant == "second_probe" {
				var probes atomic.Int64
				s.probe = func(context.Context) error {
					if probes.Add(1) == 2 {
						return original
					}
					return nil
				}
			} else {
				s.inputs[1].open = func(context.Context) (persistenceStream, error) {
					switch variant {
					case "handle_and_error":
						return b, original
					case "nil":
						return nil, nil
					case "typed_nil":
						return (*serviceStream)(nil), nil
					case "panic":
						panic("private payload")
					}
					return nil, original
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := s.Start(ctx, context.Background())
			if !errors.Is(err, ErrPersistenceServiceStart) || s.ConsumerReady() {
				t.Fatal("partial startup reported successful activation", err)
			}
			serviceAwait(t, s.Done())
			if a.closeCalls.Load() != 1 || a.nextCalls.Load() != 0 || b.nextCalls.Load() != 0 {
				t.Fatal("partial startup consumed work or leaked the first stream")
			}
			want := int64(0)
			if variant == "handle_and_error" || variant == "second_probe" {
				want = 1
			}
			if b.closeCalls.Load() != want {
				t.Fatal("partial returned handle not retained for cleanup")
			}
			for _, format := range []string{"%v", "%+v", "%#v"} {
				if strings.Contains(fmt.Sprintf(format, s.Err()), "private") {
					t.Fatal("service logging disclosed diagnostics")
				}
			}
		})
	}
	t.Run("close_during_open", func(t *testing.T) {
		stream := newServiceStream()
		s := serviceForTest(t, stream)
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		defer once.Do(func() { close(release) })
		s.inputs[0].open = func(ctx context.Context) (persistenceStream, error) {
			close(entered)
			<-ctx.Done()
			<-release
			return stream, ctx.Err()
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		started := make(chan error, 1)
		go func() { started <- s.Start(ctx, context.Background()) }()
		serviceAwait(t, entered)
		short, stop := context.WithTimeout(ctx, 10*time.Millisecond)
		err := s.Close(short)
		stop()
		if !errors.Is(err, ErrPersistenceServicePending) || persistenceDone(s.Done()) {
			t.Fatal("startup not joined but Close reported done")
		}
		once.Do(func() { close(release) })
		if err := <-started; !errors.Is(err, ErrPersistenceServiceStart) {
			t.Fatal("cancelled startup returned success", err)
		}
		serviceAwait(t, s.Done())
		if stream.closeCalls.Load() != 1 || stream.nextCalls.Load() != 0 {
			t.Fatal("late startup handle was lost or activated")
		}
	})
}

func TestPersistenceServiceStorageFreshnessAndProbeJoin(t *testing.T) {
	stream := newServiceStream()
	s := serviceForTest(t, stream)
	s.options.ProbeInterval, s.options.ProbeTimeout = 5*time.Millisecond, 5*time.Millisecond
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var checks atomic.Int64
	s.probe = func(context.Context) error {
		if checks.Add(1) <= 2 {
			return nil
		}
		close(entered)
		<-release // Deliberately ignores timeout; cached readiness must expire.
		return nil
	}
	defer serviceSetup(t, s)()
	serviceAwait(t, entered)
	serviceEventually(t, func() bool { return !s.ConsumerReady() })
	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err := s.Close(short)
	cancel()
	if !errors.Is(err, ErrPersistenceServicePending) || stream.closeCalls.Load() != 0 || persistenceDone(s.Done()) {
		t.Fatal("unjoined probe released ownership", err)
	}
	once.Do(func() { close(release) })
	serviceAwait(t, s.Done())
	if s.ConsumerReady() || stream.closeCalls.Load() != 1 {
		t.Fatal("late successful probe revived readiness")
	}
}

func TestPersistenceServiceIdlePollAndTransportFailure(t *testing.T) {
	stream := newServiceStream()
	s := serviceForTest(t, stream)
	defer serviceSetup(t, s)()
	serviceEventually(t, func() bool { return stream.nextCalls.Load() >= 2 })
	if !s.ConsumerReady() || persistenceDone(s.Done()) {
		t.Fatal("empty queue polling timeout failed healthy service")
	}
	stream.once.Do(func() { close(stream.done) })
	serviceAwait(t, s.Done())
	if !errors.Is(s.Err(), ErrPersistenceServiceStopped) || s.ConsumerReady() || stream.closeCalls.Load() != 1 {
		t.Fatal("transport termination did not retire service", s.Err())
	}
}

func TestPersistenceServiceLateDeliveryAfterStopIsNotHandled(t *testing.T) {
	stream := newServiceStream()
	s := serviceForTest(t, stream)
	entered := make(chan struct{})
	stream.nextHook = func(ctx context.Context) (*rabbitmq.ConsumerDelivery, error) {
		close(entered)
		<-ctx.Done()
		return &rabbitmq.ConsumerDelivery{}, nil
	}
	s.handle = func(context.Context, *rabbitmq.ConsumerDelivery) error {
		t.Error("post-stop delivery handled")
		return nil
	}
	defer serviceSetup(t, s)()
	serviceAwait(t, entered)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if stream.nextCalls.Load() != 1 {
		t.Fatal("worker fetched again after stopping")
	}
}

func TestPersistenceServiceCloseObserverAndConcurrentCalls(t *testing.T) {
	stream := newServiceStream()
	s := serviceForTest(t, stream)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	stream.closeHook = func(context.Context) error { close(entered); <-release; return nil }
	defer serviceSetup(t, s)()
	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err := s.Close(short)
	cancel()
	serviceAwait(t, entered)
	if !errors.Is(err, ErrPersistenceServicePending) || persistenceDone(s.Done()) {
		t.Fatal("cleanup observer timeout pretended closure")
	}
	once.Do(func() { close(release) })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var calls sync.WaitGroup
	for i := 0; i < 10; i++ {
		calls.Add(1)
		go func() {
			defer calls.Done()
			s.StopAdmission()
			if err := s.Drain(ctx); err != nil {
				t.Error(err)
			}
			if err := s.Close(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	calls.Wait()
	if stream.closeCalls.Load() != 1 {
		t.Fatal("concurrent callers duplicated cleanup")
	}
}

type idlePersistenceSession struct{ done chan struct{} }

func (s *idlePersistenceSession) Done() <-chan struct{} { return s.done }
func (*idlePersistenceSession) OpenConsumer(context.Context, string, int) (*rabbitmq.ConsumerChannel, error) {
	panic("constructor must not open subscriptions")
}

func TestPersistenceServicePureConstructionAndValidation(t *testing.T) {
	// A sql.DB with no connector is sufficient because construction must not
	// query, ping or close it. No external configuration is read.
	pool := &sql.DB{}
	input := PersistenceConsumer{Session: &idlePersistenceSession{done: make(chan struct{})}, Prefetch: 1}
	o := DefaultPersistenceOptions()
	for _, consumers := range [][]PersistenceConsumer{nil, {{Session: (*idlePersistenceSession)(nil), Prefetch: 1}},
		{input, input}, {{Session: input.Session, Prefetch: 0}}, {{Session: input.Session, Prefetch: rabbitmq.MaxConsumerPrefetch + 1}},
		{{Session: &idlePersistenceSession{}, Prefetch: 1}}} {
		if s, err := NewPersistenceService(pool, consumers, o); s != nil || !errors.Is(err, ErrPersistenceServiceInput) {
			t.Fatal("invalid subscription group accepted", err)
		}
	}
	bad := o
	bad.ProcessingTimeout = 0
	if _, err := NewPersistenceService(pool, []PersistenceConsumer{input}, bad); !errors.Is(err, ErrPersistenceServiceInput) {
		t.Fatal("unbounded processing accepted")
	}
	consumers := []PersistenceConsumer{input}
	s, err := NewPersistenceService(pool, consumers, o)
	if err != nil || s.ConsumerReady() || persistenceDone(s.Done()) {
		t.Fatal("constructor activated service", err)
	}
	consumers[0].Session = nil
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil || !persistenceDone(s.Done()) {
		t.Fatal("idle Close failed", err)
	}
	if err := s.Start(ctx, context.Background()); !errors.Is(err, ErrPersistenceServiceStart) {
		t.Fatal("closed idle service activated")
	}
}

func TestPersistenceServiceDeadlineAndPanicsDoNotLeakSubscriptions(t *testing.T) {
	for _, scenario := range []string{"operation_deadline", "handler_panic", "probe_panic"} {
		t.Run(scenario, func(t *testing.T) {
			stream := newServiceStream()
			s := serviceForTest(t, stream)
			s.options.ProcessingTimeout = 10 * time.Millisecond
			if scenario == "probe_panic" {
				s.options.ProbeInterval = 5 * time.Millisecond
				var probes atomic.Int64
				s.probe = func(context.Context) error {
					if probes.Add(1) > 2 {
						panic("private SQL diagnostics")
					}
					return nil
				}
			} else {
				s.handle = func(ctx context.Context, _ *rabbitmq.ConsumerDelivery) error {
					if scenario == "handler_panic" {
						panic("private message body")
					}
					<-ctx.Done()
					return ctx.Err()
				}
			}
			defer serviceSetup(t, s)()
			if scenario != "probe_panic" {
				stream.next <- serviceNext{delivery: &rabbitmq.ConsumerDelivery{}}
			}
			serviceAwait(t, s.Done())
			if !errors.Is(s.Err(), ErrPersistenceServiceStopped) || stream.closeCalls.Load() != 1 || s.ConsumerReady() {
				t.Fatal("deadline/panic did not retire subscription", s.Err())
			}
			if scenario == "operation_deadline" && !errors.Is(s.Err(), context.DeadlineExceeded) {
				t.Fatal("processing deadline was lost")
			}
			if strings.Contains(fmt.Sprintf("%+v", s.Err()), "private") {
				t.Fatal("panic data exposed by ordinary logging")
			}
		})
	}
}

func TestPersistenceServiceLifetimeAndInvalidStartupContexts(t *testing.T) {
	stream := newServiceStream()
	s := serviceForTest(t, stream)
	if err := s.Start(context.Background(), context.Background()); !errors.Is(err, ErrPersistenceServiceInput) || stream.nextCalls.Load() != 0 {
		t.Fatal("unbounded startup activated work")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	lifetime, stop := context.WithCancel(context.Background())
	if err := s.Start(ctx, lifetime); err != nil {
		t.Fatal(err)
	}
	stop()
	serviceAwait(t, s.Done())
	if s.ConsumerReady() || stream.closeCalls.Load() != 1 {
		t.Fatal("lifetime cancellation leaked service")
	}
}
