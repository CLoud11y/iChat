package workers

import (
	"context"
	"encoding/hex"
	"errors"
	"time"

	"iChat/database"
	"iChat/messaging/rabbitmq"

	mysql "github.com/go-sql-driver/mysql"
)

// QuarantinePublisher is borrowed. Its owner must retain it until every worker
// call has joined, including calls that outlive a runtime shutdown observer.
type QuarantinePublisher interface {
	Publish(context.Context, rabbitmq.QuarantineEnvelope) (rabbitmq.QuarantinePublishResult, error)
}

type PersistenceDisposition string

const (
	PersistenceTerminal    PersistenceDisposition = "terminal"
	PersistenceQuarantined PersistenceDisposition = "quarantined"
)

var ErrQuarantineReceipt = errors.New("quarantine publisher returned an invalid transfer receipt")

type PersistenceProcessResult struct {
	Disposition PersistenceDisposition
	Execution   database.CommandExecution
	// A valid target receipt can accompany an error if source ACK is unknown.
	Transfer rabbitmq.QuarantinePublishResult
}

// PersistenceProcessor composes SQL handling with explicit technical isolation.
// PersistenceHandler retains its original terminal-execution-only contract.
type PersistenceProcessor struct {
	handler   *PersistenceHandler
	publisher QuarantinePublisher
}

func NewPersistenceProcessor(repository *database.MessageRepository, retry PersistenceRetryOptions, publisher QuarantinePublisher) (*PersistenceProcessor, error) {
	handler, err := NewPersistenceHandlerWithRetry(repository, retry)
	if err != nil {
		return nil, err
	}
	return newPersistenceProcessor(handler, publisher)
}

func newPersistenceProcessor(handler *PersistenceHandler, publisher QuarantinePublisher) (*PersistenceProcessor, error) {
	if handler == nil || handler.persist == nil || nilPersistenceValue(publisher) {
		return nil, ErrPersistenceInput
	}
	return &PersistenceProcessor{handler: handler, publisher: publisher}, nil
}

func (p *PersistenceProcessor) Handle(ctx context.Context, delivery *rabbitmq.ConsumerDelivery) (PersistenceProcessResult, error) {
	if delivery == nil {
		return PersistenceProcessResult{}, ErrPersistenceInput
	}
	return p.process(ctx, delivery.Message, delivery.Ack)
}

func (p *PersistenceProcessor) process(ctx context.Context, message rabbitmq.ConsumerMessage, ack func(context.Context) error) (PersistenceProcessResult, error) {
	if p == nil || p.handler == nil || nilPersistenceValue(p.publisher) {
		return PersistenceProcessResult{}, ErrPersistenceInput
	}
	execution, failure := p.handler.handle(ctx, message, ack)
	if failure == nil {
		return PersistenceProcessResult{Disposition: PersistenceTerminal, Execution: execution}, nil
	}
	code := persistenceQuarantineCode(failure)
	if code == "" || ctx == nil || ctx.Err() != nil {
		return PersistenceProcessResult{}, failure
	}
	boundary := failure.(*PersistenceError) // The policy admits only direct handler boundaries.
	fail := func(kind PersistenceFailure, cause error) (PersistenceProcessResult, error) {
		return PersistenceProcessResult{}, &PersistenceError{Kind: kind, Attempts: boundary.Attempts, cause: errors.Join(failure, cause)}
	}
	snapshot, err := rabbitmq.NewQuarantineEnvelope(message, code, time.Now())
	if err != nil {
		return fail(QuarantineFailed, err)
	}
	if err := ctx.Err(); err != nil {
		return fail(QuarantineFailed, err)
	}
	receipt, err := p.publisher.Publish(ctx, snapshot)
	if err != nil {
		return fail(QuarantineFailed, err)
	}
	if !validQuarantineReceipt(snapshot.TransportID(), receipt) {
		return fail(QuarantineFailed, ErrQuarantineReceipt)
	}
	result := PersistenceProcessResult{Transfer: receipt}
	// Even a valid target receipt cannot authorize a source ACK after this
	// processing context expires. Keep the receipt and both recovery paths.
	if err := ctx.Err(); err != nil {
		_, failure := fail(AcknowledgementFailed, err)
		return result, failure
	}
	if err := ack(ctx); err != nil {
		_, failure := fail(AcknowledgementFailed, err)
		return result, failure
	}
	result.Disposition = PersistenceQuarantined
	return result, nil
}

func validQuarantineReceipt(transportID string, r rabbitmq.QuarantinePublishResult) bool {
	if r.Outcome != rabbitmq.PublishAccepted || r.TransportID != transportID || r.ConnectionEpoch == 0 || r.ChannelEpoch == 0 || r.DeliveryTag == 0 || len(r.PublishAttemptID) != 32 {
		return false
	}
	attempt, err := hex.DecodeString(r.PublishAttemptID)
	return err == nil && hex.EncodeToString(attempt) == r.PublishAttemptID
}

// This is deliberately narrower than FailureTechnical. A schema/deployment
// error or an invalid terminal result must not move an entire backlog away.
// Only the repository's completed rollback of known data constraints qualifies.
func persistenceQuarantineCode(err error) string {
	boundary, ok := err.(*PersistenceError)
	if !ok || boundary == nil || errors.Is(err, database.ErrCommandCommitUnknown) || errors.Is(err, database.ErrCommandRollbackUnknown) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ""
	}
	if boundary.Kind == InvalidDelivery {
		return "INVALID_DELIVERY"
	}
	if boundary.Kind == PersistenceFailed && boundary.Attempts > 0 && onlyQuarantineConstraints(boundary.cause) {
		return "DATA_CONSTRAINT"
	}
	return ""
}

func onlyQuarantineConstraints(err error) bool {
	if err == nil {
		return false
	}
	if server, ok := err.(*mysql.MySQLError); ok && server != nil {
		switch server.Number {
		case 1048, 1062, 1264, 1366, 1406, 1451, 1452, 3819:
			return true
		default:
			return false
		}
	}
	// A different semantic boundary is never just a diagnostic wrapper.
	if _, ok := err.(*PersistenceError); ok {
		return false
	}
	if multi, ok := err.(interface{ Unwrap() []error }); ok {
		children := multi.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !onlyQuarantineConstraints(child) {
				return false
			}
		}
		return true
	}
	if single, ok := err.(interface{ Unwrap() error }); ok {
		return onlyQuarantineConstraints(single.Unwrap())
	}
	return false
}
