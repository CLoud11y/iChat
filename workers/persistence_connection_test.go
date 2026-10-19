package workers

import (
	"context"
	"errors"
	"iChat/messaging/rabbitmq"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPersistenceConnectionRecoveryHonorsPhaseBoundaries(t *testing.T) {
	cause := errors.New("private connection credentials")
	connection := NewPersistenceConnectionError(cause)
	if !persistenceRecoveryAllowed(connection) || !errors.Is(connection, cause) || !errors.Is(connection, ErrPersistenceConnection) ||
		ClassifyPersistenceFailure(connection) != FailureInfrastructure || strings.Contains(connection.Error(), "private") {
		t.Fatal("connection phase evidence lost or exposed", connection)
	}
	for _, err := range []error{NewPersistenceConnectionError(nil), errors.Join(connection, &PersistenceError{Kind: InvalidDelivery}),
		errors.Join(connection, &PersistenceError{Kind: AcknowledgementFailed}), errors.Join(connection, context.Canceled),
		errors.Join(connection, errors.New("unknown process error")),
		persistenceServiceError(ErrPersistenceServiceStart, "subscription", rabbitmq.ErrConsumerBusy)} {
		if persistenceRecoveryAllowed(err) {
			t.Fatal("connection phase masked an unsafe sibling or startup phase", err)
		}
	}
	for _, cause := range []error{nil, rabbitmq.ErrConsumerUnavailable, rabbitmq.ErrConsumerChannelSetup, context.DeadlineExceeded} {
		if !persistenceRecoveryAllowed(persistenceServiceError(ErrPersistenceServiceStart, "subscription", cause)) {
			t.Fatal("failed subscription setup without admitted deliveries did not permit fresh connections")
		}
	}
}

func TestPersistenceSupervisorRetiredTransportUsesFreshGeneration(t *testing.T) {
	old := &supervisorTestGroup{service: serviceForTest(t, newServiceStream())}
	fresh := &supervisorTestGroup{service: serviceForTest(t, newServiceStream())}
	var calls atomic.Int64
	s := supervisorForTest(t, func(context.Context) error { return nil }, func(context.Context) (PersistenceConsumerGroup, error) {
		if calls.Add(1) == 1 {
			return old, nil
		}
		return fresh, nil
	})
	w := supervisorControlledWait(s)
	supervisorStart(t, s)
	old.service.fail(errors.Join(
		persistenceServiceError(ErrPersistenceServiceStopped, "subscription closed", nil),
		persistenceServiceError(ErrPersistenceServiceStopped, "receive", rabbitmq.ErrConsumerUnavailable)))
	w.expect(t, s.options.BaseDelay, s.options.MaxDelay, 1)
	if calls.Load() != 1 || old.closes.Load() != 1 || s.ConsumerReady() {
		t.Fatal("transport retry overtook retirement or backoff")
	}
	w.next <- struct{}{}
	serviceEventually(t, s.ConsumerReady)
	if calls.Load() != 2 || s.Status().Generation != 2 || old.service.ConsumerReady() {
		t.Fatal("transport recovery reused a retired generation")
	}
}

func TestPersistenceSupervisorStopDuringConnectionSetupIsNotOperationalFailure(t *testing.T) {
	entered := make(chan struct{})
	s := supervisorForTest(t, func(context.Context) error { return nil }, func(ctx context.Context) (PersistenceConsumerGroup, error) {
		close(entered)
		<-ctx.Done()
		return nil, NewPersistenceConnectionError(ctx.Err())
	})
	setup, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := make(chan error, 1)
	go func() { started <- s.Start(setup, context.Background()) }()
	serviceAwait(t, entered)
	s.StopAdmission()
	if err := <-started; !errors.Is(err, context.Canceled) {
		t.Fatal("canceled activation succeeded", err)
	}
	if err := s.Close(context.Background()); err != nil || s.Err() != nil {
		t.Fatal("normal setup cancellation became a runtime failure", err)
	}
}
