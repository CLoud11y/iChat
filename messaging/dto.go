// Package messaging defines the versioned, infrastructure-independent message
// protocol. Legacy models and HTTP handlers do not use this protocol yet.
package messaging

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

const (
	SchemaVersion   = 1
	MaxContentRunes = 1024
	MaxBodyBytes    = 16384
)

type TargetType string
type Media string

const (
	Private TargetType = "private"
	Group   TargetType = "group"
	Text    Media      = "text"
	Picture Media      = "picture"
	Audio   Media      = "audio"
)

var (
	ErrInvalidInput       = errors.New("invalid message input")
	ErrUnsupportedVersion = errors.New("unsupported message schema version")
	ErrIdentityMismatch   = errors.New("message identity or payload hash mismatch")
)

func invalid(field string) error { return fmt.Errorf("%w: %s", ErrInvalidInput, field) }

// SendRequest intentionally has no sender or server-derived identity fields.
// DecodeRequest rejects unknown fields, including attempts to supply senderId.
type SendRequest struct {
	ClientMessageID string     `json:"clientMessageId"`
	TargetType      TargetType `json:"targetType"`
	TargetID        string     `json:"targetId"`
	Media           Media      `json:"media"`
	Content         string     `json:"content"`
	ClientSentAt    string     `json:"clientSentAt,omitempty"`
}

// Command contains canonical business data. Revalidate it at repository trust
// boundaries; exported fields are values, not a guarantee of prior validation.
type Command struct {
	SchemaVersion   int
	CommandID       string
	ClientMessageID string
	SenderID        string
	TargetType      TargetType
	TargetID        string
	Media           Media
	Content         string
	PayloadHash     string
	ConversationID  string
	ClientSentAt    string
}

type Envelope struct {
	SchemaVersion    int        `json:"schemaVersion"`
	CommandID        string     `json:"commandId"`
	ClientMessageID  string     `json:"clientMessageId"`
	PublishAttemptID string     `json:"publishAttemptId"`
	SenderID         string     `json:"senderId"`
	TargetType       TargetType `json:"targetType"`
	TargetID         string     `json:"targetId"`
	Media            Media      `json:"media"`
	Content          string     `json:"content"`
	PayloadHash      string     `json:"payloadHash"`
	ReceivedAt       string     `json:"receivedAt"`
	ClientSentAt     string     `json:"clientSentAt,omitempty"`
}

// Sequence is uint64 internally and always a canonical decimal JSON string.
// Zero is valid for a synchronization cursor, but not for a persisted message.
type Sequence uint64

func (s Sequence) String() string               { return strconv.FormatUint(uint64(s), 10) }
func (s Sequence) MarshalJSON() ([]byte, error) { return []byte(`"` + s.String() + `"`), nil }
func (s *Sequence) UnmarshalJSON(body []byte) error {
	var value string
	if err := json.Unmarshal(body, &value); err != nil {
		return invalid("seq")
	}
	n, err := decimal(value, true)
	if err != nil || strconv.FormatUint(n, 10) != value {
		return invalid("seq")
	}
	*s = Sequence(n)
	return nil
}

type MessageDTO struct {
	MessageID       string     `json:"messageId"`
	ClientMessageID string     `json:"clientMessageId"`
	ConversationID  string     `json:"conversationId"`
	Seq             Sequence   `json:"seq"`
	SenderID        string     `json:"senderId"`
	TargetID        string     `json:"targetId"`
	TargetType      TargetType `json:"targetType"`
	Media           Media      `json:"media"`
	Content         string     `json:"content"`
	ServerCreatedAt string     `json:"serverCreatedAt"`
}

// NewMessageDTO is called only after persistence supplies a committed sequence
// and server timestamp. It does not allocate sequence numbers or persist data.
func NewMessageDTO(command Command, seq Sequence, createdAt time.Time) (MessageDTO, error) {
	if err := ValidateCommand(command); err != nil {
		return MessageDTO{}, err
	}
	if seq == 0 {
		return MessageDTO{}, invalid("seq")
	}
	stamp, err := timestamp(createdAt)
	if err != nil {
		return MessageDTO{}, invalid("serverCreatedAt")
	}
	return MessageDTO{
		MessageID: command.CommandID, ClientMessageID: command.ClientMessageID,
		ConversationID: command.ConversationID, Seq: seq, SenderID: command.SenderID,
		TargetID: command.TargetID, TargetType: command.TargetType, Media: command.Media,
		Content: command.Content, ServerCreatedAt: stamp,
	}, nil
}
