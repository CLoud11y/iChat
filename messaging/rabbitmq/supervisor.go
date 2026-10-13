package rabbitmq

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"iChat/messaging"
)

var (
	ErrSupervisorConfig   = errors.New("invalid RabbitMQ supervisor configuration")
	ErrSupervisorConnect  = errors.New("publisher session could not be established")
	ErrSupervisorTopology = errors.New("new publisher connection topology did not verify")
	ErrSupervisorSession  = errors.New("publisher session retired after a transport or notification failure")
	ErrSupervisorCleanup  = errors.New("old publisher session did not quiesce; refusing a new epoch")
	ErrSupervisorEpoch    = errors.New("publisher connection epoch exhausted")
)

type SupervisorOptions struct {
	Session  SessionOptions
	RetryMin time.Duration
	RetryMax time.Duration
}

func DefaultSupervisorOptions() SupervisorOptions {
	return SupervisorOptions{Session: DefaultSessionOptions(), RetryMin: time.Second, RetryMax: 30 * time.Second}
}

type SupervisorState string

const (
	SupervisorConnecting SupervisorState = "connecting"
	SupervisorReady      SupervisorState = "ready"
	SupervisorBackoff    SupervisorState = "backoff"
	SupervisorClosed     SupervisorState = "closed"
	SupervisorFailed     SupervisorState = "failed"
)

// Status is a point-in-time observation, not a guarantee a subsequent network
// operation will succeed. LastError contains only sanitized local sentinels.
type SupervisorStatus struct {
	State           SupervisorState
	Ready           bool
	ConnectionEpoch uint64
	LastError       error
}

type supervisedSession interface {
	Publish(context.Context, messaging.Command, time.Time) (PublishResult, error)
	Done() <-chan struct{}
	Close(context.Context) error
	abort()
	quiescent() <-chan struct{}
}
type sessionFactory func(context.Context, uint64) (supervisedSession, error)

// Supervisor runs exactly one recovery owner. It never buffers or retries
// commands; callers retry logical messages explicitly with their original keys.
type Supervisor struct {
	mu      sync.Mutex
	status  SupervisorStatus
	active  supervisedSession
	changed chan struct{}
	wake    chan struct{}
	done    chan struct{}
	cancel  context.CancelFunc
	ctx     context.Context
	options SupervisorOptions
	factory sessionFactory
	wait    func(context.Context, time.Duration) bool
	jitter  func(int64) int64
}

func (s *Supervisor) String() string { return "RabbitMQ publisher supervisor" }

// StartSupervisor returns before connecting. Every attempted epoch consumes a
// unique number, even failed handshakes/topology checks. Ready is set only after
// AMQP passive checks, management verification and fresh notification/confirm
// setup have completed for that connection.
func StartSupervisor(ctx context.Context, address string, management *Management, vhost string, o SupervisorOptions) (*Supervisor, error) {
	if ctx == nil || management == nil || management.client == nil {
		return nil, ErrSupervisorConfig
	}
	if err := ValidateSupervisorOptions(address, vhost, o); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if o.Session.TLS != nil {
		o.Session.TLS = o.Session.TLS.Clone()
	}
	return startSupervisor(ctx, o, func(ctx context.Context, epoch uint64) (supervisedSession, error) {
		options := o.Session
		options.Publisher.ConnectionEpoch = epoch
		session, err := dialPreparedPublisher(ctx, address, options, (&net.Dialer{}).DialContext, func(ctx context.Context, conn *amqp.Connection) error {
			return verifyNewConnection(ctx, conn, management, vhost)
		})
		if err != nil {
			return nil, err
		}
		return session, nil
	}), nil
}

// ValidateSupervisorOptions checks configuration without dialing, resolving a
// hostname, starting goroutines or contacting management. It shares the exact
// validation used by StartSupervisor and never includes credentials in errors.
func ValidateSupervisorOptions(address, vhost string, o SupervisorOptions) error {
	if !validSupervisorOptions(o) || !validSessionOptions(address, o.Session) {
		return ErrSupervisorConfig
	}
	uri, err := amqp.ParseURI(address)
	if err != nil || vhost == "" || uri.Vhost != vhost {
		return ErrSupervisorConfig
	}
	return nil
}

func validSupervisorOptions(o SupervisorOptions) bool {
	return o.RetryMin >= time.Millisecond && o.RetryMax >= o.RetryMin && o.RetryMax <= time.Minute
}

func verifyNewConnection(ctx context.Context, conn *amqp.Connection, m *Management, vhost string) error {
	if conn.Properties["version"] != BrokerVersion || conn.Config.Vhost != vhost {
		return ErrSupervisorTopology
	}
	if err := VerifyLocal(ctx, m, vhost); err != nil {
		return ErrSupervisorTopology
	}
	ch, err := conn.Channel()
	if err != nil {
		return ErrSupervisorTopology
	}
	defer ch.Close()
	for _, q := range localQueues() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := ch.ExchangeDeclarePassive(q.exchange, "direct", true, false, false, false, nil); err != nil {
			return ErrSupervisorTopology
		}
		if _, err := ch.QueueDeclarePassive(q.name, true, false, false, false, q.arguments); err != nil {
			return ErrSupervisorTopology
		}
	}
	// Passive checks do not validate every queue argument/binding. Re-read the
	// effective management contract before admitting work, without repairs.
	if err := VerifyLocal(ctx, m, vhost); err != nil {
		return ErrSupervisorTopology
	}
	return nil
}

func startSupervisor(ctx context.Context, o SupervisorOptions, factory sessionFactory) *Supervisor {
	ownedCtx, cancel := context.WithCancel(ctx)
	s := &Supervisor{status: SupervisorStatus{State: SupervisorConnecting}, changed: make(chan struct{}), wake: make(chan struct{}, 1), done: make(chan struct{}), cancel: cancel, ctx: ownedCtx, options: o, factory: factory, wait: waitBackoff, jitter: rand.Int64N}
	go s.run(ownedCtx)
	return s
}

func (s *Supervisor) run(ctx context.Context) {
	stop := watchCancellation(ctx, s.stop)
	defer func() { s.stop(); s.cancel(); stop(); close(s.done) }()
	epoch := s.options.Session.Publisher.ConnectionEpoch
	base := s.options.RetryMin
	for {
		if ctx.Err() != nil {
			return
		}
		s.setStatus(SupervisorConnecting, epoch, nil)
		current, err := s.factory(ctx, epoch)
		if err != nil || current == nil {
			if current != nil && !s.dispose(current) {
				s.fail(ErrSupervisorCleanup)
				return
			}
			cause := ErrSupervisorConnect
			if errors.Is(err, ErrSupervisorTopology) {
				cause = ErrSupervisorTopology
			}
			s.setStatus(SupervisorBackoff, epoch, cause)
		} else {
			if !s.install(current, epoch) {
				if !s.dispose(current) {
					s.fail(ErrSupervisorCleanup)
					return
				}
				if ctx.Err() != nil {
					return
				}
				s.setStatus(SupervisorBackoff, epoch, ErrSupervisorSession)
			} else {
				base = s.options.RetryMin
				select {
				case <-ctx.Done():
				case <-current.Done():
				case <-s.wake:
				}
				s.invalidate(current)
				if !s.dispose(current) {
					s.fail(ErrSupervisorCleanup)
					return
				}
				if ctx.Err() != nil {
					return
				}
			}
		}
		if epoch == math.MaxUint64 {
			s.fail(ErrSupervisorEpoch)
			return
		}
		if !s.wait(ctx, backoffDelay(base, s.jitter)) {
			return
		}
		epoch++
		base = nextBackoff(base, s.options.RetryMax)
	}
}

func (s *Supervisor) dispose(current supervisedSession) bool {
	current.abort()
	ctx, cancel := context.WithTimeout(context.Background(), s.options.Session.CloseTimeout)
	defer cancel()
	current.Close(ctx)
	// Do not open a new connection while old application calls still run.
	select {
	case <-current.quiescent():
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *Supervisor) install(current supervisedSession, epoch uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil || s.status.State == SupervisorClosed || s.status.State == SupervisorFailed {
		return false
	}
	select {
	case <-current.Done():
		return false
	default:
	}
	// A notification for a prior session cannot survive into a new install.
	select {
	case <-s.wake:
	default:
	}
	s.active = current
	s.status = SupervisorStatus{State: SupervisorReady, Ready: true, ConnectionEpoch: epoch}
	s.signal()
	return true
}

func (s *Supervisor) invalidate(current supervisedSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != current {
		return
	}
	s.active = nil
	if s.status.State != SupervisorClosed && s.status.State != SupervisorFailed {
		s.status.State = SupervisorBackoff
		s.status.Ready = false
		s.status.LastError = ErrSupervisorSession
	}
	current.abort()
	s.signal()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Supervisor) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.active
	s.active = nil
	if s.status.State != SupervisorFailed {
		s.status.State = SupervisorClosed
		s.status.Ready = false
	}
	if current != nil {
		current.abort()
	}
	s.signal()
}

func (s *Supervisor) setStatus(state SupervisorState, epoch uint64, cause error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status.State == SupervisorClosed || s.status.State == SupervisorFailed {
		return
	}
	s.status = SupervisorStatus{State: state, ConnectionEpoch: epoch, LastError: cause}
	s.signal()
}
func (s *Supervisor) fail(cause error) {
	s.mu.Lock()
	s.active = nil
	s.status.State = SupervisorFailed
	s.status.Ready = false
	s.status.LastError = cause
	s.signal()
	s.mu.Unlock()
}
func (s *Supervisor) signal() { close(s.changed); s.changed = make(chan struct{}) }

func (s *Supervisor) Status() SupervisorStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := s.status
	if s.ctx.Err() != nil {
		status.Ready = false
	}
	if s.active != nil {
		select {
		case <-s.active.Done():
			status.Ready = false
		default:
		}
	}
	return status
}

func (s *Supervisor) WaitReady(ctx context.Context) (uint64, error) {
	if ctx == nil {
		return 0, ErrSupervisorConfig
	}
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		s.mu.Lock()
		status := s.status
		changed := s.changed
		current := s.active
		ready := status.Ready && current != nil && s.ctx.Err() == nil
		if ready {
			select {
			case <-current.Done():
				ready = false
			default:
			}
		}
		s.mu.Unlock()
		if ready {
			return status.ConnectionEpoch, nil
		}
		if status.State == SupervisorClosed {
			return 0, ErrPublisherUnavailable
		}
		if status.State == SupervisorFailed {
			return 0, status.LastError
		}
		if s.ctx.Err() != nil {
			return 0, ErrPublisherUnavailable
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-changed:
		}
	}
}

func (s *Supervisor) Publish(ctx context.Context, c messaging.Command, receivedAt time.Time) (PublishResult, error) {
	r := PublishResult{Outcome: PublishNotStarted}
	if ctx == nil {
		return publishFailure(r, ErrPublisherDeadline)
	}
	if err := ctx.Err(); err != nil {
		return publishFailure(r, err)
	}
	if s.ctx.Err() != nil {
		return publishFailure(r, ErrPublisherUnavailable)
	}
	s.mu.Lock()
	current := s.active
	if s.ctx.Err() != nil {
		current = nil
	}
	s.mu.Unlock()
	if current == nil {
		return publishFailure(r, ErrPublisherUnavailable)
	}
	r, err := current.Publish(ctx, c, receivedAt)
	if r.Outcome == PublishUnknown || r.Outcome == PublishUnroutable || r.Outcome == PublishNacked || errors.Is(err, ErrPublisherUnavailable) || errors.Is(err, ErrPublisherNotifications) {
		s.invalidate(current)
	}
	return r, err
}

func (s *Supervisor) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		return ErrSupervisorConfig
	}
	closeCtx, cancel := context.WithTimeout(ctx, s.options.Session.CloseTimeout)
	defer cancel()
	s.stop()
	s.cancel()
	select {
	case <-s.done:
		return nil
	case <-closeCtx.Done():
		return closeCtx.Err()
	}
}

func waitBackoff(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
func backoffDelay(base time.Duration, jitter func(int64) int64) time.Duration {
	half := base / 2
	return half + time.Duration(jitter(int64(base-half)))
}
func nextBackoff(base, max time.Duration) time.Duration {
	if base >= max/2 {
		return max
	}
	return base * 2
}
