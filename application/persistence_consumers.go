package application

import (
	"context"
	"errors"
	"iChat/config"
	"iChat/messaging/rabbitmq"
	"iChat/workers"
	"sync"
	"sync/atomic"
)

var ErrPersistenceConsumers = errors.New("persistence consumer source unavailable")

type joinedMQConsumer interface {
	ownedConsumer
	WaitClosed(context.Context) error
}

// PersistenceConsumerSource is a runtime-owned, single-claim connection source.
// Claiming transfers the initial persistence connections from the static MQ
// cleanup registry; later generations use the same validated configuration.
// Construction/claim perform no I/O and create no background tasks.
type PersistenceConsumerSource struct {
	resources        *MQResources
	settings         config.ConsumerConfig
	dial             func(context.Context, config.ConsumerConfig) (ownedConsumer, error)
	lifetime         context.Context
	mu               sync.Mutex
	claimed, stopped bool
	initial, active  *persistenceMQGroup
	acquiring        chan struct{}
	cancelAcquire    context.CancelFunc
	failure          error
	closeOnce        sync.Once
	closed           chan struct{}
	closeErr         error
}

func (s *PersistenceConsumerSource) String() string   { return "persistence consumer source" }
func (s *PersistenceConsumerSource) GoString() string { return s.String() }

func newPersistenceConsumerSource(r *MQResources, settings config.ConsumerConfig,
	dial func(context.Context, config.ConsumerConfig) (ownedConsumer, error), lifetime context.Context) *PersistenceConsumerSource {
	s := &PersistenceConsumerSource{resources: r, settings: settings, dial: dial, lifetime: lifetime, closed: make(chan struct{})}
	g := newPersistenceMQGroup(s)
	for _, consumer := range r.consumers {
		if consumer.Queue == rabbitmq.PersistQueue {
			g.sessions = append(g.sessions, consumer.Session.(ownedConsumer))
		}
	}
	s.initial = g
	return s
}

func (s *PersistenceConsumerSource) claim(inputs []PreparedConsumer) (workers.PersistenceConsumerFactory, error) {
	if s.resources == nil {
		return nil, ErrPersistenceConsumers
	}
	r := s.resources
	r.mu.Lock()
	defer r.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.stopped || s.stopped || s.claimed || len(inputs) != s.settings.Workers || s.initial == nil {
		return nil, ErrPersistenceConsumers
	}
	seen := make(map[<-chan struct{}]bool, len(inputs))
	for _, input := range inputs {
		if input.Queue != rabbitmq.PersistQueue || input.Prefetch != s.settings.Prefetch || nilRoleCapability(input.Session) {
			return nil, ErrPersistenceConsumers
		}
		done := input.Session.Done()
		if done == nil || seen[done] {
			return nil, ErrPersistenceConsumers
		}
		seen[done] = true
	}
	initialSeen := make(map[<-chan struct{}]bool, len(inputs))
	for _, session := range s.initial.sessions {
		if _, ok := session.(joinedMQConsumer); !ok || !seen[session.Done()] || initialSeen[session.Done()] {
			return nil, ErrPersistenceConsumers
		}
		initialSeen[session.Done()] = true
	}
	if len(initialSeen) != len(inputs) {
		return nil, ErrPersistenceConsumers
	}
	// Keep publisher/quarantine ownership unchanged. No mutation can race Close:
	// Close sets stopped under r.mu before reading this registry.
	owned := make([]mqCloser, 0, len(r.owned)-len(inputs)+1)
	for _, resource := range r.owned {
		if consumer, ok := resource.(ownedConsumer); ok && seen[consumer.Done()] {
			continue
		}
		owned = append(owned, resource)
	}
	r.owned = append(owned, s)
	s.claimed = true
	return s.prepare, nil
}

func (s *PersistenceConsumerSource) prepare(ctx context.Context) (result workers.PersistenceConsumerGroup, err error) {
	if ctx == nil {
		return nil, ErrPersistenceConsumers
	}
	s.mu.Lock()
	if cause := errors.Join(ctx.Err(), s.lifetime.Err()); cause != nil {
		s.mu.Unlock()
		return nil, cause
	}
	if !s.claimed || s.stopped || s.failure != nil || s.acquiring != nil || s.active != nil {
		s.mu.Unlock()
		return nil, ErrPersistenceConsumers
	}
	setup, cancel := context.WithTimeout(ctx, s.resources.options.SetupTimeout)
	watchDone := make(chan struct{})
	stopWatch := context.AfterFunc(s.lifetime, func() { cancel(); close(watchDone) })
	acquiring := make(chan struct{})
	s.acquiring, s.cancelAcquire = acquiring, cancel
	g := s.initial
	s.initial = nil
	fresh := g == nil
	if fresh {
		g = newPersistenceMQGroup(s)
	}
	s.mu.Unlock()
	defer func() {
		if recover() != nil {
			err = &mqFailure{kind: ErrPersistenceConsumers, stage: "acquisition panic"}
		}
		if !stopWatch() {
			<-watchDone
		}
		setupErr := setup.Err()
		cancel()
		s.mu.Lock()
		if err == nil {
			switch {
			case ctx.Err() != nil || s.lifetime.Err() != nil:
				err = errors.Join(ctx.Err(), s.lifetime.Err())
			case s.stopped:
				err = context.Canceled
			case setupErr != nil:
				err = workers.NewPersistenceConnectionError(setupErr)
			}
		}
		g.prepared = err == nil
		s.active = g // Includes partial/late handles, until their cleanup joins.
		s.acquiring, s.cancelAcquire = nil, nil
		result = g
		close(acquiring)
		s.mu.Unlock()
	}()
	if fresh {
		seen := make(map[<-chan struct{}]bool, s.settings.Workers)
		for i := 0; i < s.settings.Workers; i++ {
			if err = setup.Err(); err != nil {
				return g, workers.NewPersistenceConnectionError(err)
			}
			session, dialErr := s.dial(setup, s.settings)
			if !nilMQResource(session) {
				if done := session.Done(); done != nil && seen[done] {
					return g, ErrPersistenceConsumers
				} else {
					seen[done] = true
				}
				g.sessions = append(g.sessions, session)
			}
			if dialErr != nil {
				return g, workers.NewPersistenceConnectionError(dialErr)
			}
			if nilMQResource(session) {
				return g, ErrPersistenceConsumers
			}
			if _, ok := session.(joinedMQConsumer); !ok {
				return g, ErrPersistenceConsumers
			}
		}
	}
	// Initial prepared connections may have aged before SQL became healthy.
	// Recheck every connection with the same deadline; never repair topology.
	seen := make(map[<-chan struct{}]bool, len(g.sessions))
	for _, session := range g.sessions {
		if done := session.Done(); done == nil || seen[done] {
			return g, ErrPersistenceConsumers
		} else {
			seen[done] = true
		}
		if verifyErr := session.Verify(setup); verifyErr != nil {
			return g, workers.NewPersistenceConnectionError(verifyErr)
		}
	}
	if err = setup.Err(); err != nil {
		return g, workers.NewPersistenceConnectionError(err)
	}
	return g, nil
}

// consumerSnapshotLocked is called with r.mu held. Only the active generation
// replaces persistence entries; the quarantine connection remains independent.
func (r *MQResources) consumerSnapshotLocked() ([]PreparedConsumer, bool) {
	if r.persistence == nil {
		return append([]PreparedConsumer(nil), r.consumers...), true
	}
	s := r.persistence
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.claimed {
		return append([]PreparedConsumer(nil), r.consumers...), true
	}
	var consumers []PreparedConsumer
	for _, consumer := range r.consumers {
		if consumer.Queue != rabbitmq.PersistQueue {
			consumers = append(consumers, consumer)
		}
	}
	g := s.active
	ready := !s.stopped && s.failure == nil && g != nil && g.prepared && !g.closing.Load()
	if ready {
		for _, session := range g.sessions {
			consumers = append(consumers, PreparedConsumer{Queue: rabbitmq.PersistQueue, Prefetch: s.settings.Prefetch, Session: session})
		}
	}
	return consumers, ready
}

// Close is a bounded observer of a single cleanup owner. Group.Close below
// instead joins that owner, as required by the persistence supervisor contract.
func (s *PersistenceConsumerSource) Close(ctx context.Context) error {
	if ctx == nil {
		return ErrPersistenceConsumers
	}
	observer, cancelObserver := context.WithTimeout(ctx, s.resources.options.CloseTimeout)
	defer cancelObserver()
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.stopped = true
		if s.cancelAcquire != nil {
			s.cancelAcquire()
		}
		acquiring := s.acquiring
		s.mu.Unlock()
		go func() {
			if acquiring != nil {
				<-acquiring
			}
			s.mu.Lock()
			groups := []*persistenceMQGroup{s.initial, s.active}
			s.mu.Unlock()
			cleanup, cancel := context.WithTimeout(ctx, s.resources.options.CloseTimeout)
			defer cancel()
			var results [2]error
			var tasks sync.WaitGroup
			for i, group := range groups {
				if group != nil {
					tasks.Add(1)
					go func() { defer tasks.Done(); results[i] = group.Close(cleanup) }()
				}
			}
			tasks.Wait()
			s.mu.Lock()
			failure := s.failure
			s.mu.Unlock()
			if cause := errors.Join(failure, errors.Join(results[:]...)); cause != nil {
				s.closeErr = &mqFailure{kind: ErrMQClose, stage: "persistence source", cause: cause}
			}
			close(s.closed)
		}()
	})
	select {
	case <-s.closed:
		return s.closeErr
	default:
	}
	select {
	case <-s.closed:
		return s.closeErr
	case <-observer.Done():
		return &mqFailure{kind: ErrMQClose, stage: "persistence source pending", cause: observer.Err()}
	}
}

type persistenceMQGroup struct {
	source   *PersistenceConsumerSource
	sessions []ownedConsumer // Immutable once prepare returns.
	prepared bool            // Protected by source.mu.
	closing  atomic.Bool
	once     sync.Once
	done     chan struct{}
	err      error
}

func newPersistenceMQGroup(source *PersistenceConsumerSource) *persistenceMQGroup {
	return &persistenceMQGroup{source: source, done: make(chan struct{})}
}
func (g *persistenceMQGroup) Consumers() []workers.PersistenceConsumer {
	consumers := make([]workers.PersistenceConsumer, len(g.sessions))
	for i, session := range g.sessions {
		consumers[i] = workers.PersistenceConsumer{Session: session, Prefetch: g.source.settings.Prefetch}
	}
	return consumers
}
func (g *persistenceMQGroup) Close(ctx context.Context) error {
	if ctx == nil {
		return ErrPersistenceConsumers
	}
	g.once.Do(func() {
		g.closing.Store(true)
		go func() {
			cleanup, cancel := context.WithTimeout(ctx, g.source.resources.options.CloseTimeout)
			defer cancel()
			results := make([]error, len(g.sessions))
			var tasks sync.WaitGroup
			for i, session := range g.sessions {
				tasks.Add(1)
				go func() {
					defer tasks.Done()
					call := func(operation func() error) (err error) {
						defer func() {
							if recover() != nil {
								err = &mqFailure{kind: ErrMQClose, stage: "persistence cleanup panic"}
							}
						}()
						return operation()
					}
					results[i] = call(func() error { return session.Close(cleanup) })
					if joined, ok := session.(joinedMQConsumer); ok {
						results[i] = errors.Join(results[i], call(func() error { return joined.WaitClosed(context.Background()) }))
					}
				}()
			}
			tasks.Wait()
			if cause := errors.Join(results...); cause != nil {
				g.err = &mqFailure{kind: ErrMQClose, stage: "persistence group", cause: cause}
			}
			s := g.source
			s.mu.Lock()
			if s.active == g {
				s.active = nil
			}
			if s.initial == g {
				s.initial = nil
			}
			if g.err != nil {
				s.failure = &mqFailure{kind: ErrMQClose, stage: "persistence group", cause: g.err}
			}
			s.mu.Unlock()
			close(g.done)
		}()
	})
	<-g.done // A deadline cannot stand in for old transport quiescence.
	return g.err
}
