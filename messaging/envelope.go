package messaging

import "time"

// NewEnvelope must be called for each physical publication. Attempt identity
// and reception time never participate in command identity or payload hash.
func NewEnvelope(command Command, receivedAt time.Time) (Envelope, error) {
	if err := ValidateCommand(command); err != nil {
		return Envelope{}, err
	}
	stamp, err := timestamp(receivedAt)
	if err != nil {
		return Envelope{}, invalid("receivedAt")
	}
	attempt, err := newAttemptID()
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{
		SchemaVersion: command.SchemaVersion, CommandID: command.CommandID,
		ClientMessageID: command.ClientMessageID, PublishAttemptID: attempt,
		SenderID: command.SenderID, TargetType: command.TargetType, TargetID: command.TargetID,
		Media: command.Media, Content: command.Content, PayloadHash: command.PayloadHash,
		ReceivedAt: stamp, ClientSentAt: command.ClientSentAt,
	}, nil
}

// ValidateEnvelope recomputes all business identities. Metadata may differ on
// retry but must still have canonical syntax. This is validation, not proof of
// the sender's authentication: only trusted publishers may write the broker.
func ValidateEnvelope(envelope Envelope) (Command, error) {
	if envelope.SchemaVersion != SchemaVersion {
		return Command{}, ErrUnsupportedVersion
	}
	attempt, err := normalizeUUID(envelope.PublishAttemptID)
	if err != nil || attempt != envelope.PublishAttemptID {
		return Command{}, invalid("publishAttemptId")
	}
	received, err := normalizeTime(envelope.ReceivedAt)
	if err != nil || received == "" || received != envelope.ReceivedAt {
		return Command{}, invalid("receivedAt")
	}
	sender, err := decimal(envelope.SenderID, false)
	if err != nil {
		return Command{}, invalid("senderId")
	}
	command, err := Normalize(sender, SendRequest{
		ClientMessageID: envelope.ClientMessageID, TargetType: envelope.TargetType,
		TargetID: envelope.TargetID, Media: envelope.Media, Content: envelope.Content, ClientSentAt: envelope.ClientSentAt,
	})
	if err != nil {
		return Command{}, err
	}
	if command.CommandID != envelope.CommandID || command.PayloadHash != envelope.PayloadHash ||
		command.SenderID != envelope.SenderID || command.TargetID != envelope.TargetID ||
		command.ClientMessageID != envelope.ClientMessageID || command.ClientSentAt != envelope.ClientSentAt {
		return Command{}, ErrIdentityMismatch
	}
	return command, nil
}
