package application

import (
	"context"
	"database/sql"
	"iChat/messaging/rabbitmq"
	"iChat/workers"
)

var _ RuntimeService = (*workers.PersistenceService)(nil)
var _ RuntimeConsumerReadiness = (*workers.PersistenceService)(nil)
var _ RuntimeService = (*workers.PersistenceSupervisor)(nil)
var _ RuntimeConsumerReadiness = (*workers.PersistenceSupervisor)(nil)

// NewSupervisedPersistenceFactory uses the runtime-owned replaceable connection
// source. Idle construction claims cleanup ownership but performs no I/O. The
// caller retains the shared pool until the returned supervisor's Done closes.
func NewSupervisedPersistenceFactory(pool *sql.DB, options workers.PersistenceSupervisorOptions) (RuntimeFactory, error) {
	if pool == nil || workers.ValidatePersistenceSupervisorOptions(options) != nil {
		return nil, workers.ErrPersistenceSupervisorInput
	}
	return func(ctx context.Context, inputs RoleDependencies) (RuntimeService, error) {
		if ctx == nil || inputs.PersistenceConsumers == nil || inputs.QuarantineConsumers != nil || !nilRoleCapability(inputs.Publisher) {
			return nil, workers.ErrPersistenceSupervisorInput
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		factory, err := inputs.PersistenceConsumers.claim(inputs.Consumers)
		if err != nil {
			return nil, err
		}
		service, err := workers.NewPersistenceSupervisor(pool, factory, options)
		if err != nil {
			return nil, err
		}
		return service, nil
	}, nil
}

// NewPersistenceFactory adapts explicitly owned shared storage to the runtime.
// Neither factory creation nor service construction opens subscriptions, probes
// storage or migrates schema. The caller retains pool until service.Done, even
// when a Close observer returns pending. Default worker/all assembly stays gated
// until quarantine and Outbox services have also been implemented.
func NewPersistenceFactory(pool *sql.DB, options workers.PersistenceOptions) (RuntimeFactory, error) {
	if pool == nil {
		return nil, workers.ErrPersistenceServiceInput
	}
	if err := workers.ValidatePersistenceOptions(options); err != nil {
		return nil, err
	}
	return func(ctx context.Context, inputs RoleDependencies) (RuntimeService, error) {
		if ctx == nil {
			return nil, workers.ErrPersistenceServiceInput
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if inputs.QuarantineConsumers != nil || !nilRoleCapability(inputs.Publisher) {
			return nil, workers.ErrPersistenceServiceInput
		}
		consumers := make([]workers.PersistenceConsumer, 0, len(inputs.Consumers))
		for _, consumer := range inputs.Consumers {
			if consumer.Queue != rabbitmq.PersistQueue {
				return nil, workers.ErrPersistenceServiceInput
			}
			consumers = append(consumers, workers.PersistenceConsumer{Session: consumer.Session, Prefetch: consumer.Prefetch})
		}
		service, err := workers.NewPersistenceService(pool, consumers, options)
		if err != nil {
			return nil, err
		}
		return service, nil
	}, nil
}
