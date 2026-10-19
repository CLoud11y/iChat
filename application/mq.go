// Package application assembles explicitly owned application dependencies.
package application

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"iChat/config"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
)

var (
	ErrMQOptions = errors.New("invalid MQ preparation options")
	ErrMQPrepare = errors.New("MQ dependency preparation failed")
	ErrMQClose   = errors.New("MQ dependency shutdown failed")
)

// MQOptions bounds the entire preparation and shutdown, independently of the
// per-connection deadlines. These are assembly inputs, not new YAML settings.
type MQOptions struct {
	SetupTimeout time.Duration
	CloseTimeout time.Duration
}

func DefaultMQOptions() MQOptions {
	return MQOptions{SetupTimeout: 30 * time.Second, CloseTimeout: 5 * time.Second}
}

func validMQOptions(o MQOptions) bool {
	return o.SetupTimeout > 0 && o.SetupTimeout <= time.Minute && o.CloseTimeout > 0 && o.CloseTimeout <= time.Minute
}

type MQPublisher interface {
	Publish(context.Context, messaging.Command, time.Time) (rabbitmq.PublishResult, error)
	WaitReady(context.Context) (uint64, error)
	Status() rabbitmq.SupervisorStatus
}

type MQConsumer interface {
	OpenConsumer(context.Context, string, int) (*rabbitmq.ConsumerChannel, error)
	Verify(context.Context) error
	Done() <-chan struct{}
}

// PreparedConsumer is an idle, verified connection, not an active subscription.
// Start it only after the business handler and its storage are assembled.
type PreparedConsumer struct {
	Queue    string
	Prefetch int
	Session  MQConsumer
}

type mqCloser interface{ Close(context.Context) error }
type ownedPublisher interface {
	MQPublisher
	mqCloser
}
type ownedConsumer interface {
	MQConsumer
	mqCloser
}

// MQResources owns every connection and the publisher recovery loop. The setup
// context only governs preparation; after success the caller must call Close.
// Accessors return capabilities/copies, never the mutable ownership registry.
type MQResources struct {
	mode      config.RuntimeMode
	options   MQOptions
	publisher ownedPublisher
	consumers []PreparedConsumer
	owned     []mqCloser
	cancel    context.CancelFunc

	mu          sync.Mutex
	stopped     bool
	closing     bool
	closed      bool
	attempt     *mqCloseAttempt
	persistence *PersistenceConsumerSource
}

type mqCloseAttempt struct {
	done chan struct{}
	err  error
}

func (*MQResources) String() string     { return "MQ resources (credentials redacted)" }
func (r *MQResources) GoString() string { return r.String() }

func (r *MQResources) Mode() config.RuntimeMode { return r.mode }

func (r *MQResources) Publisher() MQPublisher {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return nil
	}
	return r.publisher
}

func (r *MQResources) Consumers() []PreparedConsumer {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return nil
	}
	consumers, _ := r.consumerSnapshotLocked()
	return consumers
}

type mqFactories struct {
	publisher func(context.Context, context.Context, config.PublisherConfig) (ownedPublisher, error)
	consumer  func(context.Context, config.ConsumerConfig) (ownedConsumer, error)
}

// PrepareMQ rebuilds and validates the role plan from Config before any I/O.
// API creates an asynchronous supervisor (success need not mean broker-ready).
// Worker creates separately verified persistence/audit connections, with no
// QoS/Consume/ACK. No topology is declared or repaired by these constructors.
func PrepareMQ(ctx context.Context, c config.Config, o MQOptions) (*MQResources, error) {
	return prepareMQ(ctx, c, o, mqFactories{
		publisher: func(setup, lifetime context.Context, c config.PublisherConfig) (ownedPublisher, error) {
			if err := setup.Err(); err != nil {
				return nil, err
			}
			s, err := rabbitmq.StartSupervisor(lifetime, c.URL, c.Management, c.VHost, c.Options)
			if s == nil {
				return nil, err
			}
			return s, err
		},
		consumer: func(ctx context.Context, c config.ConsumerConfig) (ownedConsumer, error) {
			s, err := rabbitmq.DialConsumer(ctx, c.URL, c.Management, c.VHost, c.Options)
			if s == nil {
				return nil, err
			}
			return s, err
		},
	})
}

func prepareMQ(ctx context.Context, c config.Config, o MQOptions, f mqFactories) (result *MQResources, err error) {
	if ctx == nil || !validMQOptions(o) {
		return nil, ErrMQOptions
	}
	plan, err := c.MQRuntime()
	if err != nil {
		return nil, err
	}
	if (plan.Publisher != nil && f.publisher == nil) || (len(plan.Consumers) != 0 && f.consumer == nil) {
		return nil, ErrMQOptions
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	setupCtx, cancelSetup := context.WithTimeout(ctx, o.SetupTimeout)
	defer cancelSetup()
	// The publisher needs a lifetime distinct from the bounded setup context.
	lifetime, cancelLifetime := context.WithCancel(context.Background())
	r := &MQResources{mode: plan.Mode, options: o, cancel: cancelLifetime}
	watchDone := make(chan struct{})
	stopWatch := context.AfterFunc(setupCtx, func() {
		cancelLifetime()
		close(watchDone)
	})
	defer func() {
		// Join an already running cancellation before transferring ownership.
		if !stopWatch() {
			<-watchDone
		}
		if err == nil && setupCtx.Err() != nil {
			err = &mqFailure{kind: ErrMQPrepare, stage: "setup deadline", cause: setupCtx.Err()}
		}
		if err != nil {
			// Startup may already be cancelled. Rollback has its own total budget.
			err = errors.Join(err, r.Close(context.Background()))
			result = nil
		}
	}()
	if plan.Publisher != nil {
		p, e := f.publisher(setupCtx, lifetime, *plan.Publisher)
		if !nilMQResource(p) {
			r.owned = append(r.owned, p) // Also own a handle returned with an error.
			r.publisher = p
		}
		if e != nil || nilMQResource(p) {
			return nil, &mqFailure{kind: ErrMQPrepare, stage: "command publisher", cause: e}
		}
	}
	for _, group := range plan.Consumers {
		for i := 0; i < group.Settings.Workers; i++ {
			if e := setupCtx.Err(); e != nil {
				return nil, &mqFailure{kind: ErrMQPrepare, stage: "consumer setup deadline", cause: e}
			}
			s, e := f.consumer(setupCtx, group.Settings)
			if !nilMQResource(s) {
				r.owned = append(r.owned, s)
			}
			if e != nil || nilMQResource(s) {
				return nil, &mqFailure{kind: ErrMQPrepare, stage: fmt.Sprintf("consumer %s #%d", group.Queue, i+1), cause: e}
			}
			r.consumers = append(r.consumers, PreparedConsumer{Queue: group.Queue, Prefetch: group.Settings.Prefetch, Session: s})
		}
	}
	for _, group := range plan.Consumers {
		if group.Queue == rabbitmq.PersistQueue {
			r.persistence = newPersistenceConsumerSource(r, group.Settings, f.consumer, lifetime)
		}
	}
	return r, nil
}

// Close stops capability access and publisher recovery first, then closes all
// owned handles concurrently under ONE deadline. Even an expired caller context
// reaches every child Close, which stops admission/aborts its own transport.
// Calls are joined, never abandoned in goroutines. A later Close may retry after
// an incomplete shutdown; concurrent callers wait using their own deadlines.
func (r *MQResources) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if ctx == nil {
		return ErrMQOptions
	}
	closeCtx, cancel := context.WithTimeout(ctx, r.options.CloseTimeout)
	defer cancel()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	if r.closing {
		attempt := r.attempt
		r.mu.Unlock()
		select {
		case <-attempt.done:
			return attempt.err
		case <-closeCtx.Done():
			return &mqFailure{kind: ErrMQClose, stage: "concurrent shutdown wait", cause: closeCtx.Err()}
		}
	}
	r.stopped, r.closing = true, true
	attempt := &mqCloseAttempt{done: make(chan struct{})}
	r.attempt = attempt
	r.mu.Unlock()
	r.cancel()
	results := make([]error, len(r.owned))
	var wg sync.WaitGroup
	for i, resource := range r.owned {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = resource.Close(closeCtx)
		}()
	}
	wg.Wait() // All private factories must return context-bounded closers.
	var err error
	if cause := errors.Join(results...); cause != nil {
		err = &mqFailure{kind: ErrMQClose, stage: "owned resources", cause: cause}
	}
	r.mu.Lock()
	r.closing, r.closed = false, err == nil
	attempt.err = err
	close(attempt.done)
	r.mu.Unlock()
	return err
}

// Defend against a typed nil from a factory before adding a cleanup handle.
func nilMQResource(r mqCloser) bool {
	if r == nil {
		return true
	}
	v := reflect.ValueOf(r)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

// Retain errors.Is classification without formatting transport/factory errors,
// which may contain addresses or credentials. Do not log unwrapped causes.
type mqFailure struct {
	kind  error
	stage string
	cause error
}

func (e *mqFailure) Error() string    { return fmt.Sprintf("%s: %s", e.kind, e.stage) }
func (e *mqFailure) String() string   { return e.Error() }
func (e *mqFailure) GoString() string { return e.Error() }
func (e *mqFailure) Unwrap() []error  { return []error{e.kind, e.cause} }
