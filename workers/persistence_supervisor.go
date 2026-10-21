package workers

import (
	"context"
	"database/sql"
	"errors"
	"iChat/database"
	"iChat/messaging/rabbitmq"
	"sync"
	"time"
)

var (
	ErrPersistenceSupervisorInput   = errors.New("invalid persistence supervisor input")
	ErrPersistenceSupervisorStart   = errors.New("persistence supervisor cannot start")
	ErrPersistenceSupervisorFailed  = errors.New("persistence supervisor failed")
	ErrPersistenceSupervisorPending = errors.New("persistence supervisor shutdown pending")
	ErrPersistenceSupervisorClose   = errors.New("persistence consumer group cleanup failed")
)

// A group owns every session it supplies, including sessions whose subscription
// was never opened. Close is called exactly once, after the service has joined.
// It must join its own resource cleanup before returning, even on error. The
// supervisor retains late handles and waits for callbacks that ignore context.
// Neither the group nor its factory may close the shared SQL pool.
type PersistenceConsumerGroup interface {
	Consumers() []PersistenceConsumer
	Close(context.Context) error
}

// Each invocation must supply fresh, topology-verified dedicated sessions and
// return any partially acquired group alongside its error. A factory must not
// reuse a retired session. Runtime-owned replacement is wired separately.
type PersistenceConsumerFactory func(context.Context) (PersistenceConsumerGroup, error)

type PersistenceSupervisorOptions struct {
	Worker                                        PersistenceOptions
	SetupTimeout, BaseDelay, MaxDelay, ResetAfter time.Duration
}

func DefaultPersistenceSupervisorOptions() PersistenceSupervisorOptions {
	return PersistenceSupervisorOptions{Worker: DefaultPersistenceOptions(), SetupTimeout: 30 * time.Second,
		BaseDelay: time.Second, MaxDelay: 30 * time.Second, ResetAfter: time.Minute}
}

func ValidatePersistenceSupervisorOptions(o PersistenceSupervisorOptions) error {
	if ValidatePersistenceOptions(o.Worker) != nil || o.SetupTimeout <= 0 || o.SetupTimeout > time.Minute ||
		o.BaseDelay < 10*time.Millisecond || o.MaxDelay < o.BaseDelay || o.MaxDelay > time.Minute ||
		o.ResetAfter <= 0 || o.ResetAfter > 24*time.Hour {
		return ErrPersistenceSupervisorInput
	}
	return nil
}

type PersistenceSupervisorState string

const (
	PersistenceStateIdle     PersistenceSupervisorState = "idle"
	PersistenceStateStarting PersistenceSupervisorState = "starting"
	PersistenceStateReady    PersistenceSupervisorState = "ready"
	PersistenceStateRetiring PersistenceSupervisorState = "retiring"
	PersistenceStateBackoff  PersistenceSupervisorState = "backoff"
	PersistenceStateProbing  PersistenceSupervisorState = "probing"
	PersistenceStateStopping PersistenceSupervisorState = "stopping"
	PersistenceStateStopped  PersistenceSupervisorState = "stopped"
	PersistenceStateFailed   PersistenceSupervisorState = "failed"
)

// Generation counts factory invocations, not SQL probes. LastFailure is a safe
// classification, not a payload or SQL diagnostic. Ready is a local observation.
type PersistenceSupervisorStatus struct {
	State       PersistenceSupervisorState
	Generation  uint64
	Failures    uint32
	LastFailure PersistenceFailureClass
	Ready       bool
}

// PersistenceSupervisor serializes entire service generations. A failed group
// relinquishes all admitted SQL/ACK work and resource callbacks before another
// group can be prepared. It never retries an ACK, republishes or rejects a
// delivery. Broker redelivery and the repository's idempotency resolve old work.
type PersistenceSupervisor struct {
	mu      sync.Mutex
	options PersistenceSupervisorOptions
	factory PersistenceConsumerFactory
	probe   func(context.Context) error
	build   func(PersistenceConsumerGroup) (*PersistenceService, error)
	wait    func(context.Context, time.Duration) error

	status                     PersistenceSupervisorStatus
	started, stopping, closing bool
	current                    *PersistenceService
	control, work              context.Context
	controlCancel, workCancel  context.CancelFunc
	setupCancel                context.CancelFunc
	result                     error
	stop, closeRequest         chan struct{}
	drained, done              chan struct{}
	drainOnce, doneOnce        sync.Once
}

func (s *PersistenceSupervisor) String() string   { return "persistence supervisor" }
func (s *PersistenceSupervisor) GoString() string { return s.String() }

func NewPersistenceSupervisor(pool *sql.DB, factory PersistenceConsumerFactory, options PersistenceSupervisorOptions) (*PersistenceSupervisor, error) {
	if pool == nil || factory == nil || ValidatePersistenceSupervisorOptions(options) != nil {
		return nil, ErrPersistenceSupervisorInput
	}
	return newPersistenceSupervisor(options, factory, pool.PingContext, func(group PersistenceConsumerGroup) (*PersistenceService, error) {
		return NewPersistenceService(pool, group.Consumers(), options.Worker)
	}), nil
}

func newPersistenceSupervisor(options PersistenceSupervisorOptions, factory PersistenceConsumerFactory, probe func(context.Context) error,
	build func(PersistenceConsumerGroup) (*PersistenceService, error)) *PersistenceSupervisor {
	return &PersistenceSupervisor{options: options, factory: factory, probe: probe, build: build, wait: waitPersistenceRetry,
		status: PersistenceSupervisorStatus{State: PersistenceStateIdle}, stop: make(chan struct{}), closeRequest: make(chan struct{}),
		drained: make(chan struct{}), done: make(chan struct{})}
}

// Start observes the first activation attempt. An infrastructure outage starts
// a managed, unready backoff loop and returns nil. A terminal error returns an
// error. Setup cancellation after return never cancels recovery or active work.
func (s *PersistenceSupervisor) Start(setup, lifetime context.Context) error {
	if setup == nil || lifetime == nil {
		return ErrPersistenceSupervisorInput
	}
	if _, bounded := setup.Deadline(); !bounded {
		return ErrPersistenceSupervisorInput
	}
	if err := errors.Join(setup.Err(), lifetime.Err()); err != nil {
		return err
	}
	s.mu.Lock()
	if s.started || s.stopping {
		s.mu.Unlock()
		return ErrPersistenceSupervisorStart
	}
	s.started = true
	s.status.State = PersistenceStateStarting
	s.work, s.workCancel = context.WithCancel(lifetime)
	s.control, s.controlCancel = context.WithCancel(s.work)
	initial, cancel := context.WithTimeout(setup, s.options.SetupTimeout)
	s.setupCancel = cancel
	stopInitial := context.AfterFunc(s.control, cancel)
	s.mu.Unlock()
	defer stopInitial()
	first := make(chan error, 1)
	go s.run(initial, first)
	return <-first
}

func (s *PersistenceSupervisor) StopAdmission() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopping {
		s.stopping = true
		if s.status.State != PersistenceStateStopped && s.status.State != PersistenceStateFailed {
			s.status.State = PersistenceStateStopping
		}
		if s.controlCancel != nil {
			s.controlCancel()
			s.setupCancel()
		}
		close(s.stop)
	}
	if s.current != nil {
		s.current.StopAdmission()
	}
	if !s.started {
		s.drainOnce.Do(func() { close(s.drained) })
	}
}

func (s *PersistenceSupervisor) Drain(ctx context.Context) error {
	if ctx == nil {
		return ErrPersistenceSupervisorInput
	}
	s.StopAdmission()
	return s.observe(ctx, s.drained)
}

func (s *PersistenceSupervisor) Close(ctx context.Context) error {
	if ctx == nil {
		return ErrPersistenceSupervisorInput
	}
	s.StopAdmission()
	s.mu.Lock()
	if !s.closing {
		s.closing = true
		close(s.closeRequest)
	}
	if s.workCancel != nil {
		s.workCancel()
	}
	if !s.started {
		s.status.State = PersistenceStateStopped
		s.doneOnce.Do(func() { close(s.done) })
	}
	s.mu.Unlock()
	return s.observe(ctx, s.done)
}

func (s *PersistenceSupervisor) observe(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return s.Err()
	default:
	}
	select {
	case <-done:
		return s.Err()
	case <-ctx.Done():
		return persistenceServiceError(ErrPersistenceSupervisorPending, "wait", errors.Join(ctx.Err(), s.Err()))
	}
}

func (s *PersistenceSupervisor) Done() <-chan struct{} { return s.done }
func (s *PersistenceSupervisor) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result
}
func (s *PersistenceSupervisor) ConsumerReady() bool { return s.Status().Ready }
func (s *PersistenceSupervisor) Status() PersistenceSupervisorStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := s.status
	status.Ready = !s.stopping && s.work != nil && s.work.Err() == nil && status.State == PersistenceStateReady &&
		s.current != nil && s.current.ConsumerReady()
	return status
}

func (s *PersistenceSupervisor) setState(state PersistenceSupervisorState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopping {
		s.status.State = state
	}
}

func (s *PersistenceSupervisor) isStopping() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopping || s.work.Err() != nil
}

// Named handles survive errors/panics and cancellation after acquisition. The
// single owner, rather than a detached callback, performs all subsequent cleanup.
func (s *PersistenceSupervisor) activate(ctx context.Context) (group PersistenceConsumerGroup, service *PersistenceService, err error) {
	defer func() {
		if recover() != nil {
			err = persistenceServiceError(ErrPersistenceSupervisorFailed, "activation panic", nil)
		}
	}()
	if err = ctx.Err(); err != nil {
		return
	}
	check, cancel := context.WithTimeout(ctx, s.options.Worker.ProbeTimeout)
	err = s.probe(check)
	if err == nil {
		err = check.Err()
	}
	cancel()
	if err != nil {
		err = persistenceServiceError(ErrPersistenceServiceStart, "storage", err)
		return
	}
	if err = ctx.Err(); err != nil {
		return
	}
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		err = context.Canceled
		return
	}
	s.status.Generation++
	s.mu.Unlock()
	group, err = s.factory(ctx)
	if err != nil {
		return
	}
	if err = ctx.Err(); err != nil {
		return
	}
	if nilPersistenceValue(group) {
		err = ErrPersistenceSupervisorInput
		return
	}
	service, err = s.build(group)
	if err != nil {
		return
	}
	if service == nil {
		err = ErrPersistenceSupervisorInput
		return
	}
	s.mu.Lock()
	s.current = service
	stopped := s.stopping
	s.mu.Unlock()
	if stopped {
		service.StopAdmission()
		err = context.Canceled
		return
	}
	err = service.Start(ctx, s.work)
	return
}

func (s *PersistenceSupervisor) retire(group PersistenceConsumerGroup, service *PersistenceService) error {
	if service != nil {
		// Background is deliberate: observer timeouts cannot substitute for join.
		_ = service.Drain(context.Background())
	}
	cleanup, cancel := context.WithTimeout(context.Background(), s.options.Worker.CloseTimeout)
	defer cancel()
	var err error
	if service != nil {
		_ = service.Close(cleanup)
		<-service.Done()
		if errors.Is(service.Err(), ErrPersistenceServiceClose) {
			err = service.Err()
		}
	}
	if !nilPersistenceValue(group) {
		closeErr := func() (result error) {
			defer func() {
				if recover() != nil {
					result = persistenceServiceError(ErrPersistenceSupervisorClose, "panic", nil)
				}
			}()
			return group.Close(cleanup)
		}()
		if closeErr != nil {
			err = errors.Join(err, persistenceServiceError(ErrPersistenceSupervisorClose, "group", closeErr))
		}
	}
	s.mu.Lock()
	s.current = nil
	s.mu.Unlock()
	return err
}

func (s *PersistenceSupervisor) run(initial context.Context, first chan<- error) {
	announced := false
	announce := func(err error) {
		if !announced {
			announced = true
			first <- err
		}
	}
	ctx := initial
	var attemptCancel context.CancelFunc = func() {}
	defer func() {
		attemptCancel()
		s.setupCancel()
		s.controlCancel()
		s.workCancel()
		s.doneOnce.Do(func() { close(s.done) })
	}()
	for {
		group, service, err := s.activate(ctx)
		contextErr := ctx.Err()
		attemptCancel()
		s.setupCancel()
		readyAt := time.Now()
		if err == nil {
			s.setState(PersistenceStateReady)
			announce(nil)
			select {
			case <-service.stop:
			case <-s.stop:
			case <-s.work.Done():
				s.StopAdmission()
			}
		}
		stable := err == nil && time.Since(readyAt) >= s.options.ResetAfter
		if s.isStopping() {
			s.StopAdmission()
			if service != nil {
				_ = service.Drain(context.Background())
			}
			s.drainOnce.Do(func() { close(s.drained) })
			announce(errors.Join(err, contextErr, s.work.Err()))
			select {
			case <-s.closeRequest:
			case <-s.work.Done():
			}
			cleanupErr := s.retire(group, service)
			if service != nil {
				err = errors.Join(err, service.Err())
			}
			if supervisorCancellationOnly(err) {
				err = nil
			}
			s.finish(errors.Join(err, cleanupErr))
			return
		}
		s.setState(PersistenceStateRetiring)
		// Initial callers may observe a terminal activation error while its
		// already-acquired group is still being retired by this owner.
		if err != nil && (contextErr != nil || !persistenceRecoveryAllowed(err)) {
			announce(persistenceServiceError(ErrPersistenceSupervisorFailed, "activation", err))
		}
		cleanupErr := s.retire(group, service)
		if service != nil {
			err = errors.Join(err, service.Err())
		}
		if err == nil {
			err = persistenceServiceError(ErrPersistenceSupervisorFailed, "unexpected stop", nil)
		}
		if cleanupErr != nil || (contextErr != nil && ctx == initial) || !persistenceRecoveryAllowed(err) {
			terminal := persistenceServiceError(ErrPersistenceSupervisorFailed, "generation", errors.Join(err, cleanupErr))
			s.finish(terminal)
			announce(terminal)
			return
		}
		if s.isStopping() {
			// Close/Stop during retirement must never schedule another attempt.
			s.StopAdmission()
			s.drainOnce.Do(func() { close(s.drained) })
			announce(context.Canceled)
			select {
			case <-s.closeRequest:
			case <-s.work.Done():
			}
			s.finish(nil)
			return
		}
		s.mu.Lock()
		if stable {
			s.status.Failures = 0
		}
		s.status.Failures = min(s.status.Failures+1, 32)
		s.status.LastFailure = ClassifyPersistenceFailure(err)
		failures := s.status.Failures
		s.mu.Unlock()
		announce(nil)
		s.setState(PersistenceStateBackoff)
		policy := PersistenceRetryOptions{BaseDelay: s.options.BaseDelay, MaxDelay: s.options.MaxDelay}
		_ = s.wait(s.control, policy.delay(int(failures)))
		if s.isStopping() {
			// Re-enter the shutdown path without acquiring a group or probing.
			ctx = s.control
			continue
		}
		s.setState(PersistenceStateProbing)
		ctx, attemptCancel = context.WithTimeout(s.control, s.options.SetupTimeout)
	}
}

func (s *PersistenceSupervisor) finish(err error) {
	s.mu.Lock()
	s.result = err
	if err != nil {
		s.status.State = PersistenceStateFailed
	} else {
		s.status.State = PersistenceStateStopped
	}
	s.mu.Unlock()
	s.drainOnce.Do(func() { close(s.drained) })
}

// Recovery requires an entirely recognized infrastructure error tree. Phase
// markers alone are insufficient; malformed/technical/unknown/ACK/cancellation
// siblings veto recovery even if the aggregate classifier reports infrastructure.
func persistenceRecoveryAllowed(err error) bool {
	if err == nil || errors.Is(err, ErrPersistenceServiceClose) || errors.Is(err, ErrPersistenceSupervisorClose) {
		return false
	}
	evidence := false
	var visit func(error, bool) bool
	visit = func(current error, storage bool) bool {
		if current == nil {
			return false
		}
		if current == database.ErrCommandCommitUnknown || current == database.ErrCommandRollbackUnknown || current == database.ErrQuarantineCommitUnknown || current == database.ErrQuarantineRollbackUnknown {
			return true
		}
		if connection, ok := current.(*persistenceConnectionFailure); ok && connection != nil {
			evidence = connection.cause != nil || evidence
			return connection.cause != nil
		}
		if wrapper, ok := current.(*persistenceServiceFailure); ok {
			if wrapper.kind == ErrPersistenceServiceStart && wrapper.stage == "subscription" {
				switch wrapper.cause {
				case nil, rabbitmq.ErrConsumerUnavailable, rabbitmq.ErrConsumerChannelSetup, context.DeadlineExceeded:
					evidence = true
					return true // No delivery loops start until every subscription opens.
				}
				return false
			}
			if wrapper.kind == ErrPersistenceServiceStopped && wrapper.stage == "subscription closed" && wrapper.cause == nil {
				evidence = true
				return true
			}
			if wrapper.kind == ErrPersistenceServiceStopped && wrapper.stage == "receive" && wrapper.cause == rabbitmq.ErrConsumerUnavailable {
				evidence = true
				return true
			}
			if wrapper.stage != "storage" && wrapper.stage != "process" {
				return false
			}
			return visit(wrapper.cause, wrapper.stage == "storage")
		}
		if boundary, ok := current.(*PersistenceError); ok {
			if boundary.Kind != PersistenceFailed && boundary.Kind != CommitUnknown {
				return false
			}
			return visit(boundary.cause, false)
		}
		if boundary, ok := current.(*QuarantineError); ok {
			if boundary.Kind != QuarantineAuditFailed {
				return false
			}
			return visit(boundary.cause, true)
		}
		if storage && current == context.DeadlineExceeded {
			evidence = true
			return true
		}
		if multi, ok := current.(interface{ Unwrap() []error }); ok {
			children := multi.Unwrap()
			if len(children) == 0 {
				return false
			}
			for _, child := range children {
				if !visit(child, storage) {
					return false
				}
			}
			return true
		}
		if single, ok := current.(interface{ Unwrap() error }); ok && single.Unwrap() != nil {
			return visit(single.Unwrap(), storage)
		}
		if ClassifyPersistenceFailure(current) == FailureInfrastructure {
			evidence = true
			return true
		}
		return false
	}
	return visit(err, false) && evidence
}

func supervisorCancellationOnly(err error) bool {
	if err == nil {
		return false
	}
	if err == context.Canceled || err == context.DeadlineExceeded {
		return true
	}
	if connection, ok := err.(*persistenceConnectionFailure); ok && connection != nil {
		return supervisorCancellationOnly(connection.cause)
	}
	if wrapper, ok := err.(*persistenceServiceFailure); ok {
		// Start has admitted no deliveries until all subscriptions and the final
		// SQL probe succeed. Cancelling that setup during an orderly stop is not
		// a processing failure. Close/ACK/process phases must retain their errors.
		setup := wrapper.kind == ErrPersistenceServiceStart &&
			(wrapper.stage == "context" || wrapper.stage == "subscription" || wrapper.stage == "storage")
		probe := wrapper.kind == ErrPersistenceServiceStopped && wrapper.stage == "storage"
		return (setup || probe) && supervisorCancellationOnly(wrapper.cause)
	}
	if multi, ok := err.(interface{ Unwrap() []error }); ok {
		children := multi.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !supervisorCancellationOnly(child) {
				return false
			}
		}
		return true
	}
	return false
}
