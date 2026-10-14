package application

import (
	"context"
	"errors"
	"sync"

	"iChat/config"
)

// MQLifecycle owns an MQ resource group and one shutdown goroutine. Its context
// governs the lifetime, unlike PrepareMQ's context, which only governs setup.
// It owns connections, not business handlers or their in-flight transactions.
type MQLifecycle struct {
	resources *MQResources
	lifetime  context.Context
	stop      chan struct{}
	done      chan struct{}
	stopOnce  sync.Once
	result    error // Written before closing done; read only after observing done.
}

func (*MQLifecycle) String() string     { return "MQ lifecycle (credentials redacted)" }
func (m *MQLifecycle) GoString() string { return m.String() }

// StartMQLifecycle prepares idle MQ resources, then owns their shutdown until
// lifetime cancellation or Close. Business assembly must cancel this lifetime
// AFTER stopping admission and draining handlers, so they can still commit/ACK.
// This function does not start subscriptions, workers, HTTP or signal handling.
func StartMQLifecycle(lifetime context.Context, c config.Config, o MQOptions) (*MQLifecycle, error) {
	return startMQLifecycle(lifetime, c, o, PrepareMQ)
}

type mqPreparer func(context.Context, config.Config, MQOptions) (*MQResources, error)

func startMQLifecycle(lifetime context.Context, c config.Config, o MQOptions, prepare mqPreparer) (*MQLifecycle, error) {
	if lifetime == nil || !validMQOptions(o) || prepare == nil {
		return nil, ErrMQOptions
	}
	if err := lifetime.Err(); err != nil {
		return nil, err
	}
	r, err := prepare(lifetime, c, o)
	if err == nil && r == nil {
		err = ErrMQPrepare
	}
	if err == nil {
		err = lifetime.Err()
	}
	if err != nil {
		// Cancellation may race with a successful PrepareMQ handoff. Also own
		// any handle supplied with an error; do not inherit cancelled setup.
		if r != nil {
			err = errors.Join(err, r.Close(context.Background()))
		}
		return nil, &mqFailure{kind: ErrMQPrepare, stage: "lifecycle handoff", cause: err}
	}
	m := &MQLifecycle{resources: r, lifetime: lifetime, stop: make(chan struct{}), done: make(chan struct{})}
	go m.run()
	return m, nil
}

func (m *MQLifecycle) run() {
	select {
	case <-m.lifetime.Done():
	case <-m.stop:
	}
	m.requestStop()
	// MQResources supplies its own total bound. A cancelled lifetime must not
	// remove the shutdown budget. No Close call is abandoned in a goroutine.
	m.result = m.resources.Close(context.Background())
	close(m.done)
}

func (m *MQLifecycle) requestStop() { m.stopOnce.Do(func() { close(m.stop) }) }

func (m *MQLifecycle) stopping() bool {
	select {
	case <-m.stop:
		return true
	case <-m.lifetime.Done():
		return true
	default:
		return false
	}
}

func (m *MQLifecycle) Mode() config.RuntimeMode { return m.resources.Mode() }

func (m *MQLifecycle) Publisher() MQPublisher {
	if m.stopping() {
		return nil
	}
	return m.resources.Publisher()
}

func (m *MQLifecycle) Consumers() []PreparedConsumer {
	if m.stopping() {
		return nil
	}
	return m.resources.Consumers()
}

// Done closes after the owned shutdown attempt and goroutine finish. Inspect
// Wait's result: a timeout/error does not prove every child owner has exited.
func (m *MQLifecycle) Done() <-chan struct{} { return m.done }

// Wait observes completion using only the caller's context. Cancelling a wait
// does not stop the resource lifetime or another caller's shutdown attempt.
func (m *MQLifecycle) Wait(ctx context.Context) error {
	if ctx == nil {
		return ErrMQOptions
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-m.done:
		return m.result
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close requests shutdown, then waits under the caller's shorter context and
// the resource group's total close bound. An early return leaves the single
// owner finishing bounded cleanup; Done/Wait let the caller join it later.
// After a failed owner attempt, another Close also retries child cleanup within
// its remaining budget, retaining the first error rather than erasing it.
func (m *MQLifecycle) Close(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		return ErrMQOptions
	}
	closeCtx, cancel := context.WithTimeout(ctx, m.resources.options.CloseTimeout)
	defer cancel()
	m.requestStop()
	if err := m.Wait(closeCtx); err != nil {
		select {
		case <-m.done:
			return &mqFailure{kind: ErrMQClose, stage: "lifecycle shutdown", cause: errors.Join(err, m.resources.Close(closeCtx))}
		default:
			return &mqFailure{kind: ErrMQClose, stage: "lifecycle shutdown wait", cause: err}
		}
	}
	return nil
}
