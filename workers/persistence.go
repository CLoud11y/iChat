// Package workers adapts durable transports to the reliable message repository.
package workers

import (
	"context"
	"errors"
	"iChat/database"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
	"strconv"
)

var ErrPersistenceInput = errors.New("persistence requires a handler, delivery and bounded context")

type PersistenceFailure string

const (
	InvalidDelivery       PersistenceFailure = "invalid_delivery"
	PersistenceFailed     PersistenceFailure = "persistence_failed"
	CommitUnknown         PersistenceFailure = "commit_unknown"
	InvalidTerminalResult PersistenceFailure = "invalid_terminal_result"
	AcknowledgementFailed PersistenceFailure = "acknowledgement_failed"
)

// PersistenceError classifies a failed handoff without including body, headers,
// SQL or connection details in normal logging. Unwrap retains the original
// error for errors.Is/As and controlled diagnostics. Every failure leaves
// recovery responsibility with the original delivery/channel owner.
type PersistenceError struct {
	Kind  PersistenceFailure
	cause error
}

func (e *PersistenceError) Error() string    { return "message persistence: " + string(e.Kind) }
func (e *PersistenceError) GoString() string { return e.Error() }
func (e *PersistenceError) Unwrap() error    { return e.cause }

// PersistenceHandler processes exactly one already-received delivery. It does
// not own the repository pool, stream, retries, isolation or worker lifecycle.
// In particular it never calls Next, Reject or retries an ACK. Its caller must
// stop taking deliveries on error and retain/close the original channel before
// recovery; immediately requeueing in a loop can exhaust broker delivery limits.
type PersistenceHandler struct {
	persist func(context.Context, messaging.Command) (database.CommandExecution, error)
}

func NewPersistenceHandler(repository *database.MessageRepository) (*PersistenceHandler, error) {
	if repository == nil {
		return nil, ErrPersistenceInput
	}
	return &PersistenceHandler{persist: repository.PersistCommand}, nil
}

// Handle validates the wire message, executes P2's complete idempotent
// transaction, then writes one ACK using the original channel-bound handle.
// A nil error only proves the local ACK write completed, not that the broker
// received it. A later redelivery must go through the same repository again.
// On any error the returned execution is empty, including commit/ACK ambiguity.
func (h *PersistenceHandler) Handle(ctx context.Context, delivery *rabbitmq.ConsumerDelivery) (database.CommandExecution, error) {
	if delivery == nil {
		return database.CommandExecution{}, ErrPersistenceInput
	}
	return h.handle(ctx, delivery.Message, delivery.Ack)
}

func (h *PersistenceHandler) handle(ctx context.Context, message rabbitmq.ConsumerMessage, ack func(context.Context) error) (database.CommandExecution, error) {
	if h == nil || h.persist == nil || ctx == nil || ack == nil {
		return database.CommandExecution{}, ErrPersistenceInput
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return database.CommandExecution{}, ErrPersistenceInput
	}
	if err := ctx.Err(); err != nil {
		return database.CommandExecution{}, err
	}
	command, err := decodePersistenceMessage(message)
	if err != nil {
		return database.CommandExecution{}, &PersistenceError{Kind: InvalidDelivery, cause: err}
	}
	execution, err := h.persist(ctx, command)
	if err != nil {
		kind := PersistenceFailed
		if errors.Is(err, database.ErrCommandCommitUnknown) {
			kind = CommitUnknown
		}
		return database.CommandExecution{}, &PersistenceError{Kind: kind, cause: err}
	}
	if !terminalExecution(command, execution) {
		return database.CommandExecution{}, &PersistenceError{Kind: InvalidTerminalResult, cause: database.ErrCommandInvariant}
	}
	if err := ctx.Err(); err != nil {
		return database.CommandExecution{}, &PersistenceError{Kind: AcknowledgementFailed, cause: err}
	}
	if err := ack(ctx); err != nil {
		return database.CommandExecution{}, &PersistenceError{Kind: AcknowledgementFailed, cause: err}
	}
	return execution, nil
}

func decodePersistenceMessage(message rabbitmq.ConsumerMessage) (messaging.Command, error) {
	if message.ContentType != "application/json" || message.ContentEncoding != "" || message.DeliveryMode != 2 ||
		message.Exchange != rabbitmq.CommandsExchange || message.RoutingKey != rabbitmq.SubmitRoutingKey {
		return messaging.Command{}, messaging.ErrInvalidInput
	}
	envelope, err := messaging.DecodeEnvelope(message.Body)
	if err != nil {
		return messaging.Command{}, err
	}
	if message.MessageID != envelope.CommandID || message.CorrelationID != envelope.PublishAttemptID {
		return messaging.Command{}, messaging.ErrIdentityMismatch
	}
	return messaging.ValidateEnvelope(envelope)
}

// Reject a zero/partial or contradictory result before releasing MQ ownership.
// P2 remains responsible for the SQL invariants and complete message/Outbox set.
func terminalExecution(command messaging.Command, execution database.CommandExecution) bool {
	if execution.Disposition == database.CommandConflict {
		return execution.Result == nil
	}
	if execution.Disposition != database.CommandApplied && execution.Disposition != database.CommandReplayed {
		return false
	}
	r := execution.Result
	if r == nil || strconv.FormatUint(r.SenderID, 10) != command.SenderID || r.ClientMessageID != command.ClientMessageID ||
		r.CommandID != command.CommandID || r.PayloadHash != command.PayloadHash || r.CreatedAt.IsZero() || r.CompletedAt.IsZero() {
		return false
	}
	switch r.State {
	case database.CommandPersisted:
		return r.MessageID == command.CommandID && r.ConversationID == command.ConversationID && r.ReasonCode == ""
	case database.CommandRejected:
		if r.MessageID != "" || r.ConversationID != "" || len(r.ReasonCode) == 0 || len(r.ReasonCode) > 64 ||
			r.ReasonCode[0] < 'A' || r.ReasonCode[0] > 'Z' {
			return false
		}
		for _, c := range r.ReasonCode {
			if (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' {
				return false
			}
		}
		return true
	}
	return false
}
