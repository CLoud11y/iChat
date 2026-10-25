package messaging

import (
	"encoding/json"
	"strconv"
)

// OutboxRecord is the immutable part of a committed claim. Nullable route
// columns must agree with the payload, including NULL on rejected commands.
type OutboxRecord struct {
	EventID, CommandID        string
	EventType                 OutboxEventType
	ConversationID, MessageID *string
	Payload                   []byte
}

// Notification is a validated, content-free hint for a future v1 gateway.
// It cannot be constructed with an arbitrary channel/body outside this package.
type Notification struct {
	channel string
	body    []byte
}

func (n Notification) Channel() string  { return n.channel }
func (n Notification) Body() []byte     { return append([]byte(nil), n.body...) }
func (n Notification) Valid() bool      { return n.channel != "" && len(n.body) != 0 }
func (Notification) String() string     { return "notification (body redacted)" }
func (n Notification) GoString() string { return n.String() }

type notificationHint struct {
	SchemaVersion   int             `json:"schemaVersion"`
	EventType       OutboxEventType `json:"eventType"`
	EventID         string          `json:"eventId"`
	CommandID       string          `json:"commandId"`
	ConversationID  string          `json:"conversationId,omitempty"`
	MessageID       string          `json:"messageId,omitempty"`
	Seq             *Sequence       `json:"seq,omitempty"`
	ClientMessageID string          `json:"clientMessageId,omitempty"`
	PayloadHash     string          `json:"payloadHash,omitempty"`
	State           string          `json:"state,omitempty"`
	ReasonCode      string          `json:"reasonCode,omitempty"`
}

// DecodeOutboxNotification verifies the stored business identities before
// deriving a route. Redis hints never contain content/media or grant access.
func DecodeOutboxNotification(r OutboxRecord) (Notification, error) {
	id, err := OutboxEventID(r.CommandID, r.EventType)
	if err != nil || id != r.EventID {
		return Notification{}, ErrIdentityMismatch
	}
	h := notificationHint{SchemaVersion: SchemaVersion, EventType: r.EventType, EventID: id, CommandID: r.CommandID}
	var channel string
	switch r.EventType {
	case EventMessagePersisted:
		var e MessagePersistedEvent
		if err := decodeObject(r.Payload, &e); err != nil {
			return Notification{}, err
		}
		if e.SchemaVersion != SchemaVersion || e.EventID != id || e.CommandID != r.CommandID ||
			e.MessageID != r.CommandID || e.Seq == 0 || !notificationRoute(e.SenderID, e.TargetID, e.TargetType, e.ConversationID) ||
			!sameRoute(r, e.ConversationID, e.MessageID) {
			return Notification{}, invalid("outbox hint")
		}
		h.ConversationID, h.MessageID, h.Seq = e.ConversationID, e.MessageID, &e.Seq
		channel = "ichat:notify:v1:user:" + e.TargetID
		if e.TargetType == Group {
			channel = "ichat:notify:v1:group:" + e.TargetID
		}
	case EventCommandResult:
		var e CommandResultEvent
		if err := decodeObject(r.Payload, &e); err != nil {
			return Notification{}, err
		}
		sender, err := canonicalDecimal(e.SenderID, false)
		q, queryErr := NormalizeStatusQuery(sender, e.ClientMessageID, e.PayloadHash)
		if err != nil || queryErr != nil || q.CommandID != r.CommandID || q.ClientMessageID != e.ClientMessageID ||
			e.SchemaVersion != SchemaVersion || e.EventID != id || e.CommandID != r.CommandID {
			return Notification{}, invalid("outbox result identity")
		}
		h.ClientMessageID, h.PayloadHash, h.State, h.ReasonCode = e.ClientMessageID, e.PayloadHash, e.State, e.ReasonCode
		switch e.State {
		case "persisted":
			if e.Message == nil || e.ReasonCode != "" {
				return Notification{}, invalid("outbox persisted result")
			}
			// The outer decoder is strict for flat objects; apply the same rules
			// to the nested message (encoding/json alone accepts aliases/duplicates).
			var raw map[string]json.RawMessage
			if json.Unmarshal(r.Payload, &raw) != nil || decodeObject(raw["message"], e.Message) != nil {
				return Notification{}, invalid("outbox message")
			}
			m := e.Message
			c, err := Normalize(sender, SendRequest{ClientMessageID: e.ClientMessageID, TargetType: m.TargetType, TargetID: m.TargetID, Media: m.Media, Content: m.Content})
			stamp, stampErr := normalizeTime(m.ServerCreatedAt)
			if err != nil || stampErr != nil || stamp == "" || stamp != m.ServerCreatedAt || m.Seq == 0 ||
				c.CommandID != r.CommandID || c.PayloadHash != e.PayloadHash || m.MessageID != r.CommandID ||
				m.ClientMessageID != e.ClientMessageID || m.SenderID != e.SenderID || m.TargetID != c.TargetID ||
				!notificationRoute(m.SenderID, m.TargetID, m.TargetType, m.ConversationID) || !sameRoute(r, m.ConversationID, m.MessageID) {
				return Notification{}, invalid("outbox canonical message")
			}
			h.ConversationID, h.MessageID, h.Seq = m.ConversationID, m.MessageID, &m.Seq
		case "rejected":
			if e.Message != nil || r.ConversationID != nil || r.MessageID != nil || !notificationReason(e.ReasonCode) {
				return Notification{}, invalid("outbox rejected result")
			}
		default:
			return Notification{}, invalid("outbox state")
		}
		channel = "ichat:notify:v1:user:" + e.SenderID
	default:
		return Notification{}, invalid("outbox type")
	}
	body, err := json.Marshal(h)
	if err != nil {
		return Notification{}, invalid("notification")
	}
	return Notification{channel: channel, body: body}, nil
}

func sameRoute(r OutboxRecord, conversation, message string) bool {
	return r.ConversationID != nil && r.MessageID != nil && *r.ConversationID == conversation && *r.MessageID == message
}
func notificationRoute(senderText, targetText string, kind TargetType, conversation string) bool {
	sender, a := canonicalDecimal(senderText, false)
	target, b := canonicalDecimal(targetText, false)
	if a != nil || b != nil {
		return false
	}
	if kind == Group {
		return conversation == "g:"+targetText
	}
	if kind != Private || sender == target {
		return false
	}
	if sender > target {
		sender, target = target, sender
	}
	return conversation == "p:"+strconv.FormatUint(sender, 10)+":"+strconv.FormatUint(target, 10)
}
func notificationReason(reason string) bool {
	switch reason {
	case "SENDER_NOT_FOUND", "TARGET_NOT_FOUND", "SELF_SEND_NOT_ALLOWED", "NOT_FRIENDS", "NOT_GROUP_MEMBER":
		return true
	default:
		return false
	}
}
