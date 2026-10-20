package workers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"iChat/database"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"

	mysql "github.com/go-sql-driver/mysql"
	amqp "github.com/rabbitmq/amqp091-go"
)

type dispositionPublisher func(context.Context, rabbitmq.QuarantineEnvelope) (rabbitmq.QuarantinePublishResult, error)

func (f dispositionPublisher) Publish(ctx context.Context, q rabbitmq.QuarantineEnvelope) (rabbitmq.QuarantinePublishResult, error) {
	return f(ctx, q)
}

func dispositionReceipt(q rabbitmq.QuarantineEnvelope) rabbitmq.QuarantinePublishResult {
	return rabbitmq.QuarantinePublishResult{Outcome: rabbitmq.PublishAccepted, TransportID: q.TransportID(),
		PublishAttemptID: "0123456789abcdef0123456789abcdef", ConnectionEpoch: 1, ChannelEpoch: 1, DeliveryTag: 1}
}

func dispositionProcessor(t *testing.T, persist func(context.Context, messaging.Command) (database.CommandExecution, error), publisher dispositionPublisher) *PersistenceProcessor {
	t.Helper()
	h := &PersistenceHandler{persist: persist, wait: func(context.Context, time.Duration) error { return nil }}
	p, err := newPersistenceProcessor(h, publisher)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func dispositionContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestPersistenceDispositionWaitsForTargetBeforeSourceACK(t *testing.T) {
	message := rabbitmq.ConsumerMessage{Body: []byte{0xff, 0, '{'}, Headers: amqp.Table{"nested": amqp.Table{"bytes": []byte{0, 255}}},
		Exchange: rabbitmq.CommandsExchange, RoutingKey: rabbitmq.SubmitRoutingKey, DeliveryTag: 99}
	var sqlCalls, ackCalls atomic.Int64
	entered, release := make(chan rabbitmq.QuarantineEnvelope, 1), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	confirmed := atomic.Bool{}
	p := dispositionProcessor(t, func(context.Context, messaging.Command) (database.CommandExecution, error) {
		sqlCalls.Add(1)
		return database.CommandExecution{}, nil
	},
		func(ctx context.Context, q rabbitmq.QuarantineEnvelope) (rabbitmq.QuarantinePublishResult, error) {
			entered <- q
			<-release
			confirmed.Store(true)
			return dispositionReceipt(q), nil
		})
	type call struct {
		result PersistenceProcessResult
		err    error
	}
	done := make(chan call, 1)
	go func() {
		r, err := p.process(dispositionContext(t), message, func(context.Context) error {
			if !confirmed.Load() {
				return errors.New("source acknowledged before target")
			}
			ackCalls.Add(1)
			return nil
		})
		done <- call{r, err}
	}()
	q := <-entered
	if q.ErrorCode() != "INVALID_DELIVERY" {
		t.Fatal("wrong controlled reason")
	}
	got, err := q.Source()
	if err != nil || !reflect.DeepEqual(got, message) {
		t.Fatal("original snapshot changed", err)
	}
	if sqlCalls.Load() != 0 || ackCalls.Load() != 0 {
		t.Fatal("malformed record reached SQL or premature ACK")
	}
	once.Do(func() { close(release) })
	result := <-done
	if result.err != nil || result.result.Disposition != PersistenceQuarantined || result.result.Execution.Result != nil || result.result.Transfer.TransportID != q.TransportID() || ackCalls.Load() != 1 {
		t.Fatal("quarantine disposition lost handoff", result.err)
	}
}

func TestPersistenceDispositionTerminalDelegatesWithoutIsolation(t *testing.T) {
	command, message := persistenceMessage(t)
	for _, state := range []database.CommandState{database.CommandPersisted, database.CommandRejected} {
		t.Run(string(state), func(t *testing.T) {
			var published, acks int
			p := dispositionProcessor(t, func(context.Context, messaging.Command) (database.CommandExecution, error) {
				return persistenceExecution(command, state), nil
			},
				func(context.Context, rabbitmq.QuarantineEnvelope) (rabbitmq.QuarantinePublishResult, error) {
					published++
					return rabbitmq.QuarantinePublishResult{}, nil
				})
			r, err := p.process(dispositionContext(t), message, func(context.Context) error { acks++; return nil })
			if err != nil || r.Disposition != PersistenceTerminal || r.Execution.Result.State != state || published != 0 || acks != 1 || r.Transfer.TransportID != "" {
				t.Fatal("terminal result changed", err)
			}
		})
	}
}

func TestPersistenceDispositionConstraintPolicyAndUnsafeFailures(t *testing.T) {
	_, message := persistenceMessage(t)
	constraint := &mysql.MySQLError{Number: 3819, Message: "private SQL diagnostic"}
	tests := []struct {
		name       string
		failure    error
		quarantine bool
		attempts   int
	}{
		{"constraint", constraint, true, 1},
		{"wrapped constraint", fmt.Errorf("repository: %w", constraint), true, 1},
		{"two constraints", errors.Join(constraint, &mysql.MySQLError{Number: 1406}), true, 1},
		{"schema", &mysql.MySQLError{Number: 1146}, false, 1},
		{"syntax", &mysql.MySQLError{Number: 1064}, false, 1},
		{"connection", io.EOF, false, 1},
		{"commit unknown", errors.Join(database.ErrCommandCommitUnknown, constraint), false, 1},
		{"rollback unknown", errors.Join(database.ErrCommandRollbackUnknown, constraint), false, 1},
		{"constraint with connection", errors.Join(constraint, io.EOF), false, 1},
		{"constraint with unknown", errors.Join(constraint, errors.New("unknown")), false, 1},
		{"deadlock exhaustion", &mysql.MySQLError{Number: 1213}, false, 3},
		{"invariant", database.ErrCommandInvariant, false, 1},
		{"unknown", errors.New("unknown diagnostic"), false, 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls, published, acks := 0, 0, 0
			p := dispositionProcessor(t, func(context.Context, messaging.Command) (database.CommandExecution, error) {
				calls++
				return database.CommandExecution{}, test.failure
			},
				func(_ context.Context, q rabbitmq.QuarantineEnvelope) (rabbitmq.QuarantinePublishResult, error) {
					published++
					if q.ErrorCode() != "DATA_CONSTRAINT" {
						t.Error("wrong code")
					}
					return dispositionReceipt(q), nil
				})
			r, err := p.process(dispositionContext(t), message, func(context.Context) error { acks++; return nil })
			if test.quarantine {
				if err != nil || r.Disposition != PersistenceQuarantined || published != 1 || acks != 1 {
					t.Fatal("eligible constraint not safely transferred", err)
				}
			} else if err == nil || published != 0 || acks != 0 || r.Disposition != "" {
				t.Fatal("unsafe failure isolated", err)
			}
			if calls != test.attempts {
				t.Fatalf("repository attempts=%d want=%d", calls, test.attempts)
			}
		})
	}
	// A committed but invalid result and a post-commit ACK failure are not
	// record constraints, even if their error tree contains a constraint.
	command, message := persistenceMessage(t)
	published := 0
	p := dispositionProcessor(t, func(context.Context, messaging.Command) (database.CommandExecution, error) {
		return database.CommandExecution{}, nil
	},
		func(context.Context, rabbitmq.QuarantineEnvelope) (rabbitmq.QuarantinePublishResult, error) {
			published++
			return rabbitmq.QuarantinePublishResult{}, nil
		})
	if _, err := p.process(dispositionContext(t), message, func(context.Context) error { t.Fatal("invalid terminal ACK"); return nil }); err == nil || published != 0 {
		t.Fatal("invalid terminal isolated")
	}
	p.handler.persist = func(context.Context, messaging.Command) (database.CommandExecution, error) {
		return persistenceExecution(command, database.CommandPersisted), nil
	}
	_, err := p.process(dispositionContext(t), message, func(context.Context) error { return constraint })
	if ClassifyPersistenceFailure(err) != FailureAcknowledgement || published != 0 {
		t.Fatal("source ACK failure isolated")
	}
}

func TestPersistenceDispositionRejectsTargetFailuresAndInvalidReceipts(t *testing.T) {
	message := rabbitmq.ConsumerMessage{Body: []byte("private invalid JSON")}
	tests := []struct {
		name   string
		mutate func(*rabbitmq.QuarantinePublishResult)
		err    error
	}{
		{"unknown", func(r *rabbitmq.QuarantinePublishResult) { r.Outcome = rabbitmq.PublishUnknown }, nil},
		{"accepted with error", func(*rabbitmq.QuarantinePublishResult) {}, io.EOF},
		{"wrong transport", func(r *rabbitmq.QuarantinePublishResult) { r.TransportID = "other" }, nil},
		{"empty epoch", func(r *rabbitmq.QuarantinePublishResult) { r.ConnectionEpoch = 0 }, nil},
		{"empty channel", func(r *rabbitmq.QuarantinePublishResult) { r.ChannelEpoch = 0 }, nil},
		{"empty tag", func(r *rabbitmq.QuarantinePublishResult) { r.DeliveryTag = 0 }, nil},
		{"wrong attempt", func(r *rabbitmq.QuarantinePublishResult) { r.PublishAttemptID = strings.Repeat("Z", 32) }, nil},
		{"uppercase attempt", func(r *rabbitmq.QuarantinePublishResult) { r.PublishAttemptID = strings.Repeat("A", 32) }, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := dispositionProcessor(t, func(context.Context, messaging.Command) (database.CommandExecution, error) {
				t.Fatal("SQL for bad body")
				return database.CommandExecution{}, nil
			},
				func(_ context.Context, q rabbitmq.QuarantineEnvelope) (rabbitmq.QuarantinePublishResult, error) {
					r := dispositionReceipt(q)
					test.mutate(&r)
					return r, test.err
				})
			r, err := p.process(dispositionContext(t), message, func(context.Context) error { t.Fatal("invalid transfer ACKed source"); return nil })
			if err == nil || r.Disposition != "" || ClassifyPersistenceFailure(err) != FailureQuarantine || persistenceRecoveryAllowed(err) {
				t.Fatal("target failure changed ownership", err)
			}
			var boundary *PersistenceError
			if !errors.As(err, &boundary) || boundary.Kind != QuarantineFailed {
				t.Fatal("missing phase boundary")
			}
			for _, format := range []string{"%v", "%+v", "%#v"} {
				if strings.Contains(fmt.Sprintf(format, err), "private") {
					t.Fatal("body/diagnostic leaked")
				}
			}
		})
	}
}

func TestPersistenceDispositionCancellationACKAndSnapshotFailure(t *testing.T) {
	for _, mode := range []string{"precancel", "after confirm cancel", "ack failure", "snapshot failure"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(dispositionContext(t))
			defer cancel()
			message := rabbitmq.ConsumerMessage{Body: []byte{0xff, 0}}
			if mode == "snapshot failure" {
				message.Body = make([]byte, rabbitmq.MaxQuarantineBytes+1)
			}
			published, acks := 0, 0
			p := dispositionProcessor(t, func(context.Context, messaging.Command) (database.CommandExecution, error) {
				t.Fatal("SQL for malformed body")
				return database.CommandExecution{}, nil
			},
				func(_ context.Context, q rabbitmq.QuarantineEnvelope) (rabbitmq.QuarantinePublishResult, error) {
					published++
					if mode == "after confirm cancel" {
						cancel()
					}
					return dispositionReceipt(q), nil
				})
			if mode == "precancel" {
				cancel()
			}
			r, err := p.process(ctx, message, func(context.Context) error { acks++; return io.EOF })
			if err == nil || r.Disposition != "" {
				t.Fatal("failure declared settled")
			}
			if mode == "after confirm cancel" || mode == "ack failure" {
				if published != 1 || r.Transfer.Outcome != rabbitmq.PublishAccepted || ClassifyPersistenceFailure(err) != FailureAcknowledgement || persistenceRecoveryAllowed(err) {
					t.Fatal("confirmed target proof discarded", err)
				}
			} else if published != 0 || acks != 0 {
				t.Fatal("pretransfer failure wrote")
			}
			if mode == "ack failure" && acks != 1 || mode != "ack failure" && acks != 0 {
				t.Fatal("wrong ACK count")
			}
		})
	}
}

func TestPersistenceDispositionDrainAndForcedJoinRetainBorrowedPublisher(t *testing.T) {
	stream := newServiceStream()
	s := serviceForTest(t, stream)
	entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var acks atomic.Int64
	p := dispositionProcessor(t, func(context.Context, messaging.Command) (database.CommandExecution, error) {
		return database.CommandExecution{}, nil
	},
		func(ctx context.Context, q rabbitmq.QuarantineEnvelope) (rabbitmq.QuarantinePublishResult, error) {
			close(entered)
			<-ctx.Done()
			close(cancelled)
			<-release
			return dispositionReceipt(q), nil
		})
	s.handle = func(ctx context.Context, d *rabbitmq.ConsumerDelivery) error {
		_, err := p.process(ctx, d.Message, func(context.Context) error { acks.Add(1); return nil })
		return err
	}
	defer serviceSetup(t, s)()
	stream.next <- serviceNext{delivery: &rabbitmq.ConsumerDelivery{Message: rabbitmq.ConsumerMessage{Body: []byte("bad")}}}
	serviceAwait(t, entered)
	s.StopAdmission()
	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err := s.Drain(short)
	cancel()
	if !errors.Is(err, ErrPersistenceServicePending) {
		t.Fatal("drain abandoned transfer", err)
	}
	select {
	case <-cancelled:
		t.Fatal("graceful drain cancelled admitted transfer")
	default:
	}
	short, cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
	err = s.Close(short)
	cancel()
	serviceAwait(t, cancelled)
	if !errors.Is(err, ErrPersistenceServicePending) || persistenceDone(s.Done()) || stream.closeCalls.Load() != 0 {
		t.Fatal("pending publisher treated as joined", err)
	}
	once.Do(func() { close(release) })
	serviceAwait(t, s.Done())
	if acks.Load() != 0 || ClassifyPersistenceFailure(s.Err()) != FailureAcknowledgement || stream.closeCalls.Load() != 1 {
		t.Fatal("late target confirmation revived ACK or lost join", s.Err())
	}
}

func TestPersistenceDispositionPureConstructionAndPolicyGuards(t *testing.T) {
	var typedNil dispositionPublisher
	options := DefaultPersistenceOptions()
	options.QuarantinePublisher = typedNil
	if ValidatePersistenceOptions(options) == nil {
		t.Fatal("typed nil publisher accepted")
	}
	if _, err := newPersistenceProcessor(&PersistenceHandler{persist: func(context.Context, messaging.Command) (database.CommandExecution, error) {
		return database.CommandExecution{}, nil
	}}, typedNil); err == nil {
		t.Fatal("nil dependency accepted")
	}
	constraint := &mysql.MySQLError{Number: 3819}
	for _, failure := range []error{constraint, errors.Join(&PersistenceError{Kind: InvalidDelivery}, io.EOF),
		&PersistenceError{Kind: PersistenceFailed, cause: constraint}, &PersistenceError{Kind: InvalidTerminalResult, cause: constraint},
		&PersistenceError{Kind: PersistenceFailed, Attempts: 1, cause: errors.Join(constraint, &PersistenceError{Kind: AcknowledgementFailed, cause: constraint})}} {
		if persistenceQuarantineCode(failure) != "" {
			t.Fatal("policy admitted a non-handler or mixed boundary")
		}
	}
}
