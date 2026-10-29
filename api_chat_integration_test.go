//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/gorilla/websocket"
	"iChat/application"
	"iChat/database"
	"iChat/messaging"
	"iChat/migrations"
	"iChat/models"
	"iChat/tests/testenv"
	"iChat/utils"
	"iChat/workers"
)

func TestProductionV1ChatIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	env, err := testenv.Start(ctx, filepath.Join("docker-compose.test.yml"))
	if env != nil {
		t.Cleanup(func() {
			c, stop := context.WithTimeout(context.Background(), time.Minute)
			defer stop()
			if err := env.Close(c); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("production v1 chat fixture run=%s", env.RunID())
	conf, err := env.Config()
	if err != nil {
		t.Fatal(err)
	}
	base := apiTestConfig()
	base.MYSQL, base.REDIS = conf.MYSQL, conf.REDIS
	db, err := utils.OpenMySQL(base)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	var schema string
	if err := pool.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&schema); err != nil || schema != "ichat_test_"+env.RunID() {
		t.Fatal("refusing writes outside owned schema")
	}
	if err := migrations.ApplyReliable(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).AutoMigrate(&models.User{}, &models.Group{}, &models.Relation{}); err != nil {
		t.Fatal(err)
	}
	users := make([]models.User, 2)
	for i := range users {
		v := fmt.Sprintf("%s-%d", env.RunID()[:24], i)
		users[i] = models.User{Identifier: v, Phone: v, Name: "v1-chat-fixture"}
	}
	if err := db.WithContext(ctx).Create(&users).Error; err != nil {
		t.Fatal(err)
	}
	group := models.Group{Name: "v1-chat", OwnerId: users[0].ID, MaxUser: 50}
	if err := db.WithContext(ctx).Create(&group).Error; err != nil {
		t.Fatal(err)
	}
	relations := []models.Relation{{OwnerId: users[0].ID, TargetId: users[1].ID, Type: models.FriendRelation}, {OwnerId: users[1].ID, TargetId: users[0].ID, Type: models.FriendRelation}, {OwnerId: users[0].ID, TargetId: group.ID, Type: models.GroupRelation}}
	if err := db.WithContext(ctx).Create(&relations).Error; err != nil {
		t.Fatal(err)
	}
	client, err := utils.OpenRedis(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	tokens, err := utils.NewTokenService(base, utils.NewRedisTokenBlacklist(client))
	if err != nil {
		t.Fatal(err)
	}
	token, err := tokens.GenerateToken(users[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	o := defaultAPIOptions(base)
	o.Address = "127.0.0.1:0"
	o.ProbeInterval, o.ProbeTimeout = 100*time.Millisecond, 300*time.Millisecond
	s, err := newAPIService(base, application.RoleDependencies{Publisher: &apiPublisher{}, Health: apiTestHealth{}}, o, defaultAPIDependencies())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c, stop := context.WithTimeout(context.Background(), 5*time.Second); defer stop(); s.Close(c) })
	setup, stop := context.WithTimeout(ctx, 5*time.Second)
	err = s.Start(setup, ctx)
	stop()
	if err != nil {
		t.Fatal(err)
	}
	address := "ws://" + apiAddress(s) + "/auth/chat"
	userChannel := "ichat:notify:v1:user:" + strconv.FormatUint(uint64(users[1].ID), 10)
	groupChannel := "ichat:notify:v1:group:" + strconv.FormatUint(uint64(group.ID), 10)
	cleanupConnection := t.Cleanup
	dial := func(t *testing.T, raw string) (*websocket.Conn, string) {
		t.Helper()
		d := websocket.Dialer{HandshakeTimeout: 2 * time.Second, Subprotocols: []string{"ichat.v1"}}
		conn, response, err := d.Dial(address, http.Header{"Authorization": []string{"Bearer " + raw}})
		if err != nil {
			if response != nil {
				response.Body.Close()
			}
			t.Fatal(err)
		}
		cleanupConnection(func() { conn.Close() })
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var ready struct {
			Type string `json:"type"`
			Data struct {
				Version int    `json:"protocolVersion"`
				Session string `json:"sessionId"`
				Sync    int    `json:"syncIntervalSeconds"`
			} `json:"data"`
		}
		if err := conn.ReadJSON(&ready); err != nil || ready.Type != "ready" || ready.Data.Version != 1 || ready.Data.Sync != 10 || len(ready.Data.Session) != 32 || conn.Subprotocol() != "ichat.v1" {
			t.Fatal("production ready contract invalid", err)
		}
		return conn, ready.Data.Session
	}
	subscribers := func(channel string) int64 {
		m, err := client.PubSubNumSub(ctx, channel).Result()
		if err != nil {
			t.Fatal(err)
		}
		return m[channel]
	}
	readMessage := func(t *testing.T, conn *websocket.Conn) messaging.MessageDTO {
		t.Helper()
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		for {
			_, body, err := conn.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			var f struct {
				Type string          `json:"type"`
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(body, &f); err != nil {
				t.Fatal(err)
			}
			switch f.Type {
			case "message":
				var m messaging.MessageDTO
				if err := json.Unmarshal(f.Data, &m); err != nil {
					t.Fatal(err)
				}
				return m
			case "ping":
				if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"pong","data":{}}`)); err != nil {
					t.Fatal(err)
				}
			case "sync.required":
			default:
				t.Fatal("unexpected data frame", f.Type)
			}
		}
	}
	repo, _ := database.NewMessageRepository(pool)
	outbox, _ := database.NewOutboxRepository(pool)
	publisher, _ := workers.NewRedisNotificationPublisher(client)
	loop, err := workers.NewOutboxLoop(outbox, publisher, workers.DefaultOutboxServiceOptions().Loop)
	if err != nil {
		t.Fatal(err)
	}
	serial := 0
	seed := func(kind messaging.TargetType) messaging.Command {
		serial++
		target := users[1].ID
		if kind == messaging.Group {
			target = group.ID
		}
		c, err := messaging.Normalize(uint64(users[0].ID), messaging.SendRequest{ClientMessageID: fmt.Sprintf("12345678-1234-4234-8234-%012d", serial), TargetType: kind, TargetID: fmt.Sprint(target), Media: messaging.Audio, Content: fmt.Sprintf("canonical-v1-%d-中🙂", serial)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repo.PersistCommand(ctx, c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	flush := func() {
		r, err := loop.RunBatch(ctx)
		if err != nil || r.Completed != 2 {
			t.Fatal("Outbox publish did not complete", r, err)
		}
	}
	var first, fresh *websocket.Conn
	var firstID string
	run := func(name string, f func(*testing.T)) {
		if !t.Run(name, f) {
			t.FailNow()
		}
	}
	run("header_auth_origin_and_empty_groups", func(t *testing.T) {
		for _, headers := range []http.Header{nil, {"Authorization": []string{"Bearer invalid"}}} {
			_, response, err := websocket.DefaultDialer.Dial(address+"?token="+token, headers)
			if err == nil || response == nil || response.StatusCode != 401 {
				t.Fatal("header authentication bypassed", err)
			}
			response.Body.Close()
		}
		_, response, err := websocket.DefaultDialer.Dial(address, http.Header{"Authorization": []string{"Bearer " + token}, "Origin": []string{"https://wrong.example"}})
		if err == nil || response == nil || response.StatusCode != 403 {
			t.Fatal("cross-origin upgrade allowed", err)
		}
		response.Body.Close()
		apiRealEventually(t, func() bool { return subscribers(userChannel) == 0 })
		first, firstID = dial(t, token)
		if subscribers(userChannel) != 1 || subscribers(groupChannel) != 0 {
			t.Fatal("empty groups created extra subscription")
		}
	})
	run("actual_outbox_hint_becomes_canonical_data_frame", func(t *testing.T) {
		c := seed(messaging.Private)
		flush()
		m := readMessage(t, first)
		status, err := repo.GetCommandStatus(ctx, uint64(users[0].ID), c.ClientMessageID, c.PayloadHash)
		if err != nil || status.Message == nil || *status.Message != m {
			t.Fatal("canonical SQL message changed", err)
		}
	})
	run("replacement_has_new_identity_and_retires_old_subscription", func(t *testing.T) {
		var id string
		fresh, id = dial(t, token)
		if id == firstID {
			t.Fatal("session ID reused")
		}
		first.SetReadDeadline(time.Now().Add(time.Second))
		for {
			if _, _, err := first.ReadMessage(); err != nil {
				break
			}
		}
		apiRealEventually(t, func() bool { return subscribers(userChannel) == 1 })
	})
	run("sender_result_uses_same_production_personal_subscription", func(t *testing.T) {
		serial++
		command, err := messaging.Normalize(uint64(users[1].ID), messaging.SendRequest{ClientMessageID: fmt.Sprintf("12345678-1234-4234-8234-%012d", serial), TargetType: messaging.Private, TargetID: fmt.Sprint(users[0].ID), Media: messaging.Text, Content: "canonical-v1-sender-result"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repo.PersistCommand(ctx, command); err != nil {
			t.Fatal(err)
		}
		flush()
		fresh.SetReadDeadline(time.Now().Add(3 * time.Second))
		for {
			_, body, err := fresh.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			var frame struct {
				Type    string          `json:"type"`
				EventID string          `json:"eventId"`
				Data    json.RawMessage `json:"data"`
			}
			if json.Unmarshal(body, &frame) != nil {
				t.Fatal("invalid result envelope")
			}
			if frame.Type == "ping" {
				fresh.WriteMessage(websocket.TextMessage, []byte(`{"type":"pong","data":{}}`))
				continue
			}
			if frame.Type == "sync.required" {
				continue
			}
			expectedID, _ := messaging.OutboxEventID(command.CommandID, messaging.EventCommandResult)
			var result messaging.CommandResultData
			if frame.Type != "command.result" || frame.EventID != expectedID || json.Unmarshal(frame.Data, &result) != nil || result.Message == nil {
				t.Fatal("missing canonical command result")
			}
			status, err := repo.GetCommandStatus(ctx, uint64(users[1].ID), command.ClientMessageID, command.PayloadHash)
			if err != nil || status.Message == nil || *status.Message != *result.Message {
				t.Fatal("command result differs from SQL", err)
			}
			break
		}
	})
	run("current_session_delivery_ACK_is_durable_and_monotonic", func(t *testing.T) {
		conversationID := fmt.Sprintf("p:%d:%d", users[0].ID, users[1].ID)
		for _, seq := range []messaging.Sequence{2, 0, 2} {
			if err := fresh.WriteJSON(struct {
				Type string                `json:"type"`
				Data messaging.DeliveryACK `json:"data"`
			}{"ack", messaging.DeliveryACK{ConversationID: conversationID, DeliveredSeq: seq}}); err != nil {
				t.Fatal(err)
			}
			fresh.SetReadDeadline(time.Now().Add(3 * time.Second))
			for {
				var f struct {
					Type string          `json:"type"`
					Data json.RawMessage `json:"data"`
				}
				if err := fresh.ReadJSON(&f); err != nil {
					t.Fatal(err)
				}
				if f.Type == "ping" {
					fresh.WriteMessage(websocket.TextMessage, []byte(`{"type":"pong","data":{}}`))
					continue
				}
				if f.Type == "sync.required" {
					continue
				}
				var ack messaging.DeliveryACK
				if f.Type != "ack.confirmed" || json.Unmarshal(f.Data, &ack) != nil || ack.ConversationID != conversationID || ack.DeliveredSeq != 2 {
					t.Fatal("invalid ACK confirmation", f.Type, string(f.Data))
				}
				var stored uint64
				if err := pool.QueryRowContext(ctx, "SELECT delivered_seq FROM message_delivery_cursor WHERE user_id=? AND conversation_id=?", users[1].ID, conversationID).Scan(&stored); err != nil || stored != 2 {
					t.Fatal("confirmation preceded SQL commit", stored, err)
				}
				break
			}
		}
	})
	run("group_join_refreshes_confirmed_subscription", func(t *testing.T) {
		if err := db.WithContext(ctx).Create(&models.Relation{OwnerId: users[1].ID, TargetId: group.ID, Type: models.GroupRelation}).Error; err != nil {
			t.Fatal(err)
		}
		apiRealEventually(t, func() bool { return subscribers(groupChannel) == 1 })
		c := seed(messaging.Group)
		flush()
		m := readMessage(t, fresh)
		if m.MessageID != c.CommandID || m.ConversationID != c.ConversationID {
			t.Fatal("group hint did not materialize canonical message")
		}
	})
	run("queued_group_hint_is_rechecked_and_deleted_group_unsubscribed", func(t *testing.T) {
		_ = seed(messaging.Group)
		if _, err := pool.ExecContext(ctx, "UPDATE `group` SET deleted_at=UTC_TIMESTAMP(6) WHERE id=?", group.ID); err != nil {
			t.Fatal(err)
		}
		flush()
		barrier := seed(messaging.Private)
		flush()
		m := readMessage(t, fresh)
		if m.MessageID != barrier.CommandID {
			t.Fatal("deleted-group content reached socket")
		}
		apiRealEventually(t, func() bool { return subscribers(groupChannel) == 0 })
	})
	run("revoked_token_closes_existing_session_on_refresh", func(t *testing.T) {
		if _, err := tokens.RevokeToken(ctx, token); err != nil {
			t.Fatal(err)
		}
		apiRealEventually(t, func() bool { return subscribers(userChannel) == 0 })
		fresh.SetReadDeadline(time.Now().Add(2 * time.Second))
		for {
			if _, _, err := fresh.ReadMessage(); err != nil {
				break
			}
		}
	})
	run("API_shutdown_joins_hijacked_sessions_before_clients", func(t *testing.T) {
		next, err := tokens.GenerateToken(users[1].ID)
		if err != nil {
			t.Fatal(err)
		}
		conn, _ := dial(t, next)
		s.StopAdmission()
		closeCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		err = s.Close(closeCtx)
		stop()
		if err != nil {
			t.Fatal(err)
		}
		conn.SetReadDeadline(time.Now().Add(time.Second))
		if _, _, err := conn.ReadMessage(); err == nil {
			t.Fatal("shutdown kept WebSocket open")
		}
		apiRealEventually(t, func() bool { return subscribers(userChannel) == 0 })
		apiWait(t, s.Done())
		s.mu.Lock()
		owned := s.resources
		s.mu.Unlock()
		if err := owned.sql.PingContext(ctx); err == nil {
			t.Fatal("SQL client not closed")
		}
		if err := owned.redis.Get(ctx, "closed").Err(); !errors.Is(err, redis.ErrClosed) {
			t.Fatal("Redis client not closed", err)
		}
		if err := pool.PingContext(ctx); err != nil {
			t.Fatal("API closed external fixture pool", err)
		}
	})
	for _, tc := range []struct {
		table string
		want  int
	}{{"message", 5}, {"message_command", 5}, {"message_outbox", 10}, {"conversation", 2}, {"message_delivery_cursor", 1}, {"message_quarantine", 0}} {
		var n int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tc.table).Scan(&n); err != nil || n != tc.want {
			t.Fatal("wrong final collection", tc.table, n, err)
		}
	}
	var complete int
	if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM message_outbox WHERE status='completed'").Scan(&complete); err != nil || complete != 10 {
		t.Fatal(complete, err)
	}
	if err := migrations.ApplyReliable(ctx, pool); err != nil {
		t.Fatal("schema drift", err)
	}
}
