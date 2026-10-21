package workers

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"iChat/database"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
)

func TestPersistenceQuarantineOwnership(t *testing.T) {
	for _, mode := range []string{"graceful", "forced", "source failure"} {
		t.Run(mode, func(t *testing.T) {
			stream := newServiceStream()
			group := &supervisorTestGroup{service: serviceForTest(t, stream)}
			entered, release := make(chan context.Context, 1), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			var active, acks, acquisitions atomic.Int64
			processor := dispositionProcessor(t, func(context.Context, messaging.Command) (database.CommandExecution, error) {
				t.Error("malformed transfer reached SQL")
				return database.CommandExecution{}, nil
			}, func(ctx context.Context, q rabbitmq.QuarantineEnvelope) (rabbitmq.QuarantinePublishResult, error) {
				active.Add(1)
				defer active.Add(-1)
				entered <- ctx
				<-release // A borrowed callback remains owned after cancellation.
				return dispositionReceipt(q), nil
			})
			group.service.handle = func(ctx context.Context, d *rabbitmq.ConsumerDelivery) error {
				_, err := processor.process(ctx, d.Message, func(context.Context) error {
					acks.Add(1)
					if mode == "source failure" {
						return rabbitmq.ErrConsumerUnavailable
					}
					return nil
				})
				return err
			}
			s := supervisorForTest(t, func(context.Context) error { return nil }, func(context.Context) (PersistenceConsumerGroup, error) {
				acquisitions.Add(1)
				return group, nil
			})
			supervisorStart(t, s)
			stream.next <- serviceNext{delivery: &rabbitmq.ConsumerDelivery{Message: rabbitmq.ConsumerMessage{Body: []byte("bad")}}}
			operation := <-entered
			if mode == "source failure" {
				group.service.fail(persistenceServiceError(ErrPersistenceServiceStopped, "storage", io.EOF))
				serviceEventually(t, func() bool { return s.Status().State == PersistenceStateRetiring })
			} else {
				s.StopAdmission()
				short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
				err := s.Drain(short)
				cancel()
				if !errors.Is(err, ErrPersistenceSupervisorPending) || operation.Err() != nil {
					t.Fatal("drain abandoned/cancelled transfer", err)
				}
			}
			if mode == "forced" {
				short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
				err := s.Close(short)
				cancel()
				if !errors.Is(err, ErrPersistenceSupervisorPending) || operation.Err() == nil {
					t.Fatal("force did not retain cancelled callback", err)
				}
			}
			if active.Load() != 1 || group.closes.Load() != 0 || persistenceDone(s.Done()) || acquisitions.Load() != 1 || acks.Load() != 0 {
				t.Fatal("borrowed publisher or old group was released before actual join")
			}
			once.Do(func() { close(release) })
			if mode == "graceful" {
				if err := s.Drain(context.Background()); err != nil {
					t.Fatal(err)
				}
				if err := s.Close(context.Background()); err != nil {
					t.Fatal(err)
				}
			} else {
				serviceAwait(t, s.Done())
			}
			wantACKs := int64(1)
			if mode == "forced" {
				wantACKs = 0
			}
			if acks.Load() != wantACKs || active.Load() != 0 || acquisitions.Load() != 1 || group.closes.Load() != 1 {
				t.Fatal("late target completion revived recovery or lost exact group ownership")
			}
			if mode != "graceful" && ClassifyPersistenceFailure(s.Err()) != FailureAcknowledgement {
				t.Fatal("source/target phase responsibility lost", s.Err())
			}
		})
	}
}

func TestPersistenceQuarantineShutdownErrorsKeepPhase(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, io.EOF} {
		failure := &PersistenceError{Kind: QuarantineFailed, cause: cause}
		err := persistenceServiceError(ErrPersistenceServiceStopped, "process", failure)
		if supervisorCancellationOnly(err) || persistenceRecoveryAllowed(err) || ClassifyPersistenceFailure(err) != FailureQuarantine {
			t.Fatal("transfer failure became clean shutdown or SQL recovery", err)
		}
	}
}
