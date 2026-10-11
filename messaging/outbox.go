package messaging

// Outbox payloads are durable internal events, not a grant to receive message
// content. The notification worker/gateway must apply current authorization.
type OutboxEventType string

const (
	EventMessagePersisted OutboxEventType = "message.persisted"
	EventCommandResult    OutboxEventType = "command.result"
)

// OutboxEventID uses the same length-prefixed encoding as command identities,
// with a separate v1 domain. An event's identity never includes retry metadata.
func OutboxEventID(commandID string, eventType OutboxEventType) (string, error) {
	if len(commandID) != 64 {
		return "", invalid("commandId")
	}
	for _, char := range commandID {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return "", invalid("commandId")
		}
	}
	if eventType != EventMessagePersisted && eventType != EventCommandResult {
		return "", invalid("eventType")
	}
	return digest("ichat:outbox:v1\x00", commandID, string(eventType)), nil
}

// MessagePersistedEvent is a hint. Message content is read from MySQL after
// checking the recipient's current permission, rather than broadcast here.
type MessagePersistedEvent struct {
	SchemaVersion  int        `json:"schemaVersion"`
	EventID        string     `json:"eventId"`
	CommandID      string     `json:"commandId"`
	ConversationID string     `json:"conversationId"`
	MessageID      string     `json:"messageId"`
	Seq            Sequence   `json:"seq"`
	SenderID       string     `json:"senderId"`
	TargetID       string     `json:"targetId"`
	TargetType     TargetType `json:"targetType"`
}

// CommandResultEvent belongs to the sender. Successful results retain the
// canonical message, including the first committed timestamp and sequence.
type CommandResultEvent struct {
	SchemaVersion   int         `json:"schemaVersion"`
	EventID         string      `json:"eventId"`
	CommandID       string      `json:"commandId"`
	SenderID        string      `json:"senderId"`
	ClientMessageID string      `json:"clientMessageId"`
	PayloadHash     string      `json:"payloadHash"`
	State           string      `json:"state"`
	ReasonCode      string      `json:"reasonCode,omitempty"`
	Message         *MessageDTO `json:"message,omitempty"`
}
