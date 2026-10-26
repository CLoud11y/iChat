package messaging

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestNotificationHintAndServerFrameContracts(t *testing.T) {
	for _, kind := range []TargetType{Private, Group} {
		for _, result := range []bool{false, true} {
			r, c := notificationFixture(t, kind, result, false)
			n, e := DecodeOutboxNotification(r)
			if e != nil {
				t.Fatal(e)
			}
			h, e := DecodeNotificationHint(n.Channel(), n.Body())
			if e != nil || !h.Valid() || h.EventID() != r.EventID || h.Sequence() != Sequence(math.MaxUint64) {
				t.Fatal(e)
			}
			m, _ := NewMessageDTO(c, h.Sequence(), time.Date(2026, 10, 25, 1, 2, 3, 123456000, time.UTC))
			var f ServerFrame
			if result {
				seq := m.Seq
				d := CommandResultData{ClientMessageID: c.ClientMessageID, PayloadHash: c.PayloadHash, State: "persisted", MessageID: m.MessageID, ConversationID: m.ConversationID, Seq: &seq, Message: &m}
				f, e = NewCommandResultFrame(2, d)
				seq = 1
				m.Content = "mutated"
			} else {
				f, e = NewMessageFrame(m)
			}
			if e != nil || !f.Valid() || !f.Matches(h) || f.EventID() != r.EventID {
				t.Fatal("frame identity mismatch", e)
			}
			var wire struct {
				Type    string          `json:"type"`
				EventID string          `json:"eventId"`
				Data    json.RawMessage `json:"data"`
			}
			if json.Unmarshal(f.Bytes(), &wire) != nil || wire.Type != f.Type() || wire.EventID != h.EventID() {
				t.Fatal("invalid frame envelope")
			}
			if !strings.Contains(string(f.Bytes()), `"seq":"18446744073709551615"`) || strings.Contains(string(f.Bytes()), "mutated") {
				t.Fatal("canonical immutable DTO lost")
			}
			b := f.Bytes()
			b[0] = 'x'
			if f.Bytes()[0] != '{' {
				t.Fatal("frame bytes mutable")
			}
			if result && !h.AllowsUser(2) {
				t.Fatal("sender not scoped")
			}
			if result && h.AllowsUser(10) {
				t.Fatal("other result reader allowed")
			}
		}
	}
	r, c := notificationFixture(t, Private, true, true)
	n, _ := DecodeOutboxNotification(r)
	h, _ := DecodeNotificationHint(n.Channel(), n.Body())
	f, e := NewCommandResultFrame(2, CommandResultData{ClientMessageID: c.ClientMessageID, PayloadHash: c.PayloadHash, State: "rejected", ReasonCode: "NOT_FRIENDS"})
	if e != nil || !f.Matches(h) || strings.Contains(string(f.Bytes()), "message") {
		t.Fatal(e)
	}
}

func TestNotificationHintStrictProjection(t *testing.T) {
	r, _ := notificationFixture(t, Private, true, false)
	n, _ := DecodeOutboxNotification(r)
	for _, change := range []func(string) string{
		func(s string) string { return strings.Replace(s, `"seq":`, `"seq":"1","seq":`, 1) },
		func(s string) string { return strings.Replace(s, `"eventId":`, `"EventId":`, 1) },
		func(s string) string { return strings.Replace(s, `"seq":"18446744073709551615"`, `"seq":"01"`, 1) },
		func(s string) string { return strings.Replace(s, `"seq":"18446744073709551615"`, `"seq":1`, 1) },
		func(s string) string { return strings.Replace(s, `"seq":"18446744073709551615"`, `"seq":null`, 1) },
		func(s string) string { return strings.Replace(s, `"seq":"18446744073709551615"`, `"seq":"0"`, 1) },
		func(s string) string { return strings.Replace(s, `"state":"persisted"`, `"state":"pending"`, 1) },
		func(s string) string { return strings.Replace(s, `"schemaVersion":1`, `"schemaVersion":2`, 1) },
		func(s string) string { return strings.Replace(s, `"messageId":`, `"content":"secret","messageId":`, 1) },
		func(s string) string { return strings.TrimSuffix(s, "}") + `,"reasonCode":""}` },
		func(s string) string { return s + "{}" },
		func(string) string { return strings.Repeat(" ", MaxBodyBytes+1) },
	} {
		if h, e := DecodeNotificationHint(n.Channel(), []byte(change(string(n.Body())))); e == nil || h.Valid() {
			t.Fatal("invalid projection accepted")
		}
	}
	for _, channel := range []string{"private_2", "ichat:notify:v1:user:02", "ichat:notify:v1:user:10", "ichat:notify:v1:group:10", ""} {
		if _, e := DecodeNotificationHint(channel, n.Body()); e == nil {
			t.Fatal("channel not bound", channel)
		}
	}
	g, _ := notificationFixture(t, Group, false, false)
	gn, _ := DecodeOutboxNotification(g)
	if _, e := DecodeNotificationHint("ichat:notify:v1:group:11", gn.Body()); e == nil {
		t.Fatal("wrong group accepted")
	}
}

func TestServerFrameRejectsCorruptDTOAndTerminalShape(t *testing.T) {
	_, c := notificationFixture(t, Private, false, false)
	original, _ := NewMessageDTO(c, 1, time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC))
	for _, change := range []func(*MessageDTO){func(m *MessageDTO) { m.MessageID = "x" }, func(m *MessageDTO) { m.TargetID = "010" }, func(m *MessageDTO) { m.Seq = 0 }, func(m *MessageDTO) { m.SenderID = "02" }, func(m *MessageDTO) { m.ConversationID = "g:10" }, func(m *MessageDTO) { m.ServerCreatedAt = "2026-10-26T00:00:00+00:00" }} {
		m := original
		change(&m)
		if _, e := NewMessageFrame(m); e == nil {
			t.Fatal("corrupt DTO accepted")
		}
	}
	seq := Sequence(1)
	for _, d := range []CommandResultData{
		{ClientMessageID: c.ClientMessageID, PayloadHash: c.PayloadHash, State: "pending"},
		{ClientMessageID: c.ClientMessageID, PayloadHash: c.PayloadHash, State: "rejected", ReasonCode: "SECRET"},
		{ClientMessageID: c.ClientMessageID, PayloadHash: c.PayloadHash, State: "rejected", ReasonCode: "NOT_FRIENDS", Message: &original},
		{ClientMessageID: c.ClientMessageID, PayloadHash: c.PayloadHash, State: "persisted"},
		{ClientMessageID: c.ClientMessageID, PayloadHash: strings.Repeat("a", 64), State: "persisted", Message: &original, MessageID: c.CommandID, ConversationID: c.ConversationID, Seq: &seq},
	} {
		if _, e := NewCommandResultFrame(2, d); e == nil {
			t.Fatal("invalid terminal frame accepted")
		}
	}
	d := CommandResultData{ClientMessageID: c.ClientMessageID, PayloadHash: c.PayloadHash, State: "persisted", MessageID: c.CommandID, ConversationID: c.ConversationID, Seq: &seq}
	f, e := NewCommandResultFrame(2, d)
	if e != nil || strings.Contains(string(f.Bytes()), `"content"`) {
		t.Fatal("reference-only terminal failed", e)
	}
}
