package workers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"iChat/database"
	"iChat/messaging/rabbitmq"

	mysql "github.com/go-sql-driver/mysql"
)

func quarantineWorkerMessage(t *testing.T) rabbitmq.ConsumerMessage {
	t.Helper()
	q, err := rabbitmq.NewQuarantineEnvelope(rabbitmq.ConsumerMessage{Body: []byte{0xff, 0, '{'}, MessageID: "private-original", CorrelationID: "source-attempt", Exchange: rabbitmq.CommandsExchange, RoutingKey: rabbitmq.SubmitRoutingKey}, "INVALID_DELIVERY", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return rabbitmq.ConsumerMessage{Body: q.Encode(), ContentType: rabbitmq.QuarantineContentType, Type: rabbitmq.QuarantineMessageType, DeliveryMode: 2, MessageID: q.TransportID(), CorrelationID: "0123456789abcdef0123456789abcdef", Exchange: rabbitmq.QuarantineExchange, RoutingKey: rabbitmq.QuarantineRoutingKey}
}

func TestQuarantineHandlerAuditBeforeAck(t *testing.T) {
	message := quarantineWorkerMessage(t)
	for _, disposition := range []database.QuarantineAuditDisposition{database.QuarantineInserted, database.QuarantineExisting} {
		t.Run(string(disposition), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var saved, acked bool
			h := &QuarantineHandler{now: time.Now, save: func(_ context.Context, input rabbitmq.QuarantineInput) (database.QuarantineAuditResult, error) {
				if acked {
					t.Fatal("ACK preceded audit")
				}
				source, err := input.Envelope().Source()
				if err != nil || !strings.Contains(source.MessageID, "private-original") {
					t.Fatal("lost source", err)
				}
				saved = true
				return database.QuarantineAuditResult{TransportID: input.Envelope().TransportID(), Disposition: disposition}, nil
			}}
			result, err := h.handle(ctx, message, func(context.Context) error {
				if !saved || acked {
					t.Fatal("audit/ACK order invalid")
				}
				acked = true
				return nil
			})
			if err != nil || result.Disposition != disposition || !acked {
				t.Fatal(result, err)
			}
		})
	}
}

func TestQuarantineHandlerFailuresRetainDelivery(t *testing.T) {
	for _, stage := range []string{"parse", "save", "commit unknown", "rollback unknown", "wrong identity", "wrong disposition", "cancel after save", "ack"} {
		t.Run(stage, func(t *testing.T) {
			message := quarantineWorkerMessage(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			cause := errors.New("private SQL data and credentials")
			acks, saves := 0, 0
			h := &QuarantineHandler{now: time.Now, save: func(_ context.Context, input rabbitmq.QuarantineInput) (database.QuarantineAuditResult, error) {
				saves++
				switch stage {
				case "save":
					return database.QuarantineAuditResult{}, cause
				case "commit unknown":
					return database.QuarantineAuditResult{}, errors.Join(database.ErrQuarantineCommitUnknown, io.EOF)
				case "rollback unknown":
					return database.QuarantineAuditResult{}, errors.Join(database.ErrQuarantineRollbackUnknown, io.EOF)
				}
				result := database.QuarantineAuditResult{TransportID: input.Envelope().TransportID(), Disposition: database.QuarantineInserted}
				switch stage {
				case "wrong identity":
					result.TransportID = "forged"
				case "wrong disposition":
					result.Disposition = "success"
				case "cancel after save":
					cancel()
				}
				return result, nil
			}}
			if stage == "parse" {
				message.MessageID = "forged"
			}
			result, err := h.handle(ctx, message, func(context.Context) error { acks++; return cause })
			if err == nil || result != (database.QuarantineAuditResult{}) || acks != boolToQuarantineInt(stage == "ack") {
				t.Fatal("failure settled delivery", result, err, acks)
			}
			if stage == "parse" && saves != 0 {
				t.Fatal("invalid input reached SQL")
			}
			for _, format := range []string{"%v", "%+v", "%#v"} {
				if strings.Contains(fmt.Sprintf(format, err), "private") {
					t.Fatal("failure leaked payload")
				}
			}
		})
	}
	if _, err := NewQuarantineHandler(nil); !errors.Is(err, ErrQuarantineHandlerInput) {
		t.Fatal(err)
	}
	h := &QuarantineHandler{now: time.Now, save: func(context.Context, rabbitmq.QuarantineInput) (database.QuarantineAuditResult, error) {
		t.Fatal("unbounded operation reached storage")
		return database.QuarantineAuditResult{}, nil
	}}
	if _, err := h.handle(context.Background(), quarantineWorkerMessage(t), func(context.Context) error { return nil }); !errors.Is(err, ErrQuarantineHandlerInput) {
		t.Fatal(err)
	}
	if _, err := h.Handle(context.Background(), nil); !errors.Is(err, ErrQuarantineHandlerInput) {
		t.Fatal(err)
	}
}
func boolToQuarantineInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestQuarantineRecoveryRequiresCompleteEvidence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		allowed bool
	}{
		{"storage offline", &QuarantineError{QuarantineAuditFailed, io.EOF}, true},
		{"commit unknown", &QuarantineError{QuarantineAuditFailed, errors.Join(database.ErrQuarantineCommitUnknown, io.EOF)}, true},
		{"rollback unknown", &QuarantineError{QuarantineAuditFailed, errors.Join(database.ErrQuarantineRollbackUnknown, io.EOF)}, true},
		{"storage timeout", &QuarantineError{QuarantineAuditFailed, context.DeadlineExceeded}, true},
		{"constraint", &QuarantineError{QuarantineAuditFailed, &mysql.MySQLError{Number: 3819}}, false},
		{"invariant", &QuarantineError{QuarantineAuditFailed, database.ErrQuarantineInvariant}, false},
		{"conflict", &QuarantineError{QuarantineAuditFailed, database.ErrQuarantineConflict}, false},
		{"mixed", &QuarantineError{QuarantineAuditFailed, errors.Join(io.EOF, database.ErrQuarantineConflict)}, false},
		{"unknown mixed", &QuarantineError{QuarantineAuditFailed, errors.Join(database.ErrQuarantineCommitUnknown, io.EOF, errors.New("unclassified"))}, false},
		{"invalid input", &QuarantineError{QuarantineInputInvalid, io.EOF}, false},
		{"bad receipt", &QuarantineError{QuarantineReceiptInvalid, io.EOF}, false},
		{"ack failure", &QuarantineError{QuarantineAcknowledgementFailed, io.EOF}, false},
		{"cancel", &QuarantineError{QuarantineAuditFailed, context.Canceled}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := persistenceRecoveryAllowed(persistenceServiceError(ErrPersistenceServiceStopped, "process", tc.err)); got != tc.allowed {
				t.Fatal(got, tc.err)
			}
		})
	}
	if ClassifyPersistenceFailure(&QuarantineError{QuarantineAuditFailed, errors.Join(database.ErrQuarantineCommitUnknown, io.EOF)}) != FailureCommitUnknown {
		t.Fatal("unknown outcome lost")
	}
	if ClassifyPersistenceFailure(&QuarantineError{QuarantineAcknowledgementFailed, io.EOF}) != FailureAcknowledgement {
		t.Fatal("ACK classified as DB fault")
	}
}

func TestQuarantineServiceDrainAndPendingRetainAudit(t *testing.T) {
	for _, forced := range []bool{false, true} {
		t.Run(fmt.Sprint(forced), func(t *testing.T) {
			stream := newServiceStream()
			core := serviceForTest(t, stream)
			service := &QuarantineService{core}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			var acks atomic.Int64
			h := &QuarantineHandler{now: time.Now, save: func(_ context.Context, input rabbitmq.QuarantineInput) (database.QuarantineAuditResult, error) {
				close(entered)
				<-release
				return database.QuarantineAuditResult{TransportID: input.Envelope().TransportID(), Disposition: database.QuarantineInserted}, nil
			}}
			core.handle = func(ctx context.Context, d *rabbitmq.ConsumerDelivery) error {
				_, err := h.handle(ctx, d.Message, func(context.Context) error { acks.Add(1); return nil })
				return err
			}
			setup := serviceSetup(t, core)
			setup()
			stream.next <- serviceNext{delivery: &rabbitmq.ConsumerDelivery{Message: quarantineWorkerMessage(t)}}
			serviceAwait(t, entered)
			service.StopAdmission()
			short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()
			var err error
			if forced {
				err = service.Close(short)
			} else {
				err = service.Drain(short)
			}
			if !errors.Is(err, ErrPersistenceServicePending) || persistenceDone(service.Done()) || stream.closeCalls.Load() != 0 || acks.Load() != 0 || service.ConsumerReady() {
				t.Fatal("pending released borrowed audit work", err)
			}
			once.Do(func() { close(release) })
			wait, done := context.WithTimeout(context.Background(), time.Second)
			defer done()
			if !forced {
				if err := service.Drain(wait); err != nil {
					t.Fatal(err)
				}
			}
			_ = service.Close(wait)
			serviceAwait(t, service.Done())
			if acks.Load() != int64(boolToQuarantineInt(!forced)) || stream.closeCalls.Load() != 1 {
				t.Fatal("late audit result disposition incorrect", acks.Load(), stream.closeCalls.Load())
			}
		})
	}
}

func TestQuarantineServiceConstructionValidation(t *testing.T) {
	pool, err := sql.Open("mysql", "")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := NewQuarantineService(nil, nil, DefaultQuarantineOptions()); !errors.Is(err, ErrQuarantineServiceInput) {
		t.Fatal(err)
	}
	if _, err := NewQuarantineService(pool, nil, DefaultQuarantineOptions()); !errors.Is(err, ErrQuarantineServiceInput) {
		t.Fatal(err)
	}
	bad := DefaultQuarantineOptions()
	bad.ProcessingTimeout = 0
	if ValidateQuarantineOptions(bad) == nil {
		t.Fatal("zero deadline accepted")
	}
	if _, err := NewQuarantineSupervisor(pool, nil, DefaultQuarantineSupervisorOptions()); !errors.Is(err, ErrQuarantineServiceInput) {
		t.Fatal(err)
	}
}
