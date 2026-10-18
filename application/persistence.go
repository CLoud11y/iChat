package application

import (
	"context"
	"database/sql"
	"iChat/messaging/rabbitmq"
	"iChat/workers"
)

var _ RuntimeService = (*workers.PersistenceService)(nil)
var _ RuntimeConsumerReadiness = (*workers.PersistenceService)(nil)

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
		if !nilRoleCapability(inputs.Publisher) {
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
