package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"iChat/database"
	scheduledtasks "iChat/scheduledTasks"
)

type lifecycleWorkerOwner struct {
	checks, releases, acks atomic.Int32
	onRelease              func(context.Context) error
	onAck                  func(context.Context, []string) error
}

func (o *lifecycleWorkerOwner) Check(ctx context.Context) error { o.checks.Add(1); return ctx.Err() }
func (o *lifecycleWorkerOwner) Acknowledge(ctx context.Context, prefix []string) error {
	o.acks.Add(1)
	if o.onAck != nil {
		return o.onAck(ctx, prefix)
	}
	return ctx.Err()
}
func (o *lifecycleWorkerOwner) Release(ctx context.Context) error {
	o.releases.Add(1)
	if o.onRelease != nil {
		return o.onRelease(ctx)
	}
	return ctx.Err()
}

type lifecycleWorkerStore struct {
	read    func(context.Context) ([]string, int, error)
	process func(context.Context, []string) (int, error)
}

func (s lifecycleWorkerStore) GetAllDirtyMsgs(ctx context.Context) ([]string, int, error) {
	if s.read != nil {
		return s.read(ctx)
	}
	return nil, 0, nil
}
func (s lifecycleWorkerStore) ProcessDirtyMsgs(ctx context.Context, raw []string) (int, error) {
	if s.process != nil {
		return s.process(ctx, raw)
	}
	return len(raw), nil
}
func (lifecycleWorkerStore) RemDirtyMsgs(context.Context, []string) error {
	panic("worker must use the ownership-checked acknowledgement")
}

// Use the real cron scheduler with a millisecond schedule, not manual calls
// to worker.run: cron.Stop must account for the actual callback goroutines.
type lifecycleWorkerSchedule struct{}

func (lifecycleWorkerSchedule) Next(now time.Time) time.Time { return now.Add(5 * time.Millisecond) }

func startLifecycleWorker(t *testing.T, store scheduledtasks.MessageStore, owner *lifecycleWorkerOwner, options legacyWorkerOptions) (*legacyCronWorker, <-chan error) {
	t.Helper()
	w := newLegacyCronWorker(lifecycleWorkerSchedule{}, store, owner, options)
	reports := make(chan error, 32)
	w.report = func(err error) {
		select {
		case reports <- err:
		default:
		}
	}
	w.cron.Start()
	go w.manage()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := w.Close(ctx); err != nil && !errors.Is(err, errLegacyWorkerShutdown) {
			t.Error(err)
		}
		select {
		case <-w.Done():
		case <-ctx.Done():
			t.Error("worker fixture did not join")
		}
	})
	return w, reports
}

func TestLegacyWorkerDrainPreservesJobAndCloseWaits(t *testing.T) {
	started, cancelled, allowFinish := make(chan struct{}), make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(allowFinish) })
	defer release()
	var jobContext context.Context
	owner := &lifecycleWorkerOwner{onRelease: func(ctx context.Context) error {
		if ctx.Err() != nil || jobContext.Err() != context.Canceled {
			t.Error("release inherited observer cancellation or preceded job cancellation")
		}
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > defaultLegacyWorkerOptions().ReleaseTimeout {
			t.Error("release has no bounded independent budget")
		}
		return nil
	}}
	store := lifecycleWorkerStore{read: func(ctx context.Context) ([]string, int, error) {
		jobContext = ctx
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-allowFinish
		return nil, 0, ctx.Err()
	}}
	w, _ := startLifecycleWorker(t, store, owner, defaultLegacyWorkerOptions())
	t.Cleanup(release)
	legacyHTTPAwait(t, started)
	w.StopAdmission()
	w.run() // queued callbacks must be refused after the gate has closed
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := w.Drain(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || jobContext.Err() != nil || owner.releases.Load() != 0 || owner.checks.Load() != 1 {
		t.Fatal("drain cancelled work, released ownership, or admitted a queued job", err)
	}
	expired, stop := context.WithCancel(context.Background())
	stop()
	if err := w.Close(expired); !errors.Is(err, context.Canceled) {
		t.Fatal("expired Close did not report observer cancellation", err)
	}
	legacyHTTPAwait(t, cancelled)
	if owner.releases.Load() != 0 {
		t.Fatal("token released before cancelled job finished")
	}
	select {
	case <-w.Done():
		t.Fatal("Close reported a joined job while cleanup was blocked")
	default:
	}
	release()
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := w.Wait(ctx); err != nil || owner.releases.Load() != 1 {
		t.Fatal("automatic cleanup did not retain ownership", err)
	}
	if err := w.Close(expired); err != nil || owner.releases.Load() != 1 {
		t.Fatal("completed Close was repeated", err)
	}
}

func TestLegacyWorkerGracefulCommitAckBeforeRelease(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(finish) })
	defer release()
	var committed, acknowledged atomic.Bool
	owner := &lifecycleWorkerOwner{
		onAck: func(ctx context.Context, prefix []string) error {
			if !committed.Load() || ctx.Err() != nil || len(prefix) != 1 || prefix[0] != "raw" {
				t.Error("ack preceded commit or lost the admitted context")
			}
			acknowledged.Store(true)
			return nil
		},
		onRelease: func(ctx context.Context) error {
			if !acknowledged.Load() {
				t.Error("release preceded the job's final acknowledgement")
			}
			return ctx.Err()
		},
	}
	store := lifecycleWorkerStore{
		read: func(context.Context) ([]string, int, error) { return []string{"raw"}, 1, nil },
		process: func(ctx context.Context, raw []string) (int, error) {
			close(started)
			<-finish
			committed.Store(true)
			return 1, nil
		},
	}
	w, _ := startLifecycleWorker(t, store, owner, defaultLegacyWorkerOptions())
	t.Cleanup(release)
	legacyHTTPAwait(t, started)
	w.StopAdmission()
	release()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := w.Drain(ctx); err != nil || !acknowledged.Load() || owner.releases.Load() != 0 {
		t.Fatal("graceful drain did not retain the token until Close", err)
	}
	if err := w.Close(ctx); err != nil || owner.releases.Load() != 1 {
		t.Fatal("graceful close failed", err)
	}
}

func TestLegacyWorkerTimeoutPanicAndNonOverlap(t *testing.T) {
	var calls, active, maximum atomic.Int32
	resumed := make(chan struct{}, 1)
	options := defaultLegacyWorkerOptions()
	options.JobTimeout = 25 * time.Millisecond
	store := lifecycleWorkerStore{read: func(ctx context.Context) ([]string, int, error) {
		n := active.Add(1)
		defer active.Add(-1)
		if n > maximum.Load() {
			maximum.Store(n)
		}
		switch calls.Add(1) {
		case 1:
			<-ctx.Done()
			return nil, 0, ctx.Err()
		case 2:
			panic("SECRET_TOKEN")
		default:
			select {
			case resumed <- struct{}{}:
			default:
			}
			return nil, 0, nil
		}
	}}
	w, reports := startLifecycleWorker(t, store, &lifecycleWorkerOwner{}, options)
	legacyHTTPAwait(t, resumed)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := w.Close(ctx); err != nil || maximum.Load() != 1 || calls.Load() < 3 {
		t.Fatal("job overlapped or did not recover after timeout/panic", err)
	}
	for _, want := range []error{context.DeadlineExceeded, errLegacyWorkerPanic} {
		select {
		case err := <-reports:
			if !errors.Is(err, want) || !errors.Is(err, errLegacyWorkerJob) || strings.Contains(fmt.Sprintf("%#v", err), "SECRET_TOKEN") {
				t.Fatal("job failure lost classification or exposed panic", err)
			}
		default:
			t.Fatal("missing job failure report")
		}
	}
}

func TestLegacyWorkerConcurrentClosePreservesReleaseFailure(t *testing.T) {
	entered, allowRelease := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(allowRelease) })
	defer release()
	failure := errors.New("redis://app:SECRET_TOKEN@internal")
	owner := &lifecycleWorkerOwner{onRelease: func(ctx context.Context) error {
		if ctx.Err() != nil {
			t.Error("release used cancelled observer")
		}
		close(entered)
		<-allowRelease
		return errors.Join(failure, context.DeadlineExceeded)
	}}
	w, _ := startLifecycleWorker(t, lifecycleWorkerStore{}, owner, defaultLegacyWorkerOptions())
	t.Cleanup(release)
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Wait(expired); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	w.mu.Lock()
	stopped := w.stopped
	w.mu.Unlock()
	if stopped || w.lifetime.Err() != nil {
		t.Fatal("observer Wait stopped the worker")
	}
	_ = w.Close(expired)
	legacyHTTPAwait(t, entered)
	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	results := make(chan error, 8)
	for range 8 {
		go func() { results <- w.Close(ctx) }()
	}
	release()
	var first error
	for range 8 {
		err := <-results
		if !errors.Is(err, failure) || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, errLegacyWorkerShutdown) {
			t.Fatal("release failure was lost", err)
		}
		if first == nil {
			first = err
		} else if err != first {
			t.Fatal("waiters did not observe the same terminal result")
		}
		for _, format := range []string{"%v", "%+v", "%#v"} {
			if strings.Contains(fmt.Sprintf(format, err), "SECRET_TOKEN") {
				t.Fatal("release error leaked credentials")
			}
		}
	}
	if owner.releases.Load() != 1 || w.Close(ctx) != first {
		t.Fatal("Close retried an uncertain token release")
	}
}

func TestLegacyWorkerReleaseDeadline(t *testing.T) {
	options := defaultLegacyWorkerOptions()
	options.ReleaseTimeout = 20 * time.Millisecond
	owner := &lifecycleWorkerOwner{onRelease: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }}
	w, _ := startLifecycleWorker(t, lifecycleWorkerStore{}, owner, options)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	first := w.Close(ctx)
	if !errors.Is(first, context.DeadlineExceeded) || owner.releases.Load() != 1 || w.Wait(ctx) != first || w.Close(ctx) != first {
		t.Fatal("release did not honor/retain its independent deadline", first)
	}
}

func TestLegacyWorkerSetupHandoff(t *testing.T) {
	for _, cancelledDuringAcquire := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelledDuringAcquire), func(t *testing.T) {
			setup, cancel := context.WithCancel(context.Background())
			defer cancel()
			owner := &lifecycleWorkerOwner{}
			w, err := startOwnedLegacyWorker(setup, lifecycleWorkerStore{}, defaultLegacyWorkerOptions(), func(context.Context) (legacyWorkerOwnership, error) {
				if cancelledDuringAcquire {
					cancel()
				}
				return owner, nil
			})
			if w == nil {
				t.Fatal("known token acquisition lost its cleanup handle", err)
			}
			ctx, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			defer func() {
				if err := w.Close(ctx); err != nil || owner.releases.Load() != 1 {
					t.Error("setup handle did not join/release", err)
				}
			}()
			if cancelledDuringAcquire {
				if !errors.Is(err, errLegacyWorkerStart) || !errors.Is(err, context.Canceled) {
					t.Fatal("cancelled handoff was accepted", err)
				}
				if err := w.Wait(ctx); err != nil || owner.releases.Load() != 1 || owner.checks.Load() != 0 {
					t.Fatal("startup rollback did not automatically release the idle worker", err)
				}
			} else {
				cancel()
				if err != nil || w.lifetime.Err() != nil {
					t.Fatal("successful setup context became job lifetime", err)
				}
			}
		})
	}
}

func TestLegacyWorkerPreflight(t *testing.T) {
	for _, name := range []string{"nil_context", "nil_store", "typed_nil_store", "nil_acquire", "schedule", "job_timeout", "release_timeout", "too_long", "cancelled", "busy", "nil_owner", "typed_nil_owner"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var setup context.Context = ctx
			var store scheduledtasks.MessageStore = lifecycleWorkerStore{}
			options := defaultLegacyWorkerOptions()
			var calls int
			acquire := func(context.Context) (legacyWorkerOwnership, error) {
				calls++
				if name == "busy" {
					return nil, database.ErrLegacyWorkerBusy
				}
				if name == "typed_nil_owner" {
					return (*lifecycleWorkerOwner)(nil), nil
				}
				return nil, nil
			}
			want := errLegacyWorkerInput
			switch name {
			case "nil_context":
				setup = nil
			case "nil_store":
				store = nil
			case "typed_nil_store":
				store = (*lifecycleWorkerStore)(nil)
			case "nil_acquire":
				acquire = nil
			case "schedule":
				options.Schedule = "SECRET_TOKEN"
			case "job_timeout":
				options.JobTimeout = 0
			case "release_timeout":
				options.ReleaseTimeout = -time.Second
			case "too_long":
				options.ReleaseTimeout = time.Minute + time.Nanosecond
			case "cancelled":
				cancel()
				want = context.Canceled
			case "busy":
				want = database.ErrLegacyWorkerBusy
			}
			w, err := startOwnedLegacyWorker(setup, store, options, acquire)
			if w != nil || !errors.Is(err, want) || strings.Contains(fmt.Sprintf("%#v", err), "SECRET_TOKEN") {
				t.Fatal("preflight failure", err)
			}
			if name != "busy" && name != "nil_owner" && name != "typed_nil_owner" && calls != 0 {
				t.Fatal("invalid input attempted ownership acquisition")
			}
		})
	}
	options := defaultLegacyWorkerOptions()
	w, _ := startLifecycleWorker(t, lifecycleWorkerStore{}, &lifecycleWorkerOwner{}, options)
	for _, err := range []error{w.Drain(nil), w.Close(nil), w.Wait(nil)} {
		if !errors.Is(err, errLegacyWorkerInput) {
			t.Fatal("nil lifecycle observer accepted", err)
		}
	}
}
