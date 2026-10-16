package main

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"time"

	"iChat/database"
	scheduledtasks "iChat/scheduledTasks"
	"iChat/utils"

	"github.com/go-redis/redis/v8"
	"github.com/robfig/cron/v3"
)

var (
	errLegacyWorkerInput    = errors.New("invalid legacy worker lifecycle input")
	errLegacyWorkerStart    = errors.New("legacy worker startup failed")
	errLegacyWorkerJob      = errors.New("legacy worker job failed")
	errLegacyWorkerPanic    = errors.New("legacy worker job panicked")
	errLegacyWorkerShutdown = errors.New("legacy worker shutdown failed")
)

type legacyWorkerOptions struct {
	Schedule       string
	JobTimeout     time.Duration
	ReleaseTimeout time.Duration
}

func defaultLegacyWorkerOptions() legacyWorkerOptions {
	return legacyWorkerOptions{Schedule: "@every 60s", JobTimeout: 30 * time.Second, ReleaseTimeout: 5 * time.Second}
}

type legacyWorkerOwnership interface {
	Check(context.Context) error
	Acknowledge(context.Context, []string) error
	Release(context.Context) error
}

// The acquire callback must return nil on failure, including uncertain SET NX
// results. Only a successful acquisition transfers a known ownership token.
func startLegacyWorker(setup context.Context, store scheduledtasks.MessageStore, rds *redis.Client, options legacyWorkerOptions) (*legacyCronWorker, error) {
	return startOwnedLegacyWorker(setup, store, options, func(ctx context.Context) (legacyWorkerOwnership, error) {
		return database.AcquireLegacyWorker(ctx, rds)
	})
}

func startOwnedLegacyWorker(setup context.Context, store scheduledtasks.MessageStore, options legacyWorkerOptions, acquire func(context.Context) (legacyWorkerOwnership, error)) (*legacyCronWorker, error) {
	if setup == nil || nilLegacyWorkerDependency(store) || acquire == nil {
		return nil, errLegacyWorkerInput
	}
	schedule, err := parseLegacyWorkerOptions(options)
	if err != nil {
		return nil, err
	}
	if err := setup.Err(); err != nil {
		return nil, legacyWorkerError(errLegacyWorkerStart, err)
	}
	owner, err := acquire(setup)
	if err != nil {
		return nil, legacyWorkerError(errLegacyWorkerStart, err)
	}
	if nilLegacyWorkerDependency(owner) {
		return nil, legacyWorkerError(errLegacyWorkerStart, errLegacyWorkerInput)
	}
	w := newLegacyCronWorker(schedule, store, owner, options)
	// A successful setup context is not the job lifetime. At this handoff a
	// cancelled setup gets a handle+error; its unique owner still cleans up.
	if err := setup.Err(); err != nil {
		w.requestClose()
		go w.manage()
		return w, legacyWorkerError(errLegacyWorkerStart, err)
	}
	w.cron.Start() // sets running synchronously before Stop can be requested
	go w.manage()
	return w, nil
}

func parseLegacyWorkerOptions(options legacyWorkerOptions) (cron.Schedule, error) {
	valid := func(d time.Duration) bool { return d > 0 && d <= time.Minute }
	if !valid(options.JobTimeout) || !valid(options.ReleaseTimeout) {
		return nil, errLegacyWorkerInput
	}
	schedule, err := cron.ParseStandard(options.Schedule)
	if err != nil {
		return nil, legacyWorkerError(errLegacyWorkerInput, err)
	}
	return schedule, nil
}

func nilLegacyWorkerDependency(value any) bool {
	if value == nil {
		return true
	}
	switch reflect.ValueOf(value).Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflect.ValueOf(value).IsNil()
	default:
		return false
	}
}

// Inputs have been validated and ownership acquired before constructing this
// idle worker. A schedule does not execute until the caller starts cron.
func newLegacyCronWorker(schedule cron.Schedule, store scheduledtasks.MessageStore, owner legacyWorkerOwnership, options legacyWorkerOptions) *legacyCronWorker {
	lifetime, cancel := context.WithCancel(context.Background())
	w := &legacyCronWorker{
		cron: cron.New(), lifetime: lifetime, cancel: cancel, options: options,
		stop: make(chan struct{}), closeRequest: make(chan struct{}), drained: make(chan struct{}), done: make(chan struct{}),
		job: func(ctx context.Context) error {
			return scheduledtasks.SyncMsg(ctx, scheduledtasks.OwnedMessages{MessageStore: store, Owner: owner})
		},
		release: owner.Release,
		report:  func(err error) { utils.Logger().WithError(err).Error("sync messages failed") },
	}
	w.cron.Schedule(schedule, cron.FuncJob(w.run))
	return w
}

// One manager calls cron.Stop once, joins every cron callback, then releases
// the token exactly once when Close has been requested. Observer deadlines do
// not abandon that responsibility or trigger duplicate release RPCs.
type legacyCronWorker struct {
	cron         *cron.Cron
	options      legacyWorkerOptions
	lifetime     context.Context
	cancel       context.CancelFunc
	job          func(context.Context) error
	release      func(context.Context) error
	report       func(error)
	mu           sync.Mutex
	stopped      bool
	running      bool
	stop         chan struct{}
	closeRequest chan struct{}
	closeOnce    sync.Once
	drained      chan struct{}
	done         chan struct{}
	result       error // immutable, published by closing done
}

func (w *legacyCronWorker) run() {
	w.mu.Lock()
	if w.stopped || w.running {
		w.mu.Unlock()
		return
	}
	w.running = true
	w.mu.Unlock()
	defer func() { w.mu.Lock(); w.running = false; w.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(w.lifetime, w.options.JobTimeout)
	defer cancel()
	defer func() {
		if recover() != nil {
			// Panic values and dependency errors can contain raw payloads or
			// credentials. The compatibility log only prints a fixed category.
			w.report(legacyWorkerError(errLegacyWorkerJob, errLegacyWorkerPanic))
		}
	}()
	if err := w.job(ctx); err != nil {
		w.report(legacyWorkerError(errLegacyWorkerJob, err))
	}
}

func (w *legacyCronWorker) StopAdmission() {
	w.mu.Lock()
	if !w.stopped {
		w.stopped = true
		close(w.stop)
	}
	w.mu.Unlock()
}

func (w *legacyCronWorker) Drain(ctx context.Context) error {
	if ctx == nil {
		return errLegacyWorkerInput
	}
	w.StopAdmission()
	return legacyWorkerError(errLegacyWorkerShutdown, waitLegacyWorker(ctx, w.drained))
}

func (w *legacyCronWorker) requestClose() {
	w.StopAdmission()
	w.cancel()
	w.closeOnce.Do(func() { close(w.closeRequest) })
}

func (w *legacyCronWorker) Close(ctx context.Context) error {
	if ctx == nil {
		return errLegacyWorkerInput
	}
	w.requestClose() // even an expired observer stops/cancels admitted work
	return w.Wait(ctx)
}

func (w *legacyCronWorker) Done() <-chan struct{} { return w.done }

func (w *legacyCronWorker) Wait(ctx context.Context) error {
	if ctx == nil {
		return errLegacyWorkerInput
	}
	if err := waitLegacyWorker(ctx, w.done); err != nil {
		return legacyWorkerError(errLegacyWorkerShutdown, err)
	}
	return w.result
}

func (w *legacyCronWorker) manage() {
	<-w.stop
	<-w.cron.Stop().Done()
	close(w.drained)
	<-w.closeRequest
	defer w.cancel()
	// A job still running after cancellation must finish before this point.
	// Releasing first could allow a successor to overlap its DB transaction.
	ctx, cancel := context.WithTimeout(context.Background(), w.options.ReleaseTimeout)
	w.result = legacyWorkerError(errLegacyWorkerShutdown, w.release(ctx))
	cancel()
	close(w.done)
}

func waitLegacyWorker(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type legacyWorkerFailure struct{ kind, cause error }

func (e *legacyWorkerFailure) Error() string    { return e.kind.Error() }
func (e *legacyWorkerFailure) String() string   { return e.Error() }
func (e *legacyWorkerFailure) GoString() string { return e.Error() }
func (e *legacyWorkerFailure) Unwrap() []error  { return []error{e.kind, e.cause} }

func legacyWorkerError(kind, cause error) error {
	if cause == nil {
		return nil
	}
	return &legacyWorkerFailure{kind: kind, cause: cause}
}
