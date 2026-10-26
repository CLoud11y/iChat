package messaging

import (
	"encoding/json"
	"strconv"
	"time"
)

type CommandResultData struct {
	ClientMessageID string      `json:"clientMessageId"`
	PayloadHash     string      `json:"payloadHash"`
	State           string      `json:"state"`
	ReasonCode      string      `json:"reasonCode,omitempty"`
	MessageID       string      `json:"messageId,omitempty"`
	ConversationID  string      `json:"conversationId,omitempty"`
	Seq             *Sequence   `json:"seq,omitempty"`
	Message         *MessageDTO `json:"message,omitempty"`
}

// ServerFrame can only be constructed by typed validators. Bytes returns a
// copy; ordinary formatting never exposes its message content.
type ServerFrame struct {
	kind, eventID string
	body          []byte
	hint          notificationHint
}

func (f ServerFrame) Valid() bool      { return f.kind != "" && len(f.body) > 0 }
func (f ServerFrame) Type() string     { return f.kind }
func (f ServerFrame) EventID() string  { return f.eventID }
func (f ServerFrame) Bytes() []byte    { return append([]byte(nil), f.body...) }
func (ServerFrame) String() string     { return "server frame (body redacted)" }
func (f ServerFrame) GoString() string { return f.String() }
func (f ServerFrame) Matches(h NotificationHint) bool {
	if !f.Valid() || !h.Valid() || f.eventID != h.EventID() {
		return false
	}
	v := f.hint
	return v.EventType == h.EventType() && v.CommandID == h.CommandID() && v.ConversationID == h.ConversationID() && v.MessageID == h.MessageID() && sequenceValue(v.Seq) == h.Sequence() && v.ClientMessageID == h.ClientMessageID() && v.PayloadHash == h.PayloadHash() && v.State == h.State() && v.ReasonCode == h.ReasonCode()
}
func sequenceValue(seq *Sequence) Sequence {
	if seq == nil {
		return 0
	}
	return *seq
}
func canonicalFrameMessage(m MessageDTO) (Command, error) {
	if !notificationRoute(m.SenderID, m.TargetID, m.TargetType, m.ConversationID) {
		return Command{}, invalid("message route")
	}
	sender, e := canonicalDecimal(m.SenderID, false)
	if e != nil {
		return Command{}, e
	}
	c, e := Normalize(sender, SendRequest{ClientMessageID: m.ClientMessageID, TargetType: m.TargetType, TargetID: m.TargetID, Media: m.Media, Content: m.Content})
	if e != nil {
		return Command{}, e
	}
	stamp, e := time.Parse(time.RFC3339Nano, m.ServerCreatedAt)
	if e != nil {
		return Command{}, invalid("serverCreatedAt")
	}
	expected, e := NewMessageDTO(c, m.Seq, stamp)
	if e != nil || expected != m {
		return Command{}, invalid("canonical message")
	}
	return c, nil
}
func NewMessageFrame(m MessageDTO) (ServerFrame, error) {
	c, e := canonicalFrameMessage(m)
	if e != nil {
		return ServerFrame{}, e
	}
	h := notificationHint{EventType: EventMessagePersisted, CommandID: c.CommandID, ConversationID: m.ConversationID, MessageID: m.MessageID, Seq: &m.Seq}
	return makeServerFrame("message", h, m)
}
func NewCommandResultFrame(sender uint64, d CommandResultData) (ServerFrame, error) {
	q, e := NormalizeStatusQuery(sender, d.ClientMessageID, d.PayloadHash)
	if e != nil || q.ClientMessageID != d.ClientMessageID {
		return ServerFrame{}, invalid("command result")
	}
	h := notificationHint{EventType: EventCommandResult, CommandID: q.CommandID, ClientMessageID: d.ClientMessageID, PayloadHash: d.PayloadHash, State: d.State, ReasonCode: d.ReasonCode}
	switch d.State {
	case "rejected":
		if !notificationReason(d.ReasonCode) || d.Message != nil || d.MessageID != "" || d.ConversationID != "" || d.Seq != nil {
			return ServerFrame{}, invalid("rejected result")
		}
	case "persisted":
		conversation, e := ParseConversationID(d.ConversationID)
		if e != nil || d.MessageID != q.CommandID || d.Seq == nil || *d.Seq == 0 || d.ReasonCode != "" || (conversation.Type == Private && sender != conversation.LowUser && sender != conversation.HighUser) {
			return ServerFrame{}, invalid("persisted result reference")
		}
		if d.Message != nil {
			c, e := canonicalFrameMessage(*d.Message)
			if e != nil || c.SenderID != strconv.FormatUint(sender, 10) || c.CommandID != q.CommandID || c.PayloadHash != d.PayloadHash || d.Message.ConversationID != d.ConversationID || d.Message.Seq != *d.Seq {
				return ServerFrame{}, invalid("persisted result message")
			}
		}
		h.ConversationID, h.MessageID, h.Seq = d.ConversationID, d.MessageID, d.Seq
	default:
		return ServerFrame{}, invalid("result state")
	}
	return makeServerFrame("command.result", h, d)
}
func makeServerFrame(kind string, h notificationHint, data any) (ServerFrame, error) {
	id, e := OutboxEventID(h.CommandID, h.EventType)
	if e != nil {
		return ServerFrame{}, e
	}
	body, e := json.Marshal(struct {
		Type    string `json:"type"`
		EventID string `json:"eventId"`
		Data    any    `json:"data"`
	}{kind, id, data})
	if e != nil {
		return ServerFrame{}, invalid("server frame")
	}
	// Do not retain caller-owned pointers after returning a checked frame.
	if h.Seq != nil {
		seq := *h.Seq
		h.Seq = &seq
	}
	return ServerFrame{kind: kind, eventID: id, body: body, hint: h}, nil
}
