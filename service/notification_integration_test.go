//go:build integration

package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"iChat/database"
	"iChat/messaging"
	"iChat/migrations"
	"iChat/models"
	"iChat/tests/testenv"
	"iChat/utils"
)

func TestNotificationDeliveryIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	env, err := testenv.Start(ctx, filepath.Join("..", "docker-compose.test.yml"))
	if env != nil {
		t.Cleanup(func() {
			c, stop := context.WithTimeout(context.Background(), time.Minute)
			defer stop()
			if e := env.Close(c); e != nil {
				t.Error(e)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("notification delivery fixture run=%s", env.RunID())
	conf, err := env.Config()
	if err != nil {
		t.Fatal(err)
	}
	db, err := utils.OpenMySQL(conf)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	var schema string
	if e := pool.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&schema); e != nil || schema != "ichat_test_"+env.RunID() {
		t.Fatal("refusing writes outside owned schema")
	}
	if e := migrations.ApplyReliable(ctx, pool); e != nil {
		t.Fatal(e)
	}
	if e := db.WithContext(ctx).AutoMigrate(&models.User{}, &models.Group{}, &models.Relation{}); e != nil {
		t.Fatal(e)
	}
	users := make([]models.User, 3)
	for i := range users {
		s := fmt.Sprintf("%s-%d", env.RunID()[:24], i)
		users[i] = models.User{Identifier: s, Phone: s, Name: "notification-fixture"}
	}
	if e := db.WithContext(ctx).Create(&users).Error; e != nil {
		t.Fatal(e)
	}
	group := models.Group{Name: "notification-fixture", OwnerId: users[0].ID, MaxUser: 50}
	if e := db.WithContext(ctx).Create(&group).Error; e != nil {
		t.Fatal(e)
	}
	relations := []models.Relation{{OwnerId: users[0].ID, TargetId: users[1].ID, Type: models.FriendRelation}, {OwnerId: users[1].ID, TargetId: users[0].ID, Type: models.FriendRelation}, {OwnerId: users[0].ID, TargetId: group.ID, Type: models.GroupRelation}, {OwnerId: users[1].ID, TargetId: group.ID, Type: models.GroupRelation}}
	if e := db.WithContext(ctx).Create(&relations).Error; e != nil {
		t.Fatal(e)
	}
	repo, _ := database.NewMessageRepository(pool)
	dispatcher, _ := NewNotificationDispatcher(repo)
	rds, err := utils.OpenRedis(ctx, conf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rds.Close() })
	serial := 0
	seed := func(kind messaging.TargetType, target uint) (messaging.Command, messaging.CommandStatus) {
		serial++
		c, e := messaging.Normalize(uint64(users[0].ID), messaging.SendRequest{ClientMessageID: fmt.Sprintf("12345678-1234-4234-8234-%012d", serial), TargetType: kind, TargetID: fmt.Sprint(target), Media: messaging.Picture, Content: "fixture-private-body:中🙂"})
		if e != nil {
			t.Fatal(e)
		}
		if _, e := repo.PersistCommand(ctx, c); e != nil {
			t.Fatal(e)
		}
		s, e := repo.GetCommandStatus(ctx, uint64(users[0].ID), c.ClientMessageID, c.PayloadHash)
		if e != nil {
			t.Fatal(e)
		}
		return c, s
	}
	private, privateStatus := seed(messaging.Private, users[1].ID)
	groupCommand, groupStatus := seed(messaging.Group, group.ID)
	rejected, _ := seed(messaging.Private, users[2].ID)
	projection := func(c messaging.Command, kind messaging.OutboxEventType) messaging.Notification {
		t.Helper()
		var r messaging.OutboxRecord
		var conversation, message sql.NullString
		if e := pool.QueryRowContext(ctx, "SELECT event_id,event_type,command_id,conversation_id,message_id,payload FROM message_outbox WHERE command_id=? AND event_type=?", c.CommandID, kind).Scan(&r.EventID, &r.EventType, &r.CommandID, &conversation, &message, &r.Payload); e != nil {
			t.Fatal(e)
		}
		if conversation.Valid {
			r.ConversationID = &conversation.String
		}
		if message.Valid {
			r.MessageID = &message.String
		}
		n, e := messaging.DecodeOutboxNotification(r)
		if e != nil {
			t.Fatal(e)
		}
		return n
	}
	pm, pr := projection(private, messaging.EventMessagePersisted), projection(private, messaging.EventCommandResult)
	gm, gr := projection(groupCommand, messaging.EventMessagePersisted), projection(groupCommand, messaging.EventCommandResult)
	rr := projection(rejected, messaging.EventCommandResult)
	sub := rds.Subscribe(ctx, pm.Channel(), pr.Channel(), gm.Channel())
	defer sub.Close()
	for range 3 {
		if _, e := sub.Receive(ctx); e != nil {
			t.Fatal(e)
		}
	}
	type outcome struct {
		ok  bool
		err error
	}
	// This endpoint is a transport fixture: the authenticated UID is injected
	// by the test, not trusted from a query/body. Production session wiring is P5.3.
	deliver := func(t *testing.T, uid uint64, n messaging.Notification, wantFrame bool) json.RawMessage {
		t.Helper()
		if e := rds.Publish(ctx, n.Channel(), n.Body()).Err(); e != nil {
			t.Fatal(e)
		}
		c, stop := context.WithTimeout(ctx, time.Second)
		m, e := sub.ReceiveMessage(c)
		stop()
		if e != nil {
			t.Fatal(e)
		}
		if m.Channel != n.Channel() || m.Payload != string(n.Body()) {
			t.Fatal("Redis projection changed")
		}
		pool.SetMaxOpenConns(1)
		done := make(chan outcome, 1)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, e := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			if e != nil {
				done <- outcome{err: e}
				return
			}
			defer conn.Close()
			c, stop := context.WithTimeout(ctx, time.Second)
			defer stop()
			ok, e := dispatcher.Dispatch(c, uid, m.Channel, []byte(m.Payload), func(c context.Context, f messaging.ServerFrame) error {
				if pool.Stats().InUse != 0 {
					return errors.New("SQL connection still held during enqueue")
				}
				var one int
				if e := pool.QueryRowContext(c, "SELECT 1").Scan(&one); e != nil {
					return e
				}
				deadline, _ := c.Deadline()
				if e := conn.SetWriteDeadline(deadline); e != nil {
					return e
				}
				return conn.WriteMessage(websocket.TextMessage, f.Bytes())
			})
			done <- outcome{ok, e}
		}))
		defer server.Close()
		conn, response, e := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		if e != nil {
			t.Fatal(e)
		}
		defer conn.Close()
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		kind, body, readErr := conn.ReadMessage()
		result := <-done
		if result.err != nil || result.ok != wantFrame {
			t.Fatal("unexpected dispatch result", result.ok, result.err)
		}
		if wantFrame {
			if readErr != nil || kind != websocket.TextMessage {
				t.Fatal("missing data frame", readErr)
			}
			return body
		}
		if readErr == nil || len(body) != 0 {
			t.Fatal("unauthorized frame written")
		}
		return nil
	}
	frameData := func(t *testing.T, body []byte, kind string, n messaging.Notification) json.RawMessage {
		t.Helper()
		var f struct {
			Type    string          `json:"type"`
			EventID string          `json:"eventId"`
			Data    json.RawMessage `json:"data"`
		}
		if e := json.Unmarshal(body, &f); e != nil || f.Type != kind {
			t.Fatal("bad unified frame", e)
		}
		h, e := messaging.DecodeNotificationHint(n.Channel(), n.Body())
		if e != nil || f.EventID != h.EventID() {
			t.Fatal("event ID not retained")
		}
		return f.Data
	}
	t.Run("private_message_and_sender_result_use_canonical_DTO", func(t *testing.T) {
		body := deliver(t, uint64(users[1].ID), pm, true)
		var m messaging.MessageDTO
		if e := json.Unmarshal(frameData(t, body, "message", pm), &m); e != nil || m != *privateStatus.Message {
			t.Fatal("message differs from SQL", e)
		}
		body = deliver(t, uint64(users[0].ID), pr, true)
		var r messaging.CommandResultData
		if e := json.Unmarshal(frameData(t, body, "command.result", pr), &r); e != nil || r.Message == nil || *r.Message != m {
			t.Fatal("sender DTO differs", e)
		}
		deliver(t, uint64(users[2].ID), pm, false)
		deliver(t, uint64(users[1].ID), pr, false)
	})
	t.Run("rejected_result_is_sender_scoped_and_has_no_message", func(t *testing.T) {
		body := deliver(t, uint64(users[0].ID), rr, true)
		var r messaging.CommandResultData
		json.Unmarshal(frameData(t, body, "command.result", rr), &r)
		if r.State != "rejected" || r.ReasonCode != "NOT_FRIENDS" || r.Message != nil || r.MessageID != "" || r.Seq != nil {
			t.Fatal("invalid rejected frame")
		}
		deliver(t, uint64(users[1].ID), rr, false)
	})
	t.Run("group_member_receives_message_and_outsider_does_not", func(t *testing.T) {
		body := deliver(t, uint64(users[1].ID), gm, true)
		var m messaging.MessageDTO
		json.Unmarshal(frameData(t, body, "message", gm), &m)
		if m != *groupStatus.Message {
			t.Fatal("group DTO differs")
		}
		deliver(t, uint64(users[2].ID), gm, false)
		body = deliver(t, uint64(users[0].ID), gr, true)
		var result messaging.CommandResultData
		if e := json.Unmarshal(frameData(t, body, "command.result", gr), &result); e != nil || result.Message == nil || *result.Message != *groupStatus.Message {
			t.Fatal("authorized group result lost canonical message", e)
		}
	})
	t.Run("unfriend_does_not_revoke_participant_history", func(t *testing.T) {
		if _, e := pool.ExecContext(ctx, "UPDATE relation SET deleted_at=UTC_TIMESTAMP(6) WHERE type=? AND owner_id IN (?,?)", models.FriendRelation, users[0].ID, users[1].ID); e != nil {
			t.Fatal(e)
		}
		deliver(t, uint64(users[1].ID), pm, true)
		if _, e := pool.ExecContext(ctx, "UPDATE relation SET deleted_at=NULL WHERE type=? AND owner_id IN (?,?)", models.FriendRelation, users[0].ID, users[1].ID); e != nil {
			t.Fatal(e)
		}
	})
	t.Run("old_group_subscription_loses_content_after_leave", func(t *testing.T) {
		if _, e := pool.ExecContext(ctx, "UPDATE relation SET deleted_at=UTC_TIMESTAMP(6) WHERE owner_id=? AND target_id=? AND type=?", users[1].ID, group.ID, models.GroupRelation); e != nil {
			t.Fatal(e)
		}
		deliver(t, uint64(users[1].ID), gm, false)
		if _, e := pool.ExecContext(ctx, "UPDATE relation SET deleted_at=UTC_TIMESTAMP(6) WHERE owner_id=? AND target_id=? AND type=?", users[0].ID, group.ID, models.GroupRelation); e != nil {
			t.Fatal(e)
		}
		body := deliver(t, uint64(users[0].ID), gr, true)
		var r messaging.CommandResultData
		json.Unmarshal(frameData(t, body, "command.result", gr), &r)
		if r.State != "persisted" || r.Message != nil || r.MessageID != groupStatus.Message.MessageID || r.Seq == nil || *r.Seq != groupStatus.Message.Seq || strings.Contains(string(body), "fixture-private-body") {
			t.Fatal("revoked sender content exposed")
		}
		if _, e := pool.ExecContext(ctx, "UPDATE relation SET deleted_at=NULL WHERE target_id=? AND type=?", group.ID, models.GroupRelation); e != nil {
			t.Fatal(e)
		}
	})
	t.Run("deleted_group_and_deleted_reader_fail_closed", func(t *testing.T) {
		if _, e := pool.ExecContext(ctx, "UPDATE `group` SET deleted_at=UTC_TIMESTAMP(6) WHERE id=?", group.ID); e != nil {
			t.Fatal(e)
		}
		deliver(t, uint64(users[1].ID), gm, false)
		body := deliver(t, uint64(users[0].ID), gr, true)
		if strings.Contains(string(body), `"content"`) {
			t.Fatal("deleted group result leaked content")
		}
		if _, e := pool.ExecContext(ctx, "UPDATE `group` SET deleted_at=NULL WHERE id=?", group.ID); e != nil {
			t.Fatal(e)
		}
		if _, e := pool.ExecContext(ctx, "UPDATE `user` SET deleted_at=UTC_TIMESTAMP(6) WHERE id=?", users[1].ID); e != nil {
			t.Fatal(e)
		}
		deliver(t, uint64(users[1].ID), pm, false)
		if _, e := pool.ExecContext(ctx, "UPDATE `user` SET deleted_at=NULL WHERE id=?", users[1].ID); e != nil {
			t.Fatal(e)
		}
	})
	t.Run("permission_lock_wait_observes_committed_revocation", func(t *testing.T) {
		pool.SetMaxOpenConns(4)
		block, e := pool.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer block.Rollback()
		if _, e := block.ExecContext(ctx, "UPDATE relation SET deleted_at=UTC_TIMESTAMP(6) WHERE owner_id=? AND target_id=? AND type=?", users[1].ID, group.ID, models.GroupRelation); e != nil {
			t.Fatal(e)
		}
		h, _ := messaging.DecodeNotificationHint(gm.Channel(), gm.Body())
		done := make(chan outcome, 1)
		go func() { f, e := repo.ResolveNotification(ctx, uint64(users[1].ID), h); done <- outcome{f.Valid(), e} }()
		deadline := time.Now().Add(3 * time.Second)
		waiting := false
		for !waiting && time.Now().Before(deadline) {
			var n int
			if e := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.processlist WHERE DB=DATABASE() AND INFO LIKE 'SELECT id,deleted_at IS NULL FROM relation%' AND INFO LIKE '%FOR SHARE%'").Scan(&n); e != nil {
				t.Fatal(e)
			}
			waiting = n > 0
			if !waiting {
				time.Sleep(5 * time.Millisecond)
			}
		}
		if !waiting {
			t.Fatal("permission read did not reach actual lock wait")
		}
		select {
		case <-done:
			t.Fatal("revocation lock was bypassed")
		default:
		}
		if e := block.Commit(); e != nil {
			t.Fatal(e)
		}
		result := <-done
		if result.err != nil || result.ok {
			t.Fatal("stale membership authorized", result.err)
		}
		if _, e := pool.ExecContext(ctx, "UPDATE relation SET deleted_at=NULL WHERE owner_id=? AND target_id=? AND type=?", users[1].ID, group.ID, models.GroupRelation); e != nil {
			t.Fatal(e)
		}
	})
	t.Run("canceled_permission_wait_returns_no_frame_and_pool_recovers", func(t *testing.T) {
		pool.SetMaxOpenConns(4)
		block, e := pool.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer block.Rollback()
		if _, e := block.ExecContext(ctx, "UPDATE relation SET deleted_at=UTC_TIMESTAMP(6) WHERE owner_id=? AND target_id=? AND type=?", users[1].ID, group.ID, models.GroupRelation); e != nil {
			t.Fatal(e)
		}
		h, _ := messaging.DecodeNotificationHint(gm.Channel(), gm.Body())
		c, stop := context.WithTimeout(ctx, 100*time.Millisecond)
		f, e := repo.ResolveNotification(c, uint64(users[1].ID), h)
		stop()
		if !errors.Is(e, context.DeadlineExceeded) || f.Valid() {
			t.Fatal("canceled permission exposed frame", e)
		}
		block.Rollback()
		deliver(t, uint64(users[1].ID), gm, true)
	})
	for _, tc := range []struct {
		table string
		want  int
	}{{"message", 2}, {"message_command", 3}, {"conversation", 2}, {"message_outbox", 5}, {"message_delivery_cursor", 0}, {"message_quarantine", 0}} {
		var n int
		if e := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tc.table).Scan(&n); e != nil || n != tc.want {
			t.Fatal("notification altered durable set", tc.table, n, e)
		}
	}
	var pending int
	if e := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM message_outbox WHERE status='pending' AND attempts=0 AND lease_token IS NULL").Scan(&pending); e != nil || pending != 5 {
		t.Fatal("gateway settled Outbox", pending, e)
	}
	if e := migrations.ApplyReliable(ctx, pool); e != nil {
		t.Fatal("schema drift", e)
	}
}
