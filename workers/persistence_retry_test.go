package workers

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"iChat/database"
	"iChat/messaging"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"
)

func TestPersistenceFailureClassification(t *testing.T) {
	deadlock := &mysql.MySQLError{Number: 1213, Message: "private deadlock diagnostics"}
	lockWait := &mysql.MySQLError{Number: 1205, Message: "private lock diagnostics"}
	for _, tc := range []struct {
		name string
		err  error
		want PersistenceFailureClass
	}{
		{"none", nil, FailureNone},
		{"wrapped_deadlock", fmt.Errorf("repository: %w", deadlock), FailureTransactionRetryable},
		{"lock_wait", lockWait, FailureTransactionRetryable},
		{"joined_conflicts", errors.Join(deadlock, lockWait), FailureTransactionRetryable},
		{"exhaustion", errors.Join(ErrPersistenceRetriesExhausted, deadlock), FailureTransactionRetryable},
		{"service_exhaustion", persistenceServiceError(ErrPersistenceServiceStopped, "process", &PersistenceError{Kind: PersistenceFailed, cause: errors.Join(ErrPersistenceRetriesExhausted, deadlock)}), FailureTransactionRetryable},
		{"mixed_unknown", errors.Join(deadlock, errors.New("unknown diagnostic")), FailureUnclassified},
		{"mixed_network", errors.Join(deadlock, io.ErrUnexpectedEOF), FailureInfrastructure},
		{"mixed_constraint", errors.Join(deadlock, &mysql.MySQLError{Number: 1062}), FailureTechnical},
		{"mixed_invariant", errors.Join(deadlock, database.ErrCommandInvariant), FailureTechnical},
		{"network", &net.OpError{Op: "read", Net: "tcp", Err: io.EOF}, FailureInfrastructure},
		{"driver_bad_conn", driver.ErrBadConn, FailureInfrastructure},
		{"mysql_invalid_conn", mysql.ErrInvalidConn, FailureInfrastructure},
		{"server_capacity", &mysql.MySQLError{Number: 1040}, FailureInfrastructure},
		{"server_access", &mysql.MySQLError{Number: 1045}, FailureInfrastructure},
		{"server_shutdown", &mysql.MySQLError{Number: 1053}, FailureInfrastructure},
		{"server_connection_state", &mysql.MySQLError{Number: 9999, SQLState: [5]byte{'0', '8', 'S', '0', '1'}}, FailureInfrastructure},
		{"unknown_server", &mysql.MySQLError{Number: 9999}, FailureUnclassified},
		{"no_text_matching", errors.New("Error 1213 deadlock retryable database unavailable"), FailureUnclassified},
		{"cancelled", errors.Join(deadlock, context.Canceled), FailureCancelled},
		{"deadline", context.DeadlineExceeded, FailureCancelled},
		{"commit_unknown", errors.Join(deadlock, database.ErrCommandCommitUnknown), FailureCommitUnknown},
		{"rollback_unknown", errors.Join(deadlock, database.ErrCommandRollbackUnknown), FailureRollbackUnknown},
		{"commit_before_cancel", errors.Join(database.ErrCommandCommitUnknown, context.Canceled), FailureCommitUnknown},
		{"ack_is_not_transaction", &PersistenceError{Kind: AcknowledgementFailed, cause: deadlock}, FailureAcknowledgement},
		{"joined_ack_boundary", errors.Join(&PersistenceError{Kind: PersistenceFailed, cause: deadlock}, &PersistenceError{Kind: AcknowledgementFailed, cause: deadlock}), FailureAcknowledgement},
		{"ack_cancel", &PersistenceError{Kind: AcknowledgementFailed, cause: context.Canceled}, FailureAcknowledgement},
		{"wire", &PersistenceError{Kind: InvalidDelivery, cause: messaging.ErrInvalidInput}, FailureMalformed},
		{"terminal", &PersistenceError{Kind: InvalidTerminalResult, cause: database.ErrCommandInvariant}, FailureTechnical},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyPersistenceFailure(tc.err); got != tc.want {
				t.Fatalf("classification=%s want=%s", got, tc.want)
			}
		})
	}
}

func TestPersistenceRetryKeepsIdentityBudgetAndSingleAck(t *testing.T) {
	command, message := persistenceMessage(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var calls, waits, acks int
	h := &PersistenceHandler{retry: DefaultPersistenceRetryOptions()}
	h.persist = func(gotCtx context.Context, got messaging.Command) (database.CommandExecution, error) {
		calls++
		if gotCtx != ctx || !reflect.DeepEqual(got, command) || acks != 0 {
			t.Fatal("identity/budget changed or ACK occurred before terminal success")
		}
		switch calls {
		case 1:
			return persistenceExecution(command, database.CommandPersisted), &mysql.MySQLError{Number: 1213}
		case 2:
			return database.CommandExecution{}, &mysql.MySQLError{Number: 1205}
		}
		return persistenceExecution(command, database.CommandPersisted), nil
	}
	h.wait = func(gotCtx context.Context, delay time.Duration) error {
		waits++
		if gotCtx != ctx || delay <= 0 || delay > h.retry.MaxDelay || acks != 0 || calls != waits {
			t.Fatal("backoff lost the processing budget or ownership")
		}
		return nil
	}
	execution, err := h.handle(ctx, message, func(context.Context) error { acks++; return nil })
	if err != nil || execution.Disposition != database.CommandApplied || calls != 3 || waits != 2 || acks != 1 {
		t.Fatal("transaction retry did not complete exactly one local handoff", err, calls, waits, acks)
	}
}

func TestPersistenceRetryExhaustionRetainsTransactionClassification(t *testing.T) {
	_, message := persistenceMessage(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conflict := &mysql.MySQLError{Number: 1213, Message: "private payload/SQL"}
	calls, waits := 0, 0
	h := &PersistenceHandler{persist: func(context.Context, messaging.Command) (database.CommandExecution, error) {
		calls++
		return database.CommandExecution{}, conflict
	}, wait: func(context.Context, time.Duration) error { waits++; return nil }}
	execution, err := h.handle(ctx, message, func(context.Context) error { t.Error("exhausted delivery ACKed"); return nil })
	requirePersistenceFailure(t, execution, err, PersistenceFailed)
	var failure *PersistenceError
	if !errors.As(err, &failure) || failure.Attempts != 3 || calls != 3 || waits != 2 ||
		!errors.Is(err, conflict) || !errors.Is(err, ErrPersistenceRetriesExhausted) || ClassifyPersistenceFailure(err) != FailureTransactionRetryable {
		t.Fatal("exhaustion lost original conflict or attempt bound", err)
	}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		if strings.Contains(fmt.Sprintf(format, err), "private") {
			t.Fatal("retry diagnostics exposed by ordinary logging")
		}
	}
}

func TestPersistenceRetryNeverRepeatsUncertainOrUnclassifiedFailures(t *testing.T) {
	_, message := persistenceMessage(t)
	conflict := &mysql.MySQLError{Number: 1213}
	for _, cause := range []error{
		io.ErrUnexpectedEOF, errors.Join(conflict, database.ErrCommandRollbackUnknown),
		errors.Join(conflict, database.ErrCommandCommitUnknown), errors.Join(conflict, io.EOF),
		errors.Join(conflict, errors.New("unknown outcome")), &mysql.MySQLError{Number: 1062},
		context.Canceled, database.ErrCommandInvariant,
	} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		calls := 0
		h := &PersistenceHandler{persist: func(context.Context, messaging.Command) (database.CommandExecution, error) {
			calls++
			return database.CommandExecution{}, cause
		}, wait: func(context.Context, time.Duration) error { t.Error("nonretryable error reached backoff"); return nil }}
		execution, err := h.handle(ctx, message, func(context.Context) error { t.Error("failed delivery ACKed"); return nil })
		cancel()
		if !reflect.DeepEqual(execution, database.CommandExecution{}) || !errors.Is(err, cause) || calls != 1 || errors.Is(err, ErrPersistenceRetriesExhausted) {
			t.Fatal("nonretryable cause was repeated, lost or mislabeled", err)
		}
	}
}

func TestPersistenceRetryCancellationAndDeadlineBoundBackoff(t *testing.T) {
	_, message := persistenceMessage(t)
	for _, cancelInWait := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		calls := 0
		h := &PersistenceHandler{retry: PersistenceRetryOptions{MaxAttempts: 5, BaseDelay: time.Second, MaxDelay: time.Second},
			persist: func(context.Context, messaging.Command) (database.CommandExecution, error) {
				calls++
				return database.CommandExecution{}, &mysql.MySQLError{Number: 1205}
			}}
		if cancelInWait {
			h.wait = func(got context.Context, delay time.Duration) error {
				cancel()
				return waitPersistenceRetry(got, delay)
			}
		}
		execution, err := h.handle(ctx, message, func(context.Context) error { t.Error("cancelled retry ACKed"); return nil })
		cancel()
		requirePersistenceFailure(t, execution, err, PersistenceFailed)
		if calls != 1 || ClassifyPersistenceFailure(err) != FailureCancelled || errors.Is(err, ErrPersistenceRetriesExhausted) {
			t.Fatal("processing budget was renewed or cancellation became exhaustion", err)
		}
	}
}

func TestPersistenceRetryDoesNotRetryAckEvenWhenCauseLooksTransient(t *testing.T) {
	command, message := persistenceMessage(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	calls, acks := 0, 0
	h := &PersistenceHandler{persist: func(context.Context, messaging.Command) (database.CommandExecution, error) {
		calls++
		return persistenceExecution(command, database.CommandPersisted), nil
	}}
	execution, err := h.handle(ctx, message, func(context.Context) error { acks++; return &mysql.MySQLError{Number: 1213} })
	requirePersistenceFailure(t, execution, err, AcknowledgementFailed)
	if calls != 1 || acks != 1 || ClassifyPersistenceFailure(err) != FailureAcknowledgement {
		t.Fatal("ACK ambiguity entered the transaction retry loop")
	}
}

func TestPersistenceRetryStopsWhenFailureClassChanges(t *testing.T) {
	_, message := persistenceMessage(t)
	for _, tc := range []struct {
		err  error
		want PersistenceFailureClass
	}{
		{io.EOF, FailureInfrastructure},
		{errors.Join(database.ErrCommandCommitUnknown, &mysql.MySQLError{Number: 1213}), FailureCommitUnknown},
		{errors.Join(database.ErrCommandRollbackUnknown, &mysql.MySQLError{Number: 1205}), FailureRollbackUnknown},
		{&mysql.MySQLError{Number: 1062}, FailureTechnical},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		calls, waits := 0, 0
		h := &PersistenceHandler{persist: func(context.Context, messaging.Command) (database.CommandExecution, error) {
			calls++
			if calls == 1 {
				return database.CommandExecution{}, &mysql.MySQLError{Number: 1213}
			}
			return database.CommandExecution{}, tc.err
		}, wait: func(context.Context, time.Duration) error { waits++; return nil }}
		execution, err := h.handle(ctx, message, func(context.Context) error { t.Error("changed failure class ACKed"); return nil })
		cancel()
		var failure *PersistenceError
		if !errors.As(err, &failure) || failure.Attempts != 2 || calls != 2 || waits != 1 || execution.Result != nil ||
			ClassifyPersistenceFailure(err) != tc.want || errors.Is(err, ErrPersistenceRetriesExhausted) || !errors.Is(err, tc.err) {
			t.Fatal("retry continued or mislabeled the changed failure class", err)
		}
	}
}

func TestPersistenceRetryOptionsAndJitterBounds(t *testing.T) {
	for _, o := range []PersistenceRetryOptions{
		{MaxAttempts: 6, BaseDelay: time.Millisecond, MaxDelay: time.Second},
		{MaxAttempts: 0, BaseDelay: time.Millisecond, MaxDelay: time.Second},
		{MaxAttempts: 3, BaseDelay: 0, MaxDelay: time.Second},
		{MaxAttempts: 3, BaseDelay: time.Second, MaxDelay: time.Millisecond},
		{MaxAttempts: 3, BaseDelay: time.Second, MaxDelay: time.Minute},
	} {
		if _, err := resolvePersistenceRetry(o); !errors.Is(err, ErrPersistenceInput) {
			t.Fatal("invalid retry policy accepted")
		}
		serviceOptions := DefaultPersistenceOptions()
		serviceOptions.Retry = o
		if err := ValidatePersistenceOptions(serviceOptions); !errors.Is(err, ErrPersistenceServiceInput) {
			t.Fatal("service accepted invalid retry policy")
		}
	}
	o, err := resolvePersistenceRetry(PersistenceRetryOptions{})
	if err != nil || o != DefaultPersistenceRetryOptions() {
		t.Fatal("zero policy did not preserve defaults")
	}
	for attempt := 1; attempt <= 5; attempt++ {
		ceiling := min(o.BaseDelay*time.Duration(1<<(attempt-1)), o.MaxDelay)
		for i := 0; i < 50; i++ {
			delay := o.delay(attempt)
			if delay < ceiling/2 || delay > ceiling || delay == 0 {
				t.Fatal("jitter exceeded its exponential cap")
			}
		}
	}
	command, message := persistenceMessage(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	calls := 0
	h := &PersistenceHandler{retry: PersistenceRetryOptions{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
		persist: func(context.Context, messaging.Command) (database.CommandExecution, error) {
			calls++
			return persistenceExecution(command, database.CommandPersisted), &mysql.MySQLError{Number: 1213}
		}}
	h.handle(ctx, message, func(context.Context) error { t.Error("disabled retries ACKed failure"); return nil })
	if calls != 1 {
		t.Fatal("MaxAttempts=1 did not disable retries")
	}
}
