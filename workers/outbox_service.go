package workers

import (
	"context"
	"errors"
	"iChat/database"
	"sync"
	"time"
)

var (
	ErrOutboxServiceInput   = errors.New("invalid outbox service input")
	ErrOutboxServiceStart   = errors.New("outbox service activation failed")
	ErrOutboxServicePending = errors.New("outbox work still owns resources")
)

// Resources are transferred even on source error. Close runs once, after every
// source, probe and batch callback has returned. A nil Close means borrowed.
type OutboxResources struct {
	Store                           OutboxStore
	Publisher                       NotificationPublisher
	ProbeStorage, ProbeNotification func(context.Context) error
	Close                           func() error
}
type OutboxSource func(context.Context) (OutboxResources, error)
type OutboxServiceOptions struct {
	Loop                        OutboxOptions
	ProbeInterval, ProbeTimeout time.Duration
}

func DefaultOutboxServiceOptions() OutboxServiceOptions {
	return OutboxServiceOptions{Loop: OutboxOptions{Claim: database.OutboxClaimOptions{Limit: 10, Lease: 30 * time.Second}, Retry: database.OutboxRetryOptions{Base: time.Second, Max: time.Minute}, ClaimTimeout: 3 * time.Second, PublishTimeout: 2 * time.Second, SettlementTimeout: 3 * time.Second, PollInterval: time.Second}, ProbeInterval: 2 * time.Second, ProbeTimeout: time.Second}
}
func (o OutboxServiceOptions) Validate() error {
	if o.Loop.Validate() != nil || o.ProbeInterval <= 0 || o.ProbeInterval > time.Minute || o.ProbeTimeout <= 0 || o.ProbeTimeout > time.Minute {
		return ErrOutboxServiceInput
	}
	return nil
}

// OutboxService is a single-use, fail-stop role. Normal stop cancels admission
// and probes, preserving the processing lifetime until the admitted batch joins.
// Close/lifetime cancellation also cancels processing. Observer timeouts never
// transfer resource ownership or close Done prematurely.
type OutboxService struct {
	mu                                       sync.Mutex
	source                                   OutboxSource
	options                                  OutboxServiceOptions
	started, stopped, closing, ready         bool
	checked                                  time.Time
	work                                     context.Context
	result                                   error
	setupCancel, admissionCancel, workCancel context.CancelFunc
	stop, done                               chan struct{}
	finishOnce                               sync.Once
}

func NewOutboxService(source OutboxSource, options OutboxServiceOptions) (*OutboxService, error) {
	if source == nil || options.Validate() != nil {
		return nil, ErrOutboxServiceInput
	}
	return &OutboxService{source: source, options: options, stop: make(chan struct{}), done: make(chan struct{})}, nil
}
func (*OutboxService) String() string          { return "outbox service" }
func (s *OutboxService) GoString() string      { return s.String() }
func (s *OutboxService) Done() <-chan struct{} { return s.done }
func (s *OutboxService) Err() error            { s.mu.Lock(); defer s.mu.Unlock(); return s.result }
func (s *OutboxService) OutboxReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready && !s.stopped && !s.closing && s.work != nil && s.work.Err() == nil && time.Since(s.checked) <= s.options.ProbeInterval+s.options.ProbeTimeout
}
func (s *OutboxService) StopAdmission() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		s.stopped = true
		s.ready = false
		close(s.stop)
	}
	if s.setupCancel != nil {
		s.setupCancel()
	}
	if s.admissionCancel != nil {
		s.admissionCancel()
	}
}
func (s *OutboxService) Close(ctx context.Context) error {
	if ctx == nil {
		return ErrOutboxServiceInput
	}
	s.StopAdmission()
	s.mu.Lock()
	s.closing = true
	if s.workCancel != nil {
		s.workCancel()
	}
	idle := !s.started
	s.mu.Unlock()
	if idle {
		s.finish(nil)
	}
	return s.wait(ctx)
}
func (s *OutboxService) Drain(ctx context.Context) error {
	if ctx == nil {
		return ErrOutboxServiceInput
	}
	s.StopAdmission()
	s.mu.Lock()
	idle := !s.started
	s.mu.Unlock()
	if idle {
		s.finish(nil)
	}
	return s.wait(ctx)
}
func (s *OutboxService) wait(ctx context.Context) error {
	select {
	case <-s.done:
		return s.Err()
	default:
	}
	select {
	case <-s.done:
		return s.Err()
	case <-ctx.Done():
		return errors.Join(ErrOutboxServicePending, ctx.Err())
	}
}
func (s *OutboxService) finish(err error) {
	s.finishOnce.Do(func() { s.mu.Lock(); s.result = err; s.ready = false; s.mu.Unlock(); close(s.done) })
}

// Start waits only within setup. The owner goroutine retains late source/probe
// callbacks and resources if a dependency ignores its deadline.
func (s *OutboxService) Start(setup, lifetime context.Context) error {
	if setup == nil || lifetime == nil {
		return ErrOutboxServiceInput
	}
	if _, ok := setup.Deadline(); !ok {
		return ErrOutboxServiceInput
	}
	s.mu.Lock()
	if s.started || s.stopped || s.closing {
		s.mu.Unlock()
		return ErrOutboxServiceStart
	}
	s.started = true
	setupCtx, setupCancel := context.WithCancel(setup)
	s.setupCancel = setupCancel
	work, workCancel := context.WithCancel(lifetime)
	s.workCancel = workCancel
	s.work = work
	admission, admissionCancel := context.WithCancel(work)
	s.admissionCancel = admissionCancel
	s.mu.Unlock()
	activated := make(chan error, 1)
	go s.manage(setupCtx, work, admission, activated)
	select {
	case err := <-activated:
		setupCancel()
		if err != nil {
			s.StopAdmission()
			workCancel()
		}
		return err
	case <-setupCtx.Done():
		err := errors.Join(ErrOutboxServiceStart, setupCtx.Err())
		s.StopAdmission()
		workCancel()
		return err
	case <-work.Done():
		err := errors.Join(ErrOutboxServiceStart, work.Err())
		s.StopAdmission()
		workCancel()
		return err
	}
}
func (s *OutboxService) probe(ctx context.Context, r OutboxResources) error {
	for _, p := range []func(context.Context) error{r.ProbeStorage, r.ProbeNotification} {
		c, cancel := context.WithTimeout(ctx, s.options.ProbeTimeout)
		err := p(c)
		if err == nil {
			err = c.Err()
		}
		cancel()
		if err != nil {
			return &OutboxError{Stage: "dependency probe", cause: err}
		}
	}
	return nil
}
func (s *OutboxService) manage(setup, work, admission context.Context, activated chan<- error) {
	var resources OutboxResources
	var result error
	sent := false
	defer func() {
		if recover() != nil {
			result = &OutboxError{Stage: "service panic"}
		}
		s.StopAdmission()
		if !sent {
			activated <- errors.Join(ErrOutboxServiceStart, result)
		}
		if resources.Close != nil {
			result = errors.Join(result, safeOutboxClose(resources.Close))
		}
		s.mu.Lock()
		s.workCancel()
		s.mu.Unlock()
		s.finish(result)
	}()
	var err error
	if setup.Err() != nil || work.Err() != nil {
		result = errors.Join(ErrOutboxServiceStart, setup.Err(), work.Err())
		return
	}
	resources, err = s.source(setup)
	if err != nil {
		result = &OutboxError{Stage: "resource source", cause: err}
		return
	}
	if setup.Err() != nil || work.Err() != nil {
		result = errors.Join(ErrOutboxServiceStart, setup.Err(), work.Err())
		return
	}
	if nilPersistenceValue(resources.Store) || nilPersistenceValue(resources.Publisher) || resources.ProbeStorage == nil || resources.ProbeNotification == nil {
		result = ErrOutboxServiceInput
		return
	}
	loop, err := NewOutboxLoop(resources.Store, resources.Publisher, s.options.Loop)
	if err != nil {
		result = err
		return
	}
	if err = s.probe(setup, resources); err != nil {
		result = err
		return
	}
	s.mu.Lock()
	if setup.Err() != nil || work.Err() != nil || s.stopped {
		s.mu.Unlock()
		result = errors.Join(ErrOutboxServiceStart, setup.Err(), work.Err())
		return
	}
	s.ready, s.checked = true, time.Now()
	s.mu.Unlock()
	activated <- nil
	sent = true
	// A single owner serializes batches and probes. Probe freshness expires
	// during long processing; readiness does not borrow any aggregate state.
	nextProbe := time.Now().Add(s.options.ProbeInterval)
	for {
		if admission.Err() != nil {
			return
		}
		if !time.Now().Before(nextProbe) {
			if err = s.probe(admission, resources); err != nil {
				if admission.Err() == nil {
					result = err
				}
				return
			}
			s.mu.Lock()
			s.checked = time.Now()
			s.mu.Unlock()
			nextProbe = time.Now().Add(s.options.ProbeInterval)
		}
		if admission.Err() != nil {
			return
		}
		_, err = loop.runBatch(work, admission)
		if err != nil {
			if !(admission.Err() != nil && errors.Is(err, context.Canceled) && !outboxAmbiguous(err)) {
				result = err
			}
			return
		}
		delay := min(s.options.Loop.PollInterval, time.Until(nextProbe))
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-admission.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
func safeOutboxClose(closeResources func() error) (err error) {
	defer func() {
		if recover() != nil {
			err = &OutboxError{Stage: "resource close panic"}
		}
	}()
	if err := closeResources(); err != nil {
		return &OutboxError{Stage: "resource close", cause: err}
	}
	return nil
}
func outboxAmbiguous(err error) bool {
	return errors.Is(err, database.ErrOutboxCommitUnknown) || errors.Is(err, database.ErrOutboxRollbackUnknown)
}
