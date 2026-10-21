package workers

import (
	"context"
	"errors"
	"time"

	"iChat/database"
	"iChat/messaging/rabbitmq"
)

var ErrQuarantineHandlerInput = errors.New("invalid quarantine handler input")

type QuarantineFailure string

const (
	QuarantineInputInvalid          QuarantineFailure = "invalid_audit_input"
	QuarantineAuditFailed           QuarantineFailure = "audit_failed"
	QuarantineReceiptInvalid        QuarantineFailure = "invalid_audit_receipt"
	QuarantineAcknowledgementFailed QuarantineFailure = "audit_ack_failed"
)

type QuarantineError struct {
	Kind  QuarantineFailure
	cause error
}

func (e *QuarantineError) Error() string    { return "quarantine worker: " + string(e.Kind) }
func (e *QuarantineError) GoString() string { return e.Error() }
func (e *QuarantineError) Unwrap() error    { return e.cause }

// QuarantineHandler owns neither storage nor MQ. The original delivery handle
// alone supplies ACK capability. Failed/unknown saves never settle a delivery.
type QuarantineHandler struct {
	save func(context.Context, rabbitmq.QuarantineInput) (database.QuarantineAuditResult, error)
	now  func() time.Time
}

func NewQuarantineHandler(repository *database.QuarantineRepository) (*QuarantineHandler, error) {
	if repository == nil {
		return nil, ErrQuarantineHandlerInput
	}
	return &QuarantineHandler{save: repository.Save, now: time.Now}, nil
}

func (h *QuarantineHandler) Handle(ctx context.Context, delivery *rabbitmq.ConsumerDelivery) (database.QuarantineAuditResult, error) {
	if delivery == nil {
		return database.QuarantineAuditResult{}, ErrQuarantineHandlerInput
	}
	return h.handle(ctx, delivery.Message, delivery.Ack)
}

func (h *QuarantineHandler) handle(ctx context.Context, message rabbitmq.ConsumerMessage, ack func(context.Context) error) (database.QuarantineAuditResult, error) {
	if h == nil || h.save == nil || h.now == nil || ctx == nil || ack == nil {
		return database.QuarantineAuditResult{}, ErrQuarantineHandlerInput
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return database.QuarantineAuditResult{}, ErrQuarantineHandlerInput
	}
	if err := ctx.Err(); err != nil {
		return database.QuarantineAuditResult{}, err
	}
	input, err := rabbitmq.ParseQuarantineInput(message, h.now())
	if err != nil {
		return database.QuarantineAuditResult{}, &QuarantineError{QuarantineInputInvalid, err}
	}
	result, err := h.save(ctx, input)
	if err != nil {
		return database.QuarantineAuditResult{}, &QuarantineError{QuarantineAuditFailed, err}
	}
	if result.TransportID != input.Envelope().TransportID() || (result.Disposition != database.QuarantineInserted && result.Disposition != database.QuarantineExisting) {
		return database.QuarantineAuditResult{}, &QuarantineError{QuarantineReceiptInvalid, database.ErrQuarantineInvariant}
	}
	if err := ctx.Err(); err != nil {
		return database.QuarantineAuditResult{}, &QuarantineError{QuarantineAcknowledgementFailed, err}
	}
	if err := ack(ctx); err != nil {
		return database.QuarantineAuditResult{}, &QuarantineError{QuarantineAcknowledgementFailed, err}
	}
	return result, nil
}
