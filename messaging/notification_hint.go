package messaging

import (
	"encoding/json"
	"strconv"
	"strings"
)

// NotificationHint is validated syntax and routing, never an authorization
// grant. Its private fields prevent a caller from forging a checked value.
type NotificationHint struct {
	value   notificationHint
	channel string
	user    uint64
}

func (h NotificationHint) Valid() bool                { return h.channel != "" && h.value.EventID != "" }
func (h NotificationHint) EventID() string            { return h.value.EventID }
func (h NotificationHint) EventType() OutboxEventType { return h.value.EventType }
func (h NotificationHint) CommandID() string          { return h.value.CommandID }
func (h NotificationHint) ConversationID() string     { return h.value.ConversationID }
func (h NotificationHint) MessageID() string          { return h.value.MessageID }
func (h NotificationHint) Sequence() Sequence {
	if h.value.Seq == nil {
		return 0
	}
	return *h.value.Seq
}
func (h NotificationHint) ClientMessageID() string { return h.value.ClientMessageID }
func (h NotificationHint) PayloadHash() string     { return h.value.PayloadHash }
func (h NotificationHint) State() string           { return h.value.State }
func (h NotificationHint) ReasonCode() string      { return h.value.ReasonCode }
func (h NotificationHint) UserID() uint64          { return h.user }
func (h NotificationHint) AllowsUser(user uint64) bool {
	return h.Valid() && user != 0 && (h.user == 0 || h.user == user)
}
func (NotificationHint) String() string     { return "notification hint (body redacted)" }
func (h NotificationHint) GoString() string { return h.String() }

// DecodeNotificationHint consumes the exact content-free v1 projection emitted
// by DecodeOutboxNotification, binding it to the actual Redis channel.
func DecodeNotificationHint(channel string, body []byte) (NotificationHint, error) {
	var v notificationHint
	if err := decodeObject(body, &v); err != nil {
		return NotificationHint{}, err
	}
	id, err := OutboxEventID(v.CommandID, v.EventType)
	if err != nil || id != v.EventID || v.SchemaVersion != SchemaVersion {
		return NotificationHint{}, invalid("notification identity")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return NotificationHint{}, invalid("notification")
	}
	allowed := []string{"schemaVersion", "eventType", "eventId", "commandId"}
	result := NotificationHint{value: v, channel: channel}
	if strings.HasPrefix(channel, "ichat:notify:v1:user:") {
		u, e := canonicalDecimal(strings.TrimPrefix(channel, "ichat:notify:v1:user:"), false)
		if e != nil {
			return NotificationHint{}, invalid("notification channel")
		}
		result.user = u
	}
	if v.EventType == EventCommandResult {
		if result.user == 0 {
			return NotificationHint{}, invalid("result channel")
		}
		q, e := NormalizeStatusQuery(result.user, v.ClientMessageID, v.PayloadHash)
		if e != nil || q.CommandID != v.CommandID || q.ClientMessageID != v.ClientMessageID {
			return NotificationHint{}, invalid("result identity")
		}
		allowed = append(allowed, "clientMessageId", "payloadHash", "state")
		if v.State == "persisted" {
			allowed = append(allowed, "conversationId", "messageId", "seq")
		} else if v.State == "rejected" && notificationReason(v.ReasonCode) {
			allowed = append(allowed, "reasonCode")
		} else {
			return NotificationHint{}, invalid("result state")
		}
	} else {
		allowed = append(allowed, "conversationId", "messageId", "seq")
	}
	if len(fields) != len(allowed) {
		return NotificationHint{}, invalid("notification shape")
	}
	for _, name := range allowed {
		if _, ok := fields[name]; !ok {
			return NotificationHint{}, invalid("notification field")
		}
	}
	if v.EventType == EventMessagePersisted || v.State == "persisted" {
		c, e := ParseConversationID(v.ConversationID)
		if e != nil || v.MessageID != v.CommandID || v.Seq == nil || *v.Seq == 0 {
			return NotificationHint{}, invalid("notification message")
		}
		if c.Type == Group {
			if v.EventType == EventMessagePersisted && channel != "ichat:notify:v1:group:"+strconv.FormatUint(c.GroupID, 10) {
				return NotificationHint{}, invalid("group channel")
			}
		} else if result.user == 0 || (result.user != c.LowUser && result.user != c.HighUser) {
			return NotificationHint{}, invalid("private channel")
		}
	}
	return result, nil
}
