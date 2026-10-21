package workers

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"iChat/config"
	"iChat/database"
	"iChat/messaging/rabbitmq"
)

var ErrQuarantineServiceInput = errors.New("invalid quarantine service dependencies or options")

type QuarantineOptions struct {
	PollTimeout, ProcessingTimeout, ProbeInterval, ProbeTimeout, CloseTimeout time.Duration
}

func DefaultQuarantineOptions() QuarantineOptions {
	o := DefaultPersistenceOptions()
	return QuarantineOptions{o.PollTimeout, o.ProcessingTimeout, o.ProbeInterval, o.ProbeTimeout, o.CloseTimeout}
}

func (o QuarantineOptions) lifecycle() PersistenceOptions {
	return PersistenceOptions{PollTimeout: o.PollTimeout, ProcessingTimeout: o.ProcessingTimeout, ProbeInterval: o.ProbeInterval, ProbeTimeout: o.ProbeTimeout, CloseTimeout: o.CloseTimeout}
}

func ValidateQuarantineOptions(o QuarantineOptions) error {
	if ValidatePersistenceOptions(o.lifecycle()) != nil {
		return ErrQuarantineServiceInput
	}
	return nil
}

// The common lifecycle supplies admission, probing, drain and real task joins.
// Quarantine adds its own queue/handler; no persistence publisher or retry loop.
type QuarantineService struct{ *PersistenceService }

func (s *QuarantineService) String() string   { return "quarantine service" }
func (s *QuarantineService) GoString() string { return s.String() }

type QuarantineConsumer = PersistenceConsumer

func NewQuarantineService(pool *sql.DB, consumers []QuarantineConsumer, options QuarantineOptions) (*QuarantineService, error) {
	if pool == nil || len(consumers) == 0 || len(consumers) > config.MaxConsumerWorkers || ValidateQuarantineOptions(options) != nil {
		return nil, ErrQuarantineServiceInput
	}
	inputs := make([]persistenceSubscription, 0, len(consumers))
	seen := make(map[<-chan struct{}]bool, len(consumers))
	for _, consumer := range consumers {
		if nilPersistenceValue(consumer.Session) || consumer.Prefetch < 1 || consumer.Prefetch > rabbitmq.MaxConsumerPrefetch {
			return nil, ErrQuarantineServiceInput
		}
		done := consumer.Session.Done()
		if done == nil || seen[done] || persistenceDone(done) {
			return nil, ErrQuarantineServiceInput
		}
		seen[done] = true
		inputs = append(inputs, persistenceSubscription{done: done, open: func(ctx context.Context) (persistenceStream, error) {
			return consumer.Session.OpenConsumer(ctx, rabbitmq.QuarantineQueue, consumer.Prefetch)
		}})
	}
	repository, err := database.NewQuarantineRepository(pool)
	if err != nil {
		return nil, err
	}
	handler, err := NewQuarantineHandler(repository)
	if err != nil {
		return nil, err
	}
	return &QuarantineService{newPersistenceService(inputs, options.lifecycle(), pool.PingContext, func(ctx context.Context, d *rabbitmq.ConsumerDelivery) error {
		_, err := handler.Handle(ctx, d)
		return err
	})}, nil
}

type QuarantineSupervisorOptions struct {
	Worker                                        QuarantineOptions
	SetupTimeout, BaseDelay, MaxDelay, ResetAfter time.Duration
}

func DefaultQuarantineSupervisorOptions() QuarantineSupervisorOptions {
	o := DefaultPersistenceSupervisorOptions()
	return QuarantineSupervisorOptions{DefaultQuarantineOptions(), o.SetupTimeout, o.BaseDelay, o.MaxDelay, o.ResetAfter}
}
func (o QuarantineSupervisorOptions) lifecycle() PersistenceSupervisorOptions {
	return PersistenceSupervisorOptions{Worker: o.Worker.lifecycle(), SetupTimeout: o.SetupTimeout, BaseDelay: o.BaseDelay, MaxDelay: o.MaxDelay, ResetAfter: o.ResetAfter}
}
func ValidateQuarantineSupervisorOptions(o QuarantineSupervisorOptions) error {
	if ValidatePersistenceSupervisorOptions(o.lifecycle()) != nil {
		return ErrQuarantineServiceInput
	}
	return nil
}

type QuarantineConsumerGroup = PersistenceConsumerGroup
type QuarantineConsumerFactory = PersistenceConsumerFactory
type QuarantineSupervisor struct{ *PersistenceSupervisor }

func (s *QuarantineSupervisor) String() string   { return "quarantine supervisor" }
func (s *QuarantineSupervisor) GoString() string { return s.String() }

func NewQuarantineSupervisor(pool *sql.DB, factory QuarantineConsumerFactory, options QuarantineSupervisorOptions) (*QuarantineSupervisor, error) {
	if pool == nil || factory == nil || ValidateQuarantineSupervisorOptions(options) != nil {
		return nil, ErrQuarantineServiceInput
	}
	return &QuarantineSupervisor{newPersistenceSupervisor(options.lifecycle(), factory, pool.PingContext, func(group PersistenceConsumerGroup) (*PersistenceService, error) {
		service, err := NewQuarantineService(pool, group.Consumers(), options.Worker)
		if err != nil {
			return nil, err
		}
		return service.PersistenceService, nil
	})}, nil
}
