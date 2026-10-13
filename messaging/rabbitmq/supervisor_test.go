package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"iChat/messaging"
)

type supervisorPeer struct {
	done         chan struct{}
	idle         chan struct{}
	once         sync.Once
	closed       atomic.Int32
	publications atomic.Int32
	onPublish    func(context.Context, messaging.Command) (PublishResult, error)
	onClose      func()
}

func newSupervisorPeer() *supervisorPeer {
	idle := make(chan struct{})
	close(idle)
	return &supervisorPeer{done: make(chan struct{}), idle: idle}
}
func (p *supervisorPeer) Done() <-chan struct{}      { return p.done }
func (p *supervisorPeer) quiescent() <-chan struct{} { return p.idle }
func (p *supervisorPeer) abort()                     { p.once.Do(func() { close(p.done) }) }
func (p *supervisorPeer) Close(ctx context.Context) error {
	p.closed.Add(1)
	p.abort()
	if p.onClose != nil {
		p.onClose()
	}
	return nil
}
func (p *supervisorPeer) Publish(ctx context.Context, c messaging.Command, stamp time.Time) (PublishResult, error) {
	p.publications.Add(1)
	if p.onPublish != nil {
		return p.onPublish(ctx, c)
	}
	return PublishResult{Outcome: PublishAccepted, CommandID: c.CommandID}, nil
}

func testSupervisorOptions() SupervisorOptions {
	o := DefaultSupervisorOptions()
	o.Session = testSessionOptions()
	o.RetryMin = 10 * time.Millisecond
	o.RetryMax = 40 * time.Millisecond
	return o
}
func closeTestSupervisor(t *testing.T, s *Supervisor) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Error(err)
	}
}
func readySupervisor(t *testing.T, s *Supervisor) uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	epoch, err := s.WaitReady(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return epoch
}

func TestSupervisorRetryMonotonicEpochAndNoAutoPublish(t *testing.T) {
	var mu sync.Mutex
	var epochs []uint64
	var starts []time.Time
	p := newSupervisorPeer()
	s := startSupervisor(context.Background(), testSupervisorOptions(), func(ctx context.Context, epoch uint64) (supervisedSession, error) {
		mu.Lock()
		epochs = append(epochs, epoch)
		starts = append(starts, time.Now())
		count := len(epochs)
		mu.Unlock()
		if count < 4 {
			return nil, errors.New("amqp://u:secret@host/private")
		}
		return p, nil
	})
	t.Cleanup(func() { closeTestSupervisor(t, s) })
	if epoch := readySupervisor(t, s); epoch != 4 {
		t.Fatal("failed attempts reused epochs", epoch)
	}
	mu.Lock()
	defer mu.Unlock()
	for i, epoch := range epochs {
		if epoch != uint64(i+1) {
			t.Fatal("nonmonotonic epoch", epochs)
		}
		if i > 0 && starts[i].Sub(starts[i-1]) < time.Duration(5*(1<<min(i-1, 2)))*time.Millisecond {
			t.Fatal("retry was a busy loop")
		}
	}
	if p.publications.Load() != 0 {
		t.Fatal("reconnect auto-published a command")
	}
	if strings.Contains(fmt.Sprintf("%+v", s), "secret") || s.Status().LastError != nil {
		t.Fatal("supervisor leaked credentials or retained failure after ready")
	}
}

func TestSupervisorReadinessGateAndTopologyFailure(t *testing.T) {
	allow := make(chan struct{})
	entered := make(chan struct{})
	p := newSupervisorPeer()
	var attempts atomic.Int32
	s := startSupervisor(context.Background(), testSupervisorOptions(), func(ctx context.Context, epoch uint64) (supervisedSession, error) {
		if attempts.Add(1) == 1 {
			close(entered)
			select {
			case <-allow:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return nil, ErrSupervisorTopology
		}
		return p, nil
	})
	t.Cleanup(func() { closeTestSupervisor(t, s) })
	<-entered
	if s.Status().Ready {
		t.Fatal("connecting was ready")
	}
	if r, err := s.Publish(context.Background(), publisherCommand(t), time.Now()); !errors.Is(err, ErrPublisherUnavailable) || r.Outcome != PublishNotStarted {
		t.Fatal("published before verification", r, err)
	}
	close(allow)
	if epoch := readySupervisor(t, s); epoch != 2 {
		t.Fatal("topology failure reused first epoch", epoch)
	}
	if p.publications.Load() != 0 {
		t.Fatal("readiness probe published")
	}
}

func TestSupervisorReconnectWaitsForOldQuiescence(t *testing.T) {
	first, second := newSupervisorPeer(), newSupervisorPeer()
	first.idle = make(chan struct{})
	closed := make(chan struct{})
	first.onClose = func() { close(closed) }
	var calls atomic.Int32
	o := testSupervisorOptions()
	o.Session.CloseTimeout = 500 * time.Millisecond
	s := startSupervisor(context.Background(), o, func(ctx context.Context, epoch uint64) (supervisedSession, error) {
		if calls.Add(1) == 1 {
			return first, nil
		}
		select {
		case <-first.idle:
		default:
			t.Error("new connection overlapped old application calls")
		}
		return second, nil
	})
	t.Cleanup(func() { closeTestSupervisor(t, s) })
	readySupervisor(t, s)
	first.abort()
	<-closed
	if s.Status().Ready || calls.Load() != 1 {
		t.Fatal("new session started before old calls completed")
	}
	close(first.idle)
	if epoch := readySupervisor(t, s); epoch != 2 {
		t.Fatal("failed to reconnect", epoch)
	}
}

func TestSupervisorRefusesEpochWhenCleanupCannotJoin(t *testing.T) {
	p := newSupervisorPeer()
	p.idle = make(chan struct{})
	var calls atomic.Int32
	s := startSupervisor(context.Background(), testSupervisorOptions(), func(ctx context.Context, epoch uint64) (supervisedSession, error) { calls.Add(1); return p, nil })
	t.Cleanup(func() { close(p.idle); closeTestSupervisor(t, s) })
	readySupervisor(t, s)
	p.abort()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := s.WaitReady(ctx); !errors.Is(err, ErrSupervisorCleanup) {
		t.Fatal("stuck old calls were allowed a new epoch", err)
	}
	if calls.Load() != 1 || s.Status().State != SupervisorFailed {
		t.Fatal("cleanup failure retried unsafely")
	}
}

func TestSupervisorOldFailureCannotRetireReplacement(t *testing.T) {
	old, fresh := newSupervisorPeer(), newSupervisorPeer()
	entered := make(chan struct{})
	release := make(chan struct{})
	old.onPublish = func(ctx context.Context, c messaging.Command) (PublishResult, error) {
		close(entered)
		<-release
		return PublishResult{Outcome: PublishUnknown, ConnectionEpoch: 1}, ErrPublisherWrite
	}
	var calls atomic.Int32
	s := startSupervisor(context.Background(), testSupervisorOptions(), func(ctx context.Context, epoch uint64) (supervisedSession, error) {
		if calls.Add(1) == 1 {
			return old, nil
		}
		return fresh, nil
	})
	t.Cleanup(func() { closeTestSupervisor(t, s) })
	readySupervisor(t, s)
	finished := make(chan struct{})
	c := publisherCommand(t)
	go func() { s.Publish(context.Background(), c, time.Now()); close(finished) }()
	<-entered
	// The fake models an already-quiesced session whose external caller has
	// not yet processed its result. New install must ignore its late failure.
	old.abort()
	if epoch := readySupervisor(t, s); epoch != 2 {
		t.Fatal("new session not installed", epoch)
	}
	close(release)
	<-finished
	if !s.Status().Ready || s.Status().ConnectionEpoch != 2 {
		t.Fatal("old failure retired replacement")
	}
	select {
	case <-fresh.Done():
		t.Fatal("old result aborted new session")
	default:
	}
}

func TestSupervisorReturnRetiresWholeEpochButBusyAndInputDoNot(t *testing.T) {
	for _, outcome := range []PublishOutcome{PublishUnknown, PublishUnroutable, PublishNacked, PublishNotStarted} {
		t.Run(string(outcome), func(t *testing.T) {
			first, second := newSupervisorPeer(), newSupervisorPeer()
			cause := ErrPublisherBusy
			if outcome != PublishNotStarted {
				cause = ErrPublisherNotifications
			}
			first.onPublish = func(context.Context, messaging.Command) (PublishResult, error) {
				return PublishResult{Outcome: outcome, ConnectionEpoch: 1}, cause
			}
			var calls atomic.Int32
			s := startSupervisor(context.Background(), testSupervisorOptions(), func(context.Context, uint64) (supervisedSession, error) {
				if calls.Add(1) == 1 {
					return first, nil
				}
				return second, nil
			})
			t.Cleanup(func() { closeTestSupervisor(t, s) })
			readySupervisor(t, s)
			r, err := s.Publish(context.Background(), publisherCommand(t), time.Now())
			if r.Outcome != outcome || err == nil {
				t.Fatal("supervisor rewrote publication outcome")
			}
			if outcome == PublishNotStarted {
				if !s.Status().Ready || calls.Load() != 1 {
					t.Fatal("busy request invalidated healthy session")
				}
			} else if epoch := readySupervisor(t, s); epoch != 2 {
				t.Fatal("failed channel did not rebuild epoch", epoch)
			}
		})
	}
}

func TestSupervisorCanceledDuringDialBackoffAndReady(t *testing.T) {
	for _, mode := range []string{"dial", "backoff", "ready"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered := make(chan struct{})
			p := newSupervisorPeer()
			var calls atomic.Int32
			o := testSupervisorOptions()
			o.RetryMin = time.Second
			o.RetryMax = time.Second
			s := startSupervisor(ctx, o, func(ctx context.Context, epoch uint64) (supervisedSession, error) {
				calls.Add(1)
				close(entered)
				if mode == "dial" {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				if mode == "backoff" {
					return nil, ErrSupervisorTopology
				}
				return p, nil
			})
			<-entered
			if mode == "ready" {
				readySupervisor(t, s)
			}
			cancel()
			closeTestSupervisor(t, s)
			if calls.Load() != 1 || s.Status().Ready || s.Status().State != SupervisorClosed {
				t.Fatal("canceled recovery kept connecting")
			}
			if _, err := s.WaitReady(context.Background()); !errors.Is(err, ErrPublisherUnavailable) {
				t.Fatal("closed supervisor stayed waitable", err)
			}
		})
	}
}

func TestSupervisorEarlyDeadSessionAndEpochExhaustion(t *testing.T) {
	first, second := newSupervisorPeer(), newSupervisorPeer()
	first.abort()
	var calls atomic.Int32
	s := startSupervisor(context.Background(), testSupervisorOptions(), func(context.Context, uint64) (supervisedSession, error) {
		if calls.Add(1) == 1 {
			return first, nil
		}
		return second, nil
	})
	t.Cleanup(func() { closeTestSupervisor(t, s) })
	if epoch := readySupervisor(t, s); epoch != 2 {
		t.Fatal("session dying before install stopped recovery", epoch)
	}
	o := testSupervisorOptions()
	o.Session.Publisher.ConnectionEpoch = math.MaxUint64
	exhausted := startSupervisor(context.Background(), o, func(context.Context, uint64) (supervisedSession, error) { return nil, ErrSupervisorConnect })
	t.Cleanup(func() { closeTestSupervisor(t, exhausted) })
	if _, err := exhausted.WaitReady(publisherContext(t)); !errors.Is(err, ErrSupervisorEpoch) {
		t.Fatal("epoch wrapped instead of terminating", err)
	}
}

func TestSupervisorBackoffBoundsAndConfiguration(t *testing.T) {
	for _, base := range []time.Duration{time.Millisecond, time.Second, time.Minute} {
		if d := backoffDelay(base, func(int64) int64 { return 0 }); d < base/2 || d >= base {
			t.Fatal("low jitter invalid")
		}
		if d := backoffDelay(base, func(n int64) int64 { return n - 1 }); d < base/2 || d >= base {
			t.Fatal("high jitter invalid")
		}
	}
	if nextBackoff(20*time.Millisecond, 30*time.Millisecond) != 30*time.Millisecond {
		t.Fatal("backoff cap exceeded")
	}
	o := testSupervisorOptions()
	if validSupervisorOptions(SupervisorOptions{RetryMin: 0, RetryMax: time.Second}) {
		t.Fatal("zero backoff accepted")
	}
	for _, vhost := range []string{"", "wrong"} {
		if _, err := StartSupervisor(context.Background(), testSessionURI, &Management{}, vhost, o); !errors.Is(err, ErrSupervisorConfig) {
			t.Fatal("wrong vhost accepted")
		}
	}
	if _, err := StartSupervisor(nil, testSessionURI, nil, LocalVHost, o); !errors.Is(err, ErrSupervisorConfig) {
		t.Fatal("nil context/management accepted")
	}
}

func TestSupervisorLifetimeCancellationBlocksNewAdmission(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := newSupervisorPeer()
	s := startSupervisor(ctx, testSupervisorOptions(), func(context.Context, uint64) (supervisedSession, error) { return p, nil })
	t.Cleanup(func() { cancel(); closeTestSupervisor(t, s) })
	readySupervisor(t, s)
	cancel()
	if r, err := s.Publish(context.Background(), publisherCommand(t), time.Now()); !errors.Is(err, ErrPublisherUnavailable) || r.Outcome != PublishNotStarted || p.publications.Load() != 0 {
		t.Fatal("canceled lifetime admitted a fresh request", r, err)
	}
	if s.Status().Ready {
		t.Fatal("canceled lifetime remained ready")
	}
	if _, err := s.WaitReady(context.Background()); !errors.Is(err, ErrPublisherUnavailable) {
		t.Fatal("canceled lifetime returned a ready epoch", err)
	}
}

func TestSupervisorCanceledFactoryResultIsDisposedBeforeReady(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	p := newSupervisorPeer()
	s := startSupervisor(ctx, testSupervisorOptions(), func(ctx context.Context, epoch uint64) (supervisedSession, error) {
		close(entered)
		<-ctx.Done()
		return p, nil
	})
	<-entered
	cancel()
	closeTestSupervisor(t, s)
	if p.closed.Load() != 1 || s.Status().Ready {
		t.Fatal("late setup result was installed or leaked after cancellation")
	}
}
