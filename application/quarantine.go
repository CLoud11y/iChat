package application

import (
	"context"
	"database/sql"

	"iChat/config"
	"iChat/messaging/rabbitmq"
	"iChat/workers"
)

// QuarantineConsumerSource is a role-specific capability over the common
// single-claim, topology-verified generation owner. It cannot claim persistence.
type QuarantineConsumerSource struct{ source *PersistenceConsumerSource }

func (s *QuarantineConsumerSource) String() string   { return "quarantine consumer source" }
func (s *QuarantineConsumerSource) GoString() string { return s.String() }

func newRoleConsumerSource(r *MQResources, settings config.ConsumerConfig, dial func(context.Context, config.ConsumerConfig) (ownedConsumer, error), lifetime context.Context, queue string) *PersistenceConsumerSource {
	s := &PersistenceConsumerSource{queue: queue, resources: r, settings: settings, dial: dial, lifetime: lifetime, closed: make(chan struct{})}
	g := newPersistenceMQGroup(s)
	for _, consumer := range r.consumers {
		if consumer.Queue == queue {
			g.sessions = append(g.sessions, consumer.Session.(ownedConsumer))
		}
	}
	s.initial = g
	return s
}

// The default process assembly remains gated until every required role exists.
// This explicit factory transfers just the selected audit consumer generation.
func NewSupervisedQuarantineFactory(pool *sql.DB, options workers.QuarantineSupervisorOptions) (RuntimeFactory, error) {
	if pool == nil || workers.ValidateQuarantineSupervisorOptions(options) != nil {
		return nil, workers.ErrQuarantineServiceInput
	}
	return func(ctx context.Context, inputs RoleDependencies) (RuntimeService, error) {
		if ctx == nil || inputs.QuarantineConsumers == nil || inputs.QuarantineConsumers.source == nil || inputs.PersistenceConsumers != nil || !nilRoleCapability(inputs.Publisher) {
			return nil, workers.ErrQuarantineServiceInput
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		source := inputs.QuarantineConsumers.source
		if source.queue != rabbitmq.QuarantineQueue {
			return nil, workers.ErrQuarantineServiceInput
		}
		factory, err := source.claim(inputs.Consumers)
		if err != nil {
			return nil, err
		}
		return workers.NewQuarantineSupervisor(pool, factory, options)
	}, nil
}

func NewQuarantineFactory(pool *sql.DB, options workers.QuarantineOptions) (RuntimeFactory, error) {
	if pool == nil || workers.ValidateQuarantineOptions(options) != nil {
		return nil, workers.ErrQuarantineServiceInput
	}
	return func(ctx context.Context, inputs RoleDependencies) (RuntimeService, error) {
		if ctx == nil || inputs.PersistenceConsumers != nil || !nilRoleCapability(inputs.Publisher) {
			return nil, workers.ErrQuarantineServiceInput
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		consumers := make([]workers.QuarantineConsumer, 0, len(inputs.Consumers))
		for _, consumer := range inputs.Consumers {
			if consumer.Queue != rabbitmq.QuarantineQueue {
				return nil, workers.ErrQuarantineServiceInput
			}
			consumers = append(consumers, workers.QuarantineConsumer{Session: consumer.Session, Prefetch: consumer.Prefetch})
		}
		return workers.NewQuarantineService(pool, consumers, options)
	}, nil
}
