package application

import (
	"context"
	"errors"
	"time"

	"iChat/database"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
)

var ErrQuarantineReplayInput = errors.New("quarantine audit cannot be replayed as a validated command")

type QuarantineReplayStore interface {
	Get(context.Context, string) (database.QuarantineDetail, error)
	RecordReplay(context.Context, database.QuarantineDetail, rabbitmq.PublishResult) error
}

type QuarantineReplaySelection struct {
	TransportID     string
	PreviousAttempt string
}

type QuarantineReplayResult struct {
	TransportID   string                  `json:"transport_id"`
	CommandID     string                  `json:"command_id,omitempty"`
	PayloadHash   string                  `json:"payload_hash,omitempty"`
	AttemptID     string                  `json:"replay_attempt_id,omitempty"`
	Publish       rabbitmq.PublishOutcome `json:"publish_outcome"`
	AttemptRecord string                  `json:"attempt_record"`
}

type quarantineReplayError struct {
	stage string
	cause error
}

func (e *quarantineReplayError) Error() string    { return "quarantine replay: " + e.stage }
func (e *quarantineReplayError) Unwrap() error    { return e.cause }
func (e *quarantineReplayError) GoString() string { return e.Error() }

// ReplayQuarantine performs exactly one publication. publish is invoked lazily,
// only after a fresh full audit read and selection/identity checks. An error
// after publication preserves the result so an operator can reconcile it.
func ReplayQuarantine(ctx context.Context, store QuarantineReplayStore, selection QuarantineReplaySelection, publish func(context.Context, messaging.Command, time.Time) (rabbitmq.PublishResult, error)) (result QuarantineReplayResult, err error) {
	result = QuarantineReplayResult{TransportID: selection.TransportID, Publish: rabbitmq.PublishNotStarted, AttemptRecord: "not_attempted"}
	if ctx == nil || store == nil || publish == nil || !database.ValidQuarantineTransportID(selection.TransportID) || (selection.PreviousAttempt != "" && !messaging.ValidPublishAttemptID(selection.PreviousAttempt)) {
		return result, ErrQuarantineReplayInput
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	detail, err := store.Get(ctx, selection.TransportID)
	if err != nil {
		return result, err
	}
	previous := ""
	if detail.Summary.ReplayAttemptID != nil {
		previous = *detail.Summary.ReplayAttemptID
	}
	if (detail.Summary.State != "open" && detail.Summary.State != "replay_pending") || detail.Summary.ResolvedAt != nil || previous != selection.PreviousAttempt {
		return result, database.ErrQuarantineReplayChanged
	}
	identity, identified := detail.Input.Identity()
	source, sourceErr := detail.Input.Envelope().Source()
	wire, decodeErr := messaging.DecodeEnvelope(source.Body)
	command, commandErr := messaging.ValidateEnvelope(wire)
	if !identified || sourceErr != nil || decodeErr != nil || commandErr != nil || detail.Summary.TransportID != selection.TransportID || detail.Input.Envelope().TransportID() != selection.TransportID ||
		identity.CommandID != command.CommandID || identity.PayloadHash != command.PayloadHash || identity.SenderID != command.SenderID || identity.ClientMessageID != command.ClientMessageID {
		return result, ErrQuarantineReplayInput
	}
	stamp, err := time.Parse(time.RFC3339Nano, wire.ReceivedAt)
	if err != nil {
		return result, ErrQuarantineReplayInput
	}
	result.CommandID, result.PayloadHash = command.CommandID, command.PayloadHash
	if err := ctx.Err(); err != nil {
		return result, err
	}
	receipt, publishErr := publish(ctx, command, stamp)
	result.Publish, result.AttemptID = receipt.Outcome, receipt.PublishAttemptID
	if publishErr != nil || receipt.Outcome != rabbitmq.PublishAccepted {
		if receipt.Outcome == rabbitmq.PublishAccepted || receipt.Outcome == "" {
			result.Publish = rabbitmq.PublishUnknown
		}
		return result, &quarantineReplayError{"publication not confirmed", publishErr}
	}
	if receipt.CommandID != command.CommandID || !messaging.ValidPublishAttemptID(receipt.PublishAttemptID) || receipt.PublishAttemptID == wire.PublishAttemptID || receipt.PublishAttemptID == previous || receipt.ConnectionEpoch == 0 || receipt.ChannelEpoch == 0 || receipt.DeliveryTag == 0 {
		result.Publish = rabbitmq.PublishUnknown
		return result, &quarantineReplayError{"invalid confirmation receipt", nil}
	}
	result.AttemptRecord = "unconfirmed"
	if err := store.RecordReplay(ctx, detail, receipt); err != nil {
		return result, &quarantineReplayError{"confirmed publication; audit update not confirmed", err}
	}
	result.AttemptRecord = "recorded"
	return result, nil
}
