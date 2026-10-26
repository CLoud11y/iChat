package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"iChat/messaging"
)

type notificationReaderFunc func(context.Context, uint64, messaging.NotificationHint) (messaging.ServerFrame, error)

func (f notificationReaderFunc) ResolveNotification(c context.Context, u uint64, h messaging.NotificationHint) (messaging.ServerFrame, error) {
	return f(c, u, h)
}

func TestNotificationDispatcherBoundaries(t *testing.T) {
	c, _ := messaging.Normalize(1, messaging.SendRequest{ClientMessageID: "12345678-1234-4234-8234-123456789abc", TargetType: messaging.Private, TargetID: "2", Media: messaging.Text, Content: "private-body"})
	id, _ := messaging.OutboxEventID(c.CommandID, messaging.EventMessagePersisted)
	payload, _ := json.Marshal(messaging.MessagePersistedEvent{SchemaVersion: 1, EventID: id, CommandID: c.CommandID, ConversationID: c.ConversationID, MessageID: c.CommandID, Seq: 1, SenderID: c.SenderID, TargetID: c.TargetID, TargetType: c.TargetType})
	n, _ := messaging.DecodeOutboxNotification(messaging.OutboxRecord{EventID: id, EventType: messaging.EventMessagePersisted, CommandID: c.CommandID, ConversationID: &c.ConversationID, MessageID: &c.CommandID, Payload: payload})
	for _, mode := range []string{"ok", "suppressed", "read_error", "wrong_frame", "canceled_after_read", "enqueue_error", "foreign_user", "bad_hint", "unbounded"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var contextToUse context.Context = ctx
			if mode == "unbounded" {
				contextToUse = context.Background()
			}
			reads, writes := 0, 0
			r := notificationReaderFunc(func(cctx context.Context, uid uint64, h messaging.NotificationHint) (messaging.ServerFrame, error) {
				reads++
				if uid != 2 || !h.Valid() {
					t.Fatal("reader not scoped")
				}
				if mode == "read_error" {
					return messaging.ServerFrame{}, errors.New("SECRET")
				}
				if mode == "suppressed" {
					return messaging.ServerFrame{}, nil
				}
				seq := messaging.Sequence(1)
				if mode == "wrong_frame" {
					seq = 2
				}
				m, _ := messaging.NewMessageDTO(c, seq, time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC))
				f, e := messaging.NewMessageFrame(m)
				if mode == "canceled_after_read" {
					cancel()
				}
				return f, e
			})
			d, _ := NewNotificationDispatcher(r)
			body := n.Body()
			if mode == "bad_hint" {
				body = []byte(`{"content":"SECRET"}`)
			}
			uid := uint64(2)
			if mode == "foreign_user" {
				uid = 99
			}
			ok, e := d.Dispatch(contextToUse, uid, n.Channel(), body, func(context.Context, messaging.ServerFrame) error {
				writes++
				if mode == "enqueue_error" {
					return errors.New("SECRET")
				}
				return nil
			})
			if mode == "ok" {
				if !ok || e != nil || reads != 1 || writes != 1 {
					t.Fatal(ok, e, reads, writes)
				}
			} else if mode == "suppressed" || mode == "foreign_user" {
				if ok || e != nil || writes != 0 {
					t.Fatal(ok, e)
				}
			} else {
				if ok || e == nil || strings.Contains(e.Error(), "SECRET") {
					t.Fatal("failure leaked or succeeded", ok, e)
				}
			}
			if (mode == "foreign_user" || mode == "bad_hint" || mode == "unbounded") && reads != 0 {
				t.Fatal("invalid hint/reader reached storage")
			}
			if (mode == "read_error" || mode == "wrong_frame" || mode == "canceled_after_read") && writes != 0 {
				t.Fatal("unconfirmed content enqueued")
			}
		})
	}
	var nilReader notificationReaderFunc
	if _, e := NewNotificationDispatcher(nilReader); e == nil {
		t.Fatal("typed nil reader accepted")
	}
}
