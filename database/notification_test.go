package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"iChat/messaging"
)

type notificationTestState struct {
	command, message      []driver.Value
	active, member, group bool
	fail                  string
	events                []string
}
type notificationTestConnector struct{ s *notificationTestState }
type notificationTestConn struct{ s *notificationTestState }
type notificationTestTx struct{ s *notificationTestState }

func (c notificationTestConnector) Driver() driver.Driver { return commandTestDriver{} }
func (c notificationTestConnector) Connect(context.Context) (driver.Conn, error) {
	return notificationTestConn{c.s}, nil
}
func (notificationTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected write/prepare")
}
func (notificationTestConn) Begin() (driver.Tx, error) {
	return nil, errors.New("explicit options required")
}
func (notificationTestConn) Close() error { return nil }
func (c notificationTestConn) BeginTx(_ context.Context, o driver.TxOptions) (driver.Tx, error) {
	c.s.events = append(c.s.events, "begin")
	if o.Isolation != driver.IsolationLevel(sql.LevelReadCommitted) || o.ReadOnly {
		return nil, errors.New("permission locks require RC")
	}
	if c.s.fail == "begin" {
		return nil, errors.New("SECRET")
	}
	return notificationTestTx{c.s}, nil
}
func (notificationTestTx) Commit() error { return errors.New("notification must not commit/write") }
func (t notificationTestTx) Rollback() error {
	t.s.events = append(t.s.events, "rollback")
	if t.s.fail == "rollback" {
		return errors.New("SECRET")
	}
	return nil
}
func (c notificationTestConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	var stage string
	rows := &statusTestRows{}
	switch {
	case strings.Contains(q, "FROM `user`"):
		stage = "user"
		rows.count, rows.values = 2, []driver.Value{int64(1), c.s.active}
	case strings.Contains(q, "FROM `group`"):
		stage = "group"
		rows.count, rows.values = 2, []driver.Value{int64(2), c.s.group}
	case strings.Contains(q, "FROM relation"):
		stage = "membership"
		rows.count, rows.values = 2, []driver.Value{int64(3), c.s.member}
	case q == selectNotificationCommandSQL:
		stage = "command"
		rows.count, rows.values = 10, c.s.command
	case q == selectStatusMessageSQL:
		stage = "message"
		rows.count, rows.values = 10, c.s.message
	default:
		return nil, errors.New("unexpected notification query")
	}
	c.s.events = append(c.s.events, stage)
	if c.s.fail == stage {
		return nil, errors.New("SECRET")
	}
	return rows, nil
}
func notificationTestHint(t *testing.T, c messaging.Command, result, rejected bool) messaging.NotificationHint {
	t.Helper()
	kind := messaging.EventMessagePersisted
	if result {
		kind = messaging.EventCommandResult
	}
	id, _ := messaging.OutboxEventID(c.CommandID, kind)
	m, _ := messaging.NewMessageDTO(c, 42, time.Date(2026, 10, 11, 0, 0, 0, 654321000, time.UTC))
	var p any = messaging.MessagePersistedEvent{SchemaVersion: 1, EventID: id, CommandID: c.CommandID, ConversationID: c.ConversationID, MessageID: c.CommandID, Seq: 42, SenderID: c.SenderID, TargetID: c.TargetID, TargetType: c.TargetType}
	conversation, message := &c.ConversationID, &c.CommandID
	if result {
		v := messaging.CommandResultEvent{SchemaVersion: 1, EventID: id, CommandID: c.CommandID, SenderID: c.SenderID, ClientMessageID: c.ClientMessageID, PayloadHash: c.PayloadHash, State: "persisted", Message: &m}
		if rejected {
			v.State, v.ReasonCode, v.Message = "rejected", "NOT_FRIENDS", nil
			conversation, message = nil, nil
		}
		p = v
	}
	body, _ := json.Marshal(p)
	n, e := messaging.DecodeOutboxNotification(messaging.OutboxRecord{EventID: id, CommandID: c.CommandID, EventType: kind, ConversationID: conversation, MessageID: message, Payload: body})
	if e != nil {
		t.Fatal(e)
	}
	h, e := messaging.DecodeNotificationHint(n.Channel(), n.Body())
	if e != nil {
		t.Fatal(e)
	}
	return h
}
func TestNotificationRepositoryAuthorizationAndRelease(t *testing.T) {
	for _, mode := range []string{"private", "group", "left", "deleted_group", "result", "left_result", "rejected", "inactive", "wrong_user", "missing"} {
		t.Run(mode, func(t *testing.T) {
			c := testCommand(t)
			isGroup := mode == "group" || mode == "left" || mode == "deleted_group" || mode == "left_result"
			if isGroup {
				c, _ = messaging.Normalize(1, messaging.SendRequest{ClientMessageID: c.ClientMessageID, TargetType: messaging.Group, TargetID: "2", Media: messaging.Text, Content: c.Content})
			}
			result := mode == "result" || mode == "left_result" || mode == "rejected"
			s := &notificationTestState{command: statusTestCommandRow(c, CommandPersisted), message: statusTestMessageRow(c), active: true, member: true, group: true}
			if mode == "rejected" {
				s.command = statusTestCommandRow(c, CommandRejected)
			}
			if mode == "left" || mode == "left_result" {
				s.member = false
			}
			if mode == "deleted_group" {
				s.group = false
			}
			if mode == "inactive" {
				s.active = false
			}
			if mode == "missing" {
				s.command = nil
			}
			pool := sql.OpenDB(notificationTestConnector{s})
			defer pool.Close()
			r, _ := NewMessageRepository(pool)
			uid := uint64(2)
			if result {
				uid = 1
			}
			if mode == "wrong_user" {
				uid = 99
			}
			h := notificationTestHint(t, c, result, mode == "rejected")
			f, e := r.ResolveNotification(context.Background(), uid, h)
			if e != nil {
				t.Fatal(e)
			}
			want := mode == "private" || mode == "group" || result
			if f.Valid() != want {
				t.Fatal("wrong permission decision", mode)
			}
			if mode == "left_result" && strings.Contains(string(f.Bytes()), `"content"`) {
				t.Fatal("revoked result exposed body")
			}
			if mode == "wrong_user" {
				if len(s.events) != 0 {
					t.Fatal("foreign user reached SQL")
				}
			} else if s.events[len(s.events)-1] != "rollback" {
				t.Fatal("SQL not released before frame")
			}
			if (mode == "left" || mode == "deleted_group" || mode == "inactive") && strings.Contains(strings.Join(s.events, ","), "message") {
				t.Fatal("unauthorized body read")
			}
		})
	}
}
func TestNotificationRepositoryFailureNeverReturnsContent(t *testing.T) {
	c, _ := messaging.Normalize(1, messaging.SendRequest{ClientMessageID: "12345678-1234-4234-8234-123456789abc", TargetType: messaging.Group, TargetID: "2", Media: messaging.Text, Content: "SECRET"})
	h := notificationTestHint(t, c, true, false)
	for _, stage := range []string{"begin", "user", "command", "group", "membership", "message", "rollback", "corrupt_hash", "missing_message", "wrong_seq"} {
		t.Run(stage, func(t *testing.T) {
			s := &notificationTestState{command: statusTestCommandRow(c, CommandPersisted), message: statusTestMessageRow(c), active: true, member: true, group: true, fail: stage}
			if stage == "corrupt_hash" {
				s.command[3] = strings.Repeat("a", 64)
			}
			if stage == "missing_message" {
				s.message = nil
			}
			if stage == "wrong_seq" {
				s.message[8] = int64(43)
			}
			pool := sql.OpenDB(notificationTestConnector{s})
			defer pool.Close()
			r, _ := NewMessageRepository(pool)
			f, e := r.ResolveNotification(context.Background(), 1, h)
			if e == nil || f.Valid() || len(f.Bytes()) != 0 || strings.Contains(e.Error(), "SECRET") {
				t.Fatal("failed resolution leaked result", stage, e)
			}
		})
	}
}

func TestNotificationRepositoryRejectsRecipientSpoof(t *testing.T) {
	c := testCommand(t)
	h := notificationTestHint(t, c, false, false)
	body, _ := json.Marshal(map[string]any{"schemaVersion": 1, "eventType": h.EventType(), "eventId": h.EventID(), "commandId": h.CommandID(), "conversationId": h.ConversationID(), "messageId": h.MessageID(), "seq": h.Sequence()})
	spoof, e := messaging.DecodeNotificationHint("ichat:notify:v1:user:1", body)
	if e != nil {
		t.Fatal("participant syntax should reach SQL recipient check", e)
	}
	s := &notificationTestState{command: statusTestCommandRow(c, CommandPersisted), message: statusTestMessageRow(c), active: true, member: true, group: true}
	pool := sql.OpenDB(notificationTestConnector{s})
	defer pool.Close()
	r, _ := NewMessageRepository(pool)
	f, e := r.ResolveNotification(context.Background(), 1, spoof)
	if !errors.Is(e, ErrCommandInvariant) || f.Valid() || s.events[len(s.events)-1] != "rollback" {
		t.Fatal("sender channel accepted as recipient", e)
	}
}
