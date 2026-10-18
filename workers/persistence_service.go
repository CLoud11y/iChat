package workers

import (
	"context"
	"database/sql"
	"errors"
	"iChat/config"
	"iChat/database"
	"iChat/messaging/rabbitmq"
	"reflect"
	"sync"
	"time"
)

var (
	ErrPersistenceServiceInput   = errors.New("invalid persistence service dependencies or options")
	ErrPersistenceServiceStart   = errors.New("persistence service activation failed")
	ErrPersistenceServiceStopped = errors.New("persistence service stopped")
	ErrPersistenceServicePending = errors.New("persistence service work has not joined")
	ErrPersistenceServiceClose   = errors.New("persistence subscription cleanup failed")
)

type PersistenceOptions struct {
	PollTimeout, ProcessingTimeout, ProbeInterval, ProbeTimeout, CloseTimeout time.Duration
	Retry                                                                     PersistenceRetryOptions
}

func DefaultPersistenceOptions() PersistenceOptions {
	return PersistenceOptions{PollTimeout: time.Second, ProcessingTimeout: 5 * time.Second,
		ProbeInterval: 2 * time.Second, ProbeTimeout: time.Second, CloseTimeout: 5 * time.Second, Retry: DefaultPersistenceRetryOptions()}
}

func ValidatePersistenceOptions(o PersistenceOptions) error {
	for _, d := range []time.Duration{o.PollTimeout, o.ProcessingTimeout, o.ProbeInterval, o.ProbeTimeout, o.CloseTimeout} {
		if d <= 0 || d > time.Minute {
			return ErrPersistenceServiceInput
		}
	}
	if _, err := resolvePersistenceRetry(o.Retry); err != nil {
		return ErrPersistenceServiceInput
	}
	return nil
}

// PersistenceSession is a prepared, dedicated consumer connection. The MQ
// resource group retains its ownership; this service only closes subscriptions
// it actually opened (which retires their dedicated single-channel sessions).
type PersistenceSession interface {
	OpenConsumer(context.Context, string, int) (*rabbitmq.ConsumerChannel, error)
	Done() <-chan struct{}
}

type PersistenceConsumer struct {
	Session  PersistenceSession
	Prefetch int
}

type persistenceStream interface {
	Next(context.Context) (*rabbitmq.ConsumerDelivery, error)
	Close(context.Context) error
	Done() <-chan struct{}
}

type persistenceSubscription struct {
	done <-chan struct{}
	open func(context.Context) (persistenceStream, error)
}

// PersistenceService is a single-use, fail-stop RuntimeService. Construction is
// idle. A worker handles one delivery at a time on each dedicated connection.
// No failure reopens subscriptions or requeues in a loop. The caller owns pool
// and must retain it until Done closes, even if a Close observer times out.
type PersistenceService struct {
	mu      sync.Mutex
	options PersistenceOptions
	inputs  []persistenceSubscription
	probe   func(context.Context) error
	handle  func(context.Context, *rabbitmq.ConsumerDelivery) error

	started, running, stopped, closing bool
	checked                            time.Time
	work                               context.Context
	setupCancel, workCancel            context.CancelFunc
	pollCancel, probeCancel            context.CancelFunc
	streams                            []persistenceStream
	streamDone                         []<-chan struct{}
	result                             error

	startDone, stop, closeRequest, drained, done chan struct{}
	drainOnce, doneOnce                          sync.Once
	tasks                                        sync.WaitGroup
}

func NewPersistenceService(pool *sql.DB, consumers []PersistenceConsumer, options PersistenceOptions) (*PersistenceService, error) {
	if pool == nil || len(consumers) == 0 || len(consumers) > config.MaxConsumerWorkers || ValidatePersistenceOptions(options) != nil {
		return nil, ErrPersistenceServiceInput
	}
	inputs := make([]persistenceSubscription, 0, len(consumers))
	seen := make(map[<-chan struct{}]bool)
	for _, consumer := range consumers {
		if nilPersistenceValue(consumer.Session) || consumer.Prefetch < 1 || consumer.Prefetch > rabbitmq.MaxConsumerPrefetch {
			return nil, ErrPersistenceServiceInput
		}
		done := consumer.Session.Done()
		if done == nil || seen[done] || persistenceDone(done) {
			return nil, ErrPersistenceServiceInput
		}
		seen[done] = true
		inputs = append(inputs, persistenceSubscription{done: done, open: func(ctx context.Context) (persistenceStream, error) {
			return consumer.Session.OpenConsumer(ctx, rabbitmq.PersistQueue, consumer.Prefetch)
		}})
	}
	repository, err := database.NewMessageRepository(pool)
	if err != nil {
		return nil, err
	}
	handler, err := NewPersistenceHandlerWithRetry(repository, options.Retry)
	if err != nil {
		return nil, err
	}
	return newPersistenceService(inputs, options, pool.PingContext, func(ctx context.Context, d *rabbitmq.ConsumerDelivery) error {
		_, err := handler.Handle(ctx, d)
		return err
	}), nil
}

func newPersistenceService(inputs []persistenceSubscription, options PersistenceOptions, probe func(context.Context) error,
	handle func(context.Context, *rabbitmq.ConsumerDelivery) error) *PersistenceService {
	return &PersistenceService{inputs: append([]persistenceSubscription(nil), inputs...), options: options, probe: probe, handle: handle,
		startDone: make(chan struct{}), stop: make(chan struct{}), closeRequest: make(chan struct{}), drained: make(chan struct{}), done: make(chan struct{})}
}

func (s *PersistenceService) Start(setup, lifetime context.Context) (err error) {
	if setup == nil || lifetime == nil {
		return ErrPersistenceServiceInput
	}
	if _, bounded := setup.Deadline(); !bounded {
		return ErrPersistenceServiceInput
	}
	s.mu.Lock()
	if s.started || s.stopped || s.closing {
		s.mu.Unlock()
		return ErrPersistenceServiceStart
	}
	s.started = true
	setupCtx, cancelSetup := context.WithCancel(setup)
	s.setupCancel = cancelSetup
	s.work, s.workCancel = context.WithCancel(lifetime)
	pollCtx, cancelPoll := context.WithCancel(s.work)
	probeCtx, cancelProbe := context.WithCancel(s.work)
	s.pollCancel, s.probeCancel = cancelPoll, cancelProbe
	s.mu.Unlock()
	go s.manage(lifetime)
	defer func() {
		if recover() != nil {
			err = persistenceServiceError(ErrPersistenceServiceStart, "panic", nil)
		}
		cancelSetup() // Successful activation is independent of setup thereafter.
		if err != nil {
			s.fail(err)
		}
		close(s.startDone) // No task Add or new handle acquisition after this point.
	}()
	if err = setupCtx.Err(); err == nil {
		err = s.work.Err()
	}
	if err != nil {
		return persistenceServiceError(ErrPersistenceServiceStart, "context", err)
	}
	check, cancel := context.WithTimeout(setupCtx, s.options.ProbeTimeout)
	err = s.probe(check)
	if err == nil {
		err = check.Err()
	}
	cancel()
	if err != nil {
		return persistenceServiceError(ErrPersistenceServiceStart, "storage", err)
	}
	for _, input := range s.inputs {
		if persistenceDone(input.done) {
			return persistenceServiceError(ErrPersistenceServiceStart, "subscription", nil)
		}
		if err := setupCtx.Err(); err != nil {
			return persistenceServiceError(ErrPersistenceServiceStart, "context", err)
		}
		stream, openErr := input.open(setupCtx)
		if !nilPersistenceValue(stream) {
			s.streams = append(s.streams, stream) // Preserve handle+error for cleanup.
		}
		if openErr != nil {
			return persistenceServiceError(ErrPersistenceServiceStart, "subscription", openErr)
		}
		if nilPersistenceValue(stream) || stream.Done() == nil || persistenceDone(stream.Done()) {
			return persistenceServiceError(ErrPersistenceServiceStart, "subscription", nil)
		}
		s.streamDone = append(s.streamDone, stream.Done())
	}
	// Opening many subscriptions may outlive the first storage observation.
	// Only a new successful probe may supply the activation freshness timestamp.
	check, cancel = context.WithTimeout(setupCtx, s.options.ProbeTimeout)
	err = s.probe(check)
	if err == nil {
		err = check.Err()
	}
	cancel()
	if err != nil {
		return persistenceServiceError(ErrPersistenceServiceStart, "storage", err)
	}
	s.mu.Lock()
	if setupCtx.Err() != nil || s.work.Err() != nil || s.stopped {
		s.mu.Unlock()
		return persistenceServiceError(ErrPersistenceServiceStart, "context", errors.Join(setupCtx.Err(), s.work.Err()))
	}
	for _, done := range s.streamDone {
		if persistenceDone(done) {
			s.mu.Unlock()
			return persistenceServiceError(ErrPersistenceServiceStart, "subscription", nil)
		}
	}
	s.running, s.checked = true, time.Now()
	s.tasks.Add(2*len(s.streams) + 1)
	s.mu.Unlock()
	for i, stream := range s.streams {
		go s.consume(pollCtx, stream)
		go s.watchSubscription(pollCtx, s.streamDone[i])
	}
	go s.monitor(probeCtx)
	return nil
}

func (s *PersistenceService) StopAdmission() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		s.stopped, s.running = true, false
		close(s.stop)
		for _, cancel := range []context.CancelFunc{s.setupCancel, s.pollCancel, s.probeCancel} {
			if cancel != nil {
				cancel()
			}
		}
	}
	if !s.started {
		s.drainOnce.Do(func() { close(s.drained) })
	}
}

func (s *PersistenceService) requestClose(force bool) {
	s.StopAdmission()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closing {
		s.closing = true
		close(s.closeRequest)
	}
	if force && s.workCancel != nil {
		s.workCancel()
	}
	if !s.started {
		s.doneOnce.Do(func() { close(s.done) })
	}
}

func (s *PersistenceService) Drain(ctx context.Context) error {
	if ctx == nil {
		return ErrPersistenceServiceInput
	}
	s.StopAdmission()
	return s.wait(ctx, s.drained)
}

func (s *PersistenceService) Close(ctx context.Context) error {
	if ctx == nil {
		return ErrPersistenceServiceInput
	}
	s.requestClose(true)
	return s.wait(ctx, s.done)
}

func (s *PersistenceService) wait(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return s.Err()
	default:
	}
	select {
	case <-done:
		return s.Err()
	case <-ctx.Done():
		return persistenceServiceError(ErrPersistenceServicePending, "wait", errors.Join(ctx.Err(), s.Err()))
	}
}

func (s *PersistenceService) Done() <-chan struct{} { return s.done }
func (s *PersistenceService) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result
}

func (s *PersistenceService) ConsumerReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running || s.stopped || s.work == nil || s.work.Err() != nil || s.stale() {
		return false
	}
	for _, done := range s.streamDone {
		if persistenceDone(done) {
			return false
		}
	}
	return true
}

// Called only under mu.
func (s *PersistenceService) stale() bool {
	return s.checked.IsZero() || time.Since(s.checked) > s.options.ProbeInterval+2*s.options.ProbeTimeout
}

func (s *PersistenceService) admit() bool {
	s.mu.Lock()
	stopped, stale := s.stopped || s.work.Err() != nil, s.stale()
	lost := false
	for _, done := range s.streamDone {
		lost = lost || persistenceDone(done)
	}
	s.mu.Unlock()
	if !stopped && stale {
		s.fail(persistenceServiceError(ErrPersistenceServiceStopped, "storage stale", nil))
	}
	if !stopped && lost {
		s.fail(persistenceServiceError(ErrPersistenceServiceStopped, "subscription closed", nil))
	}
	return !stopped && !stale && !lost
}

func (s *PersistenceService) consume(poll context.Context, stream persistenceStream) {
	defer s.tasks.Done()
	defer s.catchPanic()
	for s.admit() {
		wait, cancel := context.WithTimeout(poll, s.options.PollTimeout)
		delivery, err := stream.Next(wait)
		cancel()
		if poll.Err() != nil {
			return // A late delivery remains unacknowledged on the original stream.
		}
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && !persistenceDone(stream.Done()) {
				continue // Empty, healthy queues are not service failures.
			}
			s.fail(persistenceServiceError(ErrPersistenceServiceStopped, "receive", err))
			return
		}
		// This lock is the admission boundary relative to StopAdmission. A
		// handler admitted here keeps its work context through graceful drain.
		if !s.admit() {
			return
		}
		operation, finish := context.WithTimeout(s.work, s.options.ProcessingTimeout)
		err = s.handle(operation, delivery)
		if err == nil {
			err = operation.Err()
		}
		finish()
		if err != nil {
			s.fail(persistenceServiceError(ErrPersistenceServiceStopped, "process", err))
			return
		}
	}
}

func (s *PersistenceService) watchSubscription(ctx context.Context, done <-chan struct{}) {
	defer s.tasks.Done()
	select {
	case <-ctx.Done():
	case <-done:
		if ctx.Err() == nil {
			s.fail(persistenceServiceError(ErrPersistenceServiceStopped, "subscription closed", nil))
		}
	}
}

func (s *PersistenceService) monitor(ctx context.Context) {
	defer s.tasks.Done()
	defer s.catchPanic()
	timer := time.NewTicker(s.options.ProbeInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		check, cancel := context.WithTimeout(ctx, s.options.ProbeTimeout)
		err := s.probe(check)
		if err == nil {
			err = check.Err()
		}
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.fail(persistenceServiceError(ErrPersistenceServiceStopped, "storage", err))
			return
		}
		s.mu.Lock()
		if !s.stopped {
			s.checked = time.Now()
		}
		s.mu.Unlock()
	}
}

func (s *PersistenceService) catchPanic() {
	if recover() != nil {
		s.fail(persistenceServiceError(ErrPersistenceServiceStopped, "panic", nil))
	}
}

func (s *PersistenceService) fail(err error) {
	s.mu.Lock()
	s.result = errors.Join(s.result, err)
	s.mu.Unlock()
	s.requestClose(false) // Stop siblings' polling, allow admitted work to finish.
}

func (s *PersistenceService) manage(lifetime context.Context) {
	select {
	case <-s.stop:
	case <-lifetime.Done():
		s.requestClose(true)
	}
	<-s.startDone
	s.tasks.Wait()
	s.drainOnce.Do(func() { close(s.drained) })
	select {
	case <-s.closeRequest:
	case <-lifetime.Done():
		s.requestClose(true)
	}
	s.workCancel()
	cleanup, cancel := context.WithTimeout(context.Background(), s.options.CloseTimeout)
	defer cancel()
	var closers sync.WaitGroup
	for _, stream := range s.streams {
		closers.Add(1)
		go func() {
			defer closers.Done()
			defer func() {
				if recover() != nil {
					s.fail(persistenceServiceError(ErrPersistenceServiceClose, "panic", nil))
				}
			}()
			if err := stream.Close(cleanup); err != nil {
				s.fail(persistenceServiceError(ErrPersistenceServiceClose, "subscription", err))
			}
		}()
	}
	closers.Wait() // Observer expiry never means these tasks have joined.
	s.doneOnce.Do(func() { close(s.done) })
}

func persistenceDone(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

func nilPersistenceValue(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return v.IsNil()
	}
	return false
}

type persistenceServiceFailure struct {
	kind, cause error
	stage       string
}

func persistenceServiceError(kind error, stage string, cause error) error {
	return &persistenceServiceFailure{kind: kind, stage: stage, cause: cause}
}
func (e *persistenceServiceFailure) Error() string    { return e.kind.Error() + ": " + e.stage }
func (e *persistenceServiceFailure) GoString() string { return e.Error() }
func (e *persistenceServiceFailure) Unwrap() []error {
	if e.cause == nil {
		return []error{e.kind}
	}
	return []error{e.kind, e.cause}
}
