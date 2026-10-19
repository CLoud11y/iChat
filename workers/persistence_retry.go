package workers

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"iChat/database"
	"iChat/messaging"
	"io"
	"math/rand/v2"
	"net"
	"time"

	mysql "github.com/go-sql-driver/mysql"
)

type PersistenceFailureClass string

const (
	FailureNone                 PersistenceFailureClass = ""
	FailureMalformed            PersistenceFailureClass = "malformed_delivery"
	FailureTransactionRetryable PersistenceFailureClass = "transaction_retryable"
	FailureInfrastructure       PersistenceFailureClass = "infrastructure"
	FailureCommitUnknown        PersistenceFailureClass = "commit_unknown"
	FailureRollbackUnknown      PersistenceFailureClass = "rollback_unknown"
	FailureCancelled            PersistenceFailureClass = "cancelled"
	FailureAcknowledgement      PersistenceFailureClass = "acknowledgement"
	FailureTechnical            PersistenceFailureClass = "technical"
	FailureUnclassified         PersistenceFailureClass = "unclassified"
)

var ErrPersistenceRetriesExhausted = errors.New("persistence transaction retries exhausted")

// Classification is conservative: only an error tree containing exclusively
// known transaction conflicts permits local retry. None of these classes alone
// authorizes ACK, Reject or quarantine transfer. In particular, exhaustion is
// still a transaction conflict, never evidence that the payload is poison.
func ClassifyPersistenceFailure(err error) PersistenceFailureClass {
	if err == nil {
		return FailureNone
	}
	if errors.Is(err, database.ErrCommandCommitUnknown) {
		return FailureCommitUnknown
	}
	if errors.Is(err, database.ErrCommandRollbackUnknown) {
		return FailureRollbackUnknown
	}
	cancelled := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
	var commit, acknowledgement, malformed bool
	var retryable, infrastructure, technical, unknown bool
	var visit func(error)
	visit = func(current error) {
		if current == nil || current == ErrPersistenceRetriesExhausted {
			return
		}
		if connection, ok := current.(*persistenceConnectionFailure); ok && connection != nil {
			infrastructure = true
			return
		}
		if service, ok := current.(*persistenceServiceFailure); ok && service != nil {
			// Lifecycle tags describe where the failure was observed; they are
			// not additional SQL evidence that changes a delivery's classification.
			if service.cause == nil {
				unknown = true
			} else {
				visit(service.cause)
			}
			return
		}
		if boundary, ok := current.(*PersistenceError); ok && boundary != nil {
			switch boundary.Kind {
			case CommitUnknown:
				commit = true
				return
			case AcknowledgementFailed:
				acknowledgement = true
				return
			case InvalidDelivery:
				malformed = true
				return
			case InvalidTerminalResult:
				technical = true
				return
			}
		}
		switch current {
		case driver.ErrBadConn, mysql.ErrInvalidConn, sql.ErrConnDone, io.EOF, io.ErrUnexpectedEOF, net.ErrClosed, database.ErrCommandRepositoryUnavailable:
			infrastructure = true
			return
		case database.ErrCommandInvariant, database.ErrInvalidCommandDecision, database.ErrConversationInvariant, database.ErrConversationExhausted, ErrPersistenceInput:
			technical = true
			return
		}
		if _, ok := current.(net.Error); ok {
			infrastructure = true
			return
		}
		if server, ok := current.(*mysql.MySQLError); ok && server != nil {
			switch server.Number {
			case 1205, 1213: // Lock wait timeout / deadlock; repository rolled back.
				retryable = true
			case 1040, 1042, 1043, 1044, 1045, 1049, 1053, 1129, 1130, 1203:
				infrastructure = true // Capacity, connection, access or deployment.
			case 1048, 1054, 1062, 1064, 1146, 1264, 1366, 1406, 1451, 1452, 3819:
				technical = true // Known SQL/schema/constraint failures, not business rejections.
			default:
				if server.SQLState[0] == '0' && server.SQLState[1] == '8' {
					infrastructure = true
				} else {
					unknown = true
				}
			}
			return
		}
		if multi, ok := current.(interface{ Unwrap() []error }); ok {
			children := multi.Unwrap()
			if len(children) == 0 {
				unknown = true
			}
			for _, child := range children {
				visit(child)
			}
			return
		}
		if single, ok := current.(interface{ Unwrap() error }); ok && single.Unwrap() != nil {
			visit(single.Unwrap())
			return
		}
		unknown = true
	}
	visit(err)
	switch {
	case commit:
		return FailureCommitUnknown
	case acknowledgement:
		return FailureAcknowledgement
	case cancelled:
		return FailureCancelled
	case infrastructure:
		return FailureInfrastructure
	case malformed:
		return FailureMalformed
	case technical:
		return FailureTechnical
	case unknown:
		return FailureUnclassified
	case retryable:
		return FailureTransactionRetryable
	default:
		return FailureUnclassified
	}
}

type PersistenceRetryOptions struct {
	MaxAttempts         int // Includes the first repository call; 1 disables retries.
	BaseDelay, MaxDelay time.Duration
}

func DefaultPersistenceRetryOptions() PersistenceRetryOptions {
	return PersistenceRetryOptions{MaxAttempts: 3, BaseDelay: 25 * time.Millisecond, MaxDelay: 250 * time.Millisecond}
}

func resolvePersistenceRetry(o PersistenceRetryOptions) (PersistenceRetryOptions, error) {
	if o == (PersistenceRetryOptions{}) {
		o = DefaultPersistenceRetryOptions()
	}
	if o.MaxAttempts < 1 || o.MaxAttempts > 5 || o.BaseDelay <= 0 || o.MaxDelay < o.BaseDelay || o.MaxDelay > 5*time.Second {
		return PersistenceRetryOptions{}, ErrPersistenceInput
	}
	return o, nil
}

func (o PersistenceRetryOptions) delay(attempt int) time.Duration {
	ceiling := o.BaseDelay
	for i := 1; i < attempt && ceiling < o.MaxDelay; i++ {
		ceiling = min(2*ceiling, o.MaxDelay)
	}
	// Equal jitter: bounded away from a zero-delay retry, at most the cap.
	floor := max(time.Nanosecond, ceiling/2)
	return floor + time.Duration(rand.Int64N(int64(ceiling-floor)+1))
}

func waitPersistenceRetry(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

// Each attempt finishes its repository-owned transaction before waiting or
// trying again. All attempts and backoff share the original processing budget;
// command identity, physical delivery and its ACK handle never change.
func (h *PersistenceHandler) persistWithRetry(ctx context.Context, command messaging.Command) (database.CommandExecution, int, error) {
	o, err := resolvePersistenceRetry(h.retry)
	if err != nil {
		return database.CommandExecution{}, 0, err
	}
	wait := h.wait
	if wait == nil {
		wait = waitPersistenceRetry
	}
	var last error
	for attempt := 1; attempt <= o.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return database.CommandExecution{}, attempt - 1, errors.Join(last, err)
		}
		execution, err := h.persist(ctx, command)
		if err == nil {
			return execution, attempt, nil
		}
		last = err
		if err := ctx.Err(); err != nil {
			return database.CommandExecution{}, attempt, errors.Join(last, err)
		}
		if ClassifyPersistenceFailure(last) != FailureTransactionRetryable {
			return database.CommandExecution{}, attempt, last
		}
		if attempt == o.MaxAttempts {
			return database.CommandExecution{}, attempt, errors.Join(ErrPersistenceRetriesExhausted, last)
		}
		if err := wait(ctx, o.delay(attempt)); err != nil {
			return database.CommandExecution{}, attempt, errors.Join(last, err)
		}
	}
	panic("validated persistence retry count is unreachable")
}
