package application

import (
	"context"
	"sync"

	"iChat/config"
	"iChat/health"
	"iChat/messaging/rabbitmq"
)

// RuntimeAcceptance is an explicit API capability, separate from Start and
// transport connectivity. AcceptanceReady confirms the confirmed-MQ handler
// is assembled and admitting work. AuthenticationReady must include fresh Redis
// authentication availability. Neither method may perform I/O or consult the
// aggregate RuntimeHealth (which would recurse). Missing capability fails closed.
type RuntimeAcceptance interface {
	AcceptanceReady() bool
	AuthenticationReady() bool
}

// RuntimeConsumerReadiness describes a business consumer's cached admission,
// subscription and storage state. An idle connection alone is insufficient.
// This does not gate acceptance: commands may safely accumulate while storage
// or consumers recover. Implementations must not perform I/O or aggregate reads.
type RuntimeConsumerReadiness interface {
	ConsumerReady() bool
}

// Outbox readiness is independent of MQ and API authentication availability.
type RuntimeOutboxReadiness interface{ OutboxReady() bool }

type readinessRole struct {
	role    RuntimeRole
	service RuntimeService
	done    <-chan struct{}
}

type runtimeHealth struct {
	mu        sync.RWMutex
	ctx       context.Context
	mode      config.RuntimeMode
	active    bool
	stopped   bool
	resources *MQResources
	roles     []readinessRole // Immutable after activation.
}

func (*runtimeHealth) String() string     { return "runtime health observer" }
func (h *runtimeHealth) GoString() string { return h.String() }

func (h *runtimeHealth) activate(resources *MQResources, entries []*runtimeEntry) {
	roles := make([]readinessRole, len(entries))
	for i, entry := range entries {
		roles[i] = readinessRole{entry.role, entry.service, entry.done}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.stopped {
		h.resources, h.roles, h.active = resources, roles, true
	}
}

func (h *runtimeHealth) stop() {
	h.mu.Lock()
	h.stopped = true
	h.mu.Unlock()
}

func (h *runtimeHealth) RuntimeReadiness() health.RuntimeStatus {
	h.mu.RLock()
	active, stopped, resources, roles := h.active, h.stopped, h.resources, h.roles
	h.mu.RUnlock()
	status := health.RuntimeStatus{}
	hasAPI := h.mode == config.ModeAPI || h.mode == config.ModeAll
	hasConsumers := h.mode == config.ModeWorker || h.mode == config.ModeAll
	setSelected := func(state health.ReadinessState) health.RuntimeStatus {
		if hasAPI {
			status.Acceptance = state
		}
		if hasConsumers {
			status.Consumers = state
			status.Outbox = state
		}
		return status
	}
	if stopped || h.ctx.Err() != nil {
		return setSelected(health.Stopping)
	}
	if !active {
		return setSelected(health.Starting)
	}
	for _, role := range roles {
		if readinessDone(role.done) {
			// Any selected service exit triggers shutdown of this process. A dead
			// consumer connection alone, below, remains a separate diagnostic.
			return setSelected(health.ServiceUnavailable)
		}
	}
	var publisher MQPublisher
	var consumers []PreparedConsumer
	consumerResourcesReady := false
	if resources != nil {
		resources.mu.Lock()
		publisher = resources.publisher
		consumers, consumerResourcesReady = resources.consumerSnapshotLocked()
		stopped = resources.stopped
		resources.mu.Unlock()
	}
	if stopped {
		return setSelected(health.Stopping)
	}
	if hasAPI {
		status.Acceptance = health.ServiceUnavailable
		for _, role := range roles {
			if role.role != RoleAPI {
				continue
			}
			api, ok := role.service.(RuntimeAcceptance)
			if !ok || nilRoleCapability(api) || !api.AcceptanceReady() {
				break
			}
			if !api.AuthenticationReady() {
				status.Acceptance = health.AuthenticationUnavailable
				break
			}
			status.Acceptance = health.PublisherUnavailable
			if !nilRoleCapability(publisher) {
				p := publisher.Status()
				if p.Ready && p.State == rabbitmq.SupervisorReady && p.ConnectionEpoch != 0 {
					status.Acceptance = health.Ready
				}
			}
		}
	}
	if hasConsumers {
		status.Outbox = health.OutboxUnavailable
		for _, role := range roles {
			if role.role == RoleOutbox {
				outbox, ok := role.service.(RuntimeOutboxReadiness)
				if ok && !nilRoleCapability(outbox) && outbox.OutboxReady() {
					status.Outbox = health.Ready
				}
			}
		}
		status.Consumers = health.Ready
		if len(consumers) == 0 || !consumerResourcesReady {
			status.Consumers = health.ConsumerUnavailable
		}
		for _, consumer := range consumers {
			if nilRoleCapability(consumer.Session) || readinessDone(consumer.Session.Done()) {
				status.Consumers = health.ConsumerUnavailable
				break
			}
		}
		for _, role := range roles {
			if role.role != RolePersistence && role.role != RoleQuarantine {
				continue
			}
			consumer, ok := role.service.(RuntimeConsumerReadiness)
			if !ok || nilRoleCapability(consumer) || !consumer.ConsumerReady() {
				status.Consumers = health.ConsumerUnavailable
			}
		}
	}
	// Cancellation can arrive while cached service methods are being sampled.
	h.mu.RLock()
	stopped = h.stopped || h.ctx.Err() != nil
	h.mu.RUnlock()
	if resources != nil {
		resources.mu.Lock()
		stopped = stopped || resources.stopped
		resources.mu.Unlock()
	}
	if stopped {
		return setSelected(health.Stopping)
	}
	return status
}

func readinessDone(done <-chan struct{}) bool {
	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}
