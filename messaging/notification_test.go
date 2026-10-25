package messaging

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func notificationFixture(t *testing.T, kind TargetType, result, rejected bool) (OutboxRecord, Command) {
	t.Helper()
	c, err := Normalize(2, SendRequest{ClientMessageID: "12345678-1234-4234-8234-123456789abc", TargetType: kind, TargetID: "10", Media: Text, Content: "private-secret"})
	if err != nil {
		t.Fatal(err)
	}
	eventType := EventMessagePersisted
	if result {
		eventType = EventCommandResult
	}
	id, _ := OutboxEventID(c.CommandID, eventType)
	r := OutboxRecord{EventID: id, CommandID: c.CommandID, EventType: eventType, ConversationID: &c.ConversationID, MessageID: &c.CommandID}
	m, _ := NewMessageDTO(c, Sequence(math.MaxUint64), time.Date(2026, 10, 25, 1, 2, 3, 123456000, time.UTC))
	var e any = MessagePersistedEvent{SchemaVersion: 1, EventID: id, CommandID: c.CommandID, ConversationID: c.ConversationID, MessageID: c.CommandID, Seq: m.Seq, SenderID: c.SenderID, TargetID: c.TargetID, TargetType: kind}
	if result {
		v := CommandResultEvent{SchemaVersion: 1, EventID: id, CommandID: c.CommandID, SenderID: c.SenderID, ClientMessageID: c.ClientMessageID, PayloadHash: c.PayloadHash, State: "persisted", Message: &m}
		if rejected {
			v.State, v.ReasonCode, v.Message = "rejected", "NOT_FRIENDS", nil
			r.ConversationID, r.MessageID = nil, nil
		}
		e = v
	}
	r.Payload, err = json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return r, c
}

func TestOutboxNotificationProjection(t *testing.T) {
	for _, tc := range []struct {
		kind             TargetType
		result, rejected bool
		channel          string
	}{
		{Private, false, false, "ichat:notify:v1:user:10"}, {Group, false, false, "ichat:notify:v1:group:10"},
		{Private, true, false, "ichat:notify:v1:user:2"}, {Group, true, false, "ichat:notify:v1:user:2"}, {Private, true, true, "ichat:notify:v1:user:2"},
	} {
		r, _ := notificationFixture(t, tc.kind, tc.result, tc.rejected)
		n, err := DecodeOutboxNotification(r)
		if err != nil || !n.Valid() || n.Channel() != tc.channel {
			t.Fatal("bad projection", err)
		}
		body := string(n.Body())
		if strings.Contains(body, "private-secret") || strings.Contains(body, "content") || strings.Contains(body, "media") {
			t.Fatal("content escaped")
		}
		if !tc.rejected && !strings.Contains(body, `"seq":"18446744073709551615"`) {
			t.Fatal("sequence lost")
		}
		copy := n.Body()
		copy[0] = 'x'
		if n.Body()[0] != '{' {
			t.Fatal("body can be mutated")
		}
	}
}

func TestOutboxNotificationRejectsContradictions(t *testing.T) {
	for _, tc := range []struct {
		name             string
		result, rejected bool
		change           func(*OutboxRecord)
	}{
		{"event_id", false, false, func(r *OutboxRecord) { r.EventID = strings.Repeat("0", 64) }},
		{"unknown_type", false, false, func(r *OutboxRecord) { r.EventType = "other" }},
		{"null_route", false, false, func(r *OutboxRecord) { r.MessageID = nil }},
		{"wrong_route", true, false, func(r *OutboxRecord) { s := "g:99"; r.ConversationID = &s }},
		{"rejected_route", true, true, func(r *OutboxRecord) { s := "x"; r.MessageID = &s }},
		{"missing", false, false, func(r *OutboxRecord) { r.Payload = []byte(`{}`) }},
		{"too_large", false, false, func(r *OutboxRecord) { r.Payload = []byte(strings.Repeat(" ", MaxBodyBytes+1)) }},
		{"duplicate", false, false, func(r *OutboxRecord) {
			r.Payload = []byte(strings.Replace(string(r.Payload), `"seq":`, `"seq":"1","seq":`, 1))
		}},
		{"zero_seq", false, false, func(r *OutboxRecord) {
			r.Payload = []byte(strings.Replace(string(r.Payload), `"18446744073709551615"`, `"0"`, 1))
		}},
		{"numeric_seq", false, false, func(r *OutboxRecord) {
			r.Payload = []byte(strings.Replace(string(r.Payload), `"18446744073709551615"`, `1`, 1))
		}},
		{"alias", false, false, func(r *OutboxRecord) {
			r.Payload = []byte(strings.Replace(string(r.Payload), `"senderId"`, `"SenderId"`, 1))
		}},
		{"null", false, false, func(r *OutboxRecord) {
			r.Payload = []byte(strings.Replace(string(r.Payload), `"senderId":"2"`, `"senderId":null`, 1))
		}},
		{"version", false, false, func(r *OutboxRecord) {
			r.Payload = []byte(strings.Replace(string(r.Payload), `"schemaVersion":1`, `"schemaVersion":2`, 1))
		}},
		{"target", false, false, func(r *OutboxRecord) {
			r.Payload = []byte(strings.Replace(string(r.Payload), `"targetId":"10"`, `"targetId":"11"`, 1))
		}},
		{"canonical_id", false, false, func(r *OutboxRecord) {
			r.Payload = []byte(strings.Replace(string(r.Payload), `"senderId":"2"`, `"senderId":"02"`, 1))
		}},
		{"nested_duplicate", true, false, func(r *OutboxRecord) {
			r.Payload = []byte(strings.Replace(string(r.Payload), `"content":`, `"content":"a","content":`, 1))
		}},
		{"nested_unknown", true, false, func(r *OutboxRecord) {
			r.Payload = []byte(strings.Replace(string(r.Payload), `"content":`, `"extra":1,"content":`, 1))
		}},
		{"nested_alias", true, false, func(r *OutboxRecord) {
			r.Payload = []byte(strings.Replace(string(r.Payload), `"content":`, `"Content":`, 1))
		}},
		{"nested_missing", true, false, func(r *OutboxRecord) {
			r.Payload = []byte(strings.Replace(string(r.Payload), `"content":"private-secret",`, "", 1))
		}},
		{"hash", true, false, func(r *OutboxRecord) {
			r.Payload = []byte(strings.Replace(string(r.Payload), "private-secret", "changed", 1))
		}},
		{"nested_sender", true, false, func(r *OutboxRecord) {
			s := string(r.Payload)
			i := strings.LastIndex(s, `"senderId":"2"`)
			r.Payload = []byte(s[:i] + strings.Replace(s[i:], `"senderId":"2"`, `"senderId":"3"`, 1))
		}},
		{"timestamp", true, false, func(r *OutboxRecord) {
			r.Payload = []byte(strings.Replace(string(r.Payload), "2026-10-25", "2026-13-25", 1))
		}},
		{"rejected_reason", true, true, func(r *OutboxRecord) {
			r.Payload = []byte(strings.Replace(string(r.Payload), "NOT_FRIENDS", "private-secret", 1))
		}},
		{"nonterminal", true, true, func(r *OutboxRecord) {
			r.Payload = []byte(strings.Replace(string(r.Payload), "rejected", "pending", 1))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := notificationFixture(t, Private, tc.result, tc.rejected)
			tc.change(&r)
			n, err := DecodeOutboxNotification(r)
			if err == nil || n.Valid() {
				t.Fatal("contradiction accepted")
			}
		})
	}
}
