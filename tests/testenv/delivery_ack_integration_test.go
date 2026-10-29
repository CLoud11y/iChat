//go:build integration

package testenv

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	mysql "github.com/go-sql-driver/mysql"
	"github.com/gorilla/websocket"
	"iChat/database"
	"iChat/messaging"
	"iChat/migrations"
	"iChat/models"
	"iChat/router"
	"iChat/service"
	"iChat/utils"
)

type deliveryACKBarrier struct {
	mu            sync.Mutex
	repo          *database.MessageRepository
	before, after func(context.Context)
	calls         atomic.Int32
}

func (b *deliveryACKBarrier) AcknowledgeDelivery(ctx context.Context, u uint64, id string, s messaging.Sequence) (messaging.Sequence, error) {
	b.calls.Add(1)
	b.mu.Lock()
	before, after := b.before, b.after
	b.before, b.after = nil, nil
	b.mu.Unlock()
	if before != nil {
		before(ctx)
	}
	seq, err := b.repo.AcknowledgeDelivery(ctx, u, id, s)
	if after != nil {
		after(ctx)
	}
	return seq, err
}

// Uses owned SQL/Redis, real production JWT/router/subscriptions, and the
// classic-protocol proxy to discard an actual MySQL COMMIT OK response.
// Business commands are created by PersistCommand; no MQ pipeline is claimed.
func TestDeliveryACKIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	env, err := Start(ctx, filepath.Join("..", "..", "docker-compose.test.yml"))
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
	t.Logf("delivery ACK fixture run=%s", env.RunID())
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
	if err := pool.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&schema); err != nil || schema != "ichat_test_"+env.RunID() {
		t.Fatal("refusing writes outside owned schema")
	}
	if err := migrations.ApplyReliable(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).AutoMigrate(&models.User{}, &models.Group{}, &models.Relation{}); err != nil {
		t.Fatal(err)
	}
	users := make([]models.User, 3)
	for i := range users {
		v := fmt.Sprintf("%s-%d", env.RunID()[:24], i)
		users[i] = models.User{Identifier: v, Phone: v, Name: "delivery-ack-fixture"}
	}
	if err := db.WithContext(ctx).Create(&users).Error; err != nil {
		t.Fatal(err)
	}
	group := models.Group{Name: "ack", OwnerId: users[0].ID, MaxUser: 50}
	if err := db.WithContext(ctx).Create(&group).Error; err != nil {
		t.Fatal(err)
	}
	relations := []models.Relation{{OwnerId: users[0].ID, TargetId: users[1].ID, Type: models.FriendRelation}, {OwnerId: users[1].ID, TargetId: users[0].ID, Type: models.FriendRelation}, {OwnerId: users[0].ID, TargetId: group.ID, Type: models.GroupRelation}, {OwnerId: users[1].ID, TargetId: group.ID, Type: models.GroupRelation}}
	if err := db.WithContext(ctx).Create(&relations).Error; err != nil {
		t.Fatal(err)
	}
	repo, _ := database.NewMessageRepository(pool)
	var private, groupID string
	for i := 1; i <= 9; i++ {
		kind, target := messaging.Private, uint64(users[1].ID)
		if i == 9 {
			kind, target = messaging.Group, uint64(group.ID)
		}
		command, e := messaging.Normalize(uint64(users[0].ID), messaging.SendRequest{ClientMessageID: fmt.Sprintf("ef762b6c-1234-4234-8234-%012d", i), TargetType: kind, TargetID: strconv.FormatUint(target, 10), Media: messaging.Text, Content: fmt.Sprintf("ack-content-%d", i)})
		if e != nil {
			t.Fatal(e)
		}
		if _, e = repo.PersistCommand(ctx, command); e != nil {
			t.Fatal(e)
		}
		if kind == messaging.Private {
			private = command.ConversationID
		} else {
			groupID = command.ConversationID
		}
	}
	reader := uint64(users[1].ID)
	cursor := func(u uint64, id string) (uint64, string) {
		t.Helper()
		var seq uint64
		var stamp string
		e := pool.QueryRowContext(ctx, "SELECT delivered_seq,DATE_FORMAT(updated_at,'%Y-%m-%d %H:%i:%s.%f') FROM message_delivery_cursor WHERE user_id=? AND conversation_id=?", u, id).Scan(&seq, &stamp)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			t.Fatal(e)
		}
		return seq, stamp
	}
	check := func(u uint64, id string, want uint64) {
		t.Helper()
		got, _ := cursor(u, id)
		if got != want {
			t.Fatalf("cursor %s: %d != %d", id, got, want)
		}
	}
	t.Run("zero_lower_duplicate_and_independent_users", func(t *testing.T) {
		for _, seq := range []messaging.Sequence{0, 2, 1, 2} {
			got, e := repo.AcknowledgeDelivery(ctx, reader, private, seq)
			want := seq
			if seq == 1 {
				want = 2
			}
			if e != nil || got != want {
				t.Fatal(got, e)
			}
		}
		_, stamp := cursor(reader, private)
		if _, e := repo.AcknowledgeDelivery(ctx, reader, private, 0); e != nil {
			t.Fatal(e)
		}
		_, again := cursor(reader, private)
		if stamp != again {
			t.Fatal("lower ACK changed timestamp")
		}
		if _, e := repo.AcknowledgeDelivery(ctx, uint64(users[0].ID), private, 1); e != nil {
			t.Fatal(e)
		}
		check(reader, private, 2)
		check(uint64(users[0].ID), private, 1)
		if got, e := repo.AcknowledgeDelivery(ctx, uint64(users[2].ID), private, 1); !errors.Is(e, database.ErrConversationForbidden) || got != 0 {
			t.Fatal(got, e)
		}
		if got, e := repo.AcknowledgeDelivery(ctx, reader, private, 9); !errors.Is(e, database.ErrDeliverySequence) || got != 0 {
			t.Fatal(got, e)
		}
		check(reader, private, 2)
	})
	t.Run("concurrent_reordered_acks_converge_without_regression", func(t *testing.T) {
		var wg sync.WaitGroup
		errs := make(chan error, 32)
		for i := 0; i < 32; i++ {
			seq := messaging.Sequence(i % 9)
			wg.Add(1)
			go func() {
				defer wg.Done()
				got, e := repo.AcknowledgeDelivery(ctx, reader, private, seq)
				if e != nil {
					errs <- e
				} else if got < seq {
					errs <- errors.New("regressed receipt")
				}
			}()
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			t.Error(e)
		}
		check(reader, private, 8)
	})
	t.Run("current_group_permission_lock_and_cancellation", func(t *testing.T) {
		if _, e := repo.AcknowledgeDelivery(ctx, reader, groupID, 0); e != nil {
			t.Fatal(e)
		}
		tx, e := pool.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback()
		if _, e := tx.ExecContext(ctx, "UPDATE relation SET deleted_at=UTC_TIMESTAMP(6) WHERE owner_id=? AND target_id=? AND type=?", reader, group.ID, models.GroupRelation); e != nil {
			t.Fatal(e)
		}
		result := make(chan error, 1)
		go func() { _, e := repo.AcknowledgeDelivery(ctx, reader, groupID, 1); result <- e }()
		lifecycleEventually(t, func() bool {
			var n int
			e := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.processlist WHERE DB=? AND INFO LIKE '%FROM relation%FOR SHARE%'", schema).Scan(&n)
			return e == nil && n > 0
		})
		select {
		case e := <-result:
			t.Fatal("ACK bypassed permission lock", e)
		default:
		}
		if e := tx.Commit(); e != nil {
			t.Fatal(e)
		}
		if e := <-result; !errors.Is(e, database.ErrConversationForbidden) {
			t.Fatal(e)
		}
		check(reader, groupID, 0)
		if _, e := pool.ExecContext(ctx, "UPDATE relation SET deleted_at=NULL WHERE owner_id=? AND target_id=? AND type=?", reader, group.ID, models.GroupRelation); e != nil {
			t.Fatal(e)
		}
		blocked, e := pool.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer blocked.Rollback()
		if _, e := blocked.ExecContext(ctx, "UPDATE relation SET deleted_at=UTC_TIMESTAMP(6) WHERE owner_id=? AND target_id=? AND type=?", reader, group.ID, models.GroupRelation); e != nil {
			t.Fatal(e)
		}
		short, stop := context.WithTimeout(ctx, 150*time.Millisecond)
		got, e := repo.AcknowledgeDelivery(short, reader, groupID, 1)
		stop()
		if e == nil || got != 0 {
			t.Fatal("canceled ACK confirmed", got, e)
		}
		if e := blocked.Rollback(); e != nil {
			t.Fatal(e)
		}
		check(reader, groupID, 0)
	})
	// Proxy SQL is used only by the ACK repository; setup/subscriptions use the
	// direct pool, so arming cannot accidentally consume another transaction.
	proxy := startQuarantineSQLProxy(t, net.JoinHostPort(conf.MYSQL.Host, conf.MYSQL.Port))
	dsn := mysql.NewConfig()
	dsn.Net, dsn.Addr = "tcp", proxy.address
	dsn.User, dsn.Passwd, dsn.DBName = conf.MYSQL.User, conf.MYSQL.Password, conf.MYSQL.DbName
	dsn.ParseTime = true
	dsn.Timeout = time.Second
	dsn.ReadTimeout, dsn.WriteTimeout = 2*time.Second, 2*time.Second
	connector, e := mysql.NewConnector(dsn)
	if e != nil {
		t.Fatal("configure isolated ACK SQL")
	}
	proxied := sql.OpenDB(connector)
	t.Cleanup(func() { proxied.Close() })
	proxyRepo, _ := database.NewMessageRepository(proxied)
	client, e := utils.OpenRedis(ctx, conf)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { client.Close() })
	conf.JWT.Key = "delivery-ack-fixture-" + env.RunID()
	conf.JWT.TokenHourLifeSpan = 1
	tokens, e := utils.NewTokenService(conf, utils.NewRedisTokenBlacklist(client))
	if e != nil {
		t.Fatal(e)
	}
	token, e := tokens.GenerateToken(users[1].ID)
	if e != nil {
		t.Fatal(e)
	}
	source, e := database.NewNotificationSource(pool, client)
	if e != nil {
		t.Fatal(e)
	}
	barrier := &deliveryACKBarrier{repo: proxyRepo}
	chat, e := service.NewReliableChat(source, repo, barrier, service.DefaultReliableChatOptions())
	if e != nil {
		t.Fatal(e)
	}
	r := gin.New()
	if e := router.RegisterReliableChat(r, tokens, chat); e != nil {
		t.Fatal(e)
	}
	lifetime, stopSessions := context.WithCancel(ctx)
	defer stopSessions()
	var handlers sync.WaitGroup
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		handlers.Add(1)
		defer handlers.Done()
		r.ServeHTTP(w, req.WithContext(lifetime))
	}))
	t.Cleanup(func() { stopSessions(); server.Close(); handlers.Wait() })
	dial := func() (*websocket.Conn, string) {
		t.Helper()
		conn, response, e := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/auth/chat", http.Header{"Authorization": []string{"Bearer " + token}})
		if e != nil {
			if response != nil {
				response.Body.Close()
			}
			t.Fatal(e)
		}
		t.Cleanup(func() { conn.Close() })
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var f struct {
			Type string `json:"type"`
			Data struct {
				Session string `json:"sessionId"`
			} `json:"data"`
		}
		if e := conn.ReadJSON(&f); e != nil || f.Type != "ready" {
			t.Fatal(e, f.Type)
		}
		return conn, f.Data.Session
	}
	send := func(conn *websocket.Conn, id string, seq messaging.Sequence) {
		t.Helper()
		if e := conn.WriteJSON(struct {
			Type string                `json:"type"`
			Data messaging.DeliveryACK `json:"data"`
		}{"ack", messaging.DeliveryACK{ConversationID: id, DeliveredSeq: seq}}); e != nil {
			t.Fatal(e)
		}
	}
	confirmed := func(conn *websocket.Conn, id string, want messaging.Sequence) {
		t.Helper()
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		for {
			var f struct {
				Type string          `json:"type"`
				Data json.RawMessage `json:"data"`
			}
			if e := conn.ReadJSON(&f); e != nil {
				t.Fatal(e)
			}
			if f.Type != "ack.confirmed" {
				continue
			}
			var a messaging.DeliveryACK
			if e := json.Unmarshal(f.Data, &a); e != nil || a.ConversationID != id || a.DeliveredSeq != want {
				t.Fatal("invalid committed confirmation", a, e)
			}
			return
		}
	}
	closedWithoutConfirmation := func(conn *websocket.Conn) {
		t.Helper()
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		for {
			var f struct {
				Type string `json:"type"`
			}
			if e := conn.ReadJSON(&f); e != nil {
				if ne, ok := e.(net.Error); ok && ne.Timeout() {
					t.Fatal("session failed to close")
				}
				return
			}
			if f.Type == "ack.confirmed" {
				t.Fatal("failed ACK was confirmed")
			}
		}
	}
	t.Run("production_socket_confirm_after_commit_and_reconnect_replay", func(t *testing.T) {
		conn, _ := dial()
		send(conn, private, 1)
		confirmed(conn, private, 8)
		check(reader, private, 8)
		send(conn, groupID, 0)
		confirmed(conn, groupID, 0)
		check(reader, groupID, 0)
		// Application-level confirmation may be lost after SQL committed.
		conn.Close()
		again, _ := dial()
		send(again, groupID, 0)
		confirmed(again, groupID, 0)
		again.Close()
	})
	t.Run("real_commit_OK_lost_no_confirmation_and_same_ACK_recovers", func(t *testing.T) {
		conn, _ := dial()
		proxy.armed.Store(true)
		send(conn, groupID, 1)
		closedWithoutConfirmation(conn)
		if proxy.dropped.Load() != 1 {
			t.Fatal("actual SQL OK not intercepted")
		}
		check(reader, groupID, 1)
		again, _ := dial()
		send(again, groupID, 1)
		confirmed(again, groupID, 1)
		send(again, groupID, 0)
		confirmed(again, groupID, 1)
		again.Close()
		check(reader, groupID, 1)
	})
	t.Run("replacement_waits_for_admitted_ACK_and_rejects_old_socket", func(t *testing.T) {
		old, oldID := dial()
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		defer unblock()
		barrier.mu.Lock()
		barrier.before = func(context.Context) { close(entered); <-release }
		barrier.mu.Unlock()
		send(old, private, 1)
		lifecycleAwait(t, entered)
		type connection struct {
			socket *websocket.Conn
			id     string
			err    error
		}
		fresh := make(chan connection, 1)
		go func() {
			conn, response, e := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/auth/chat", http.Header{"Authorization": []string{"Bearer " + token}})
			if e != nil {
				if response != nil {
					response.Body.Close()
				}
				fresh <- connection{err: e}
				return
			}
			var f struct {
				Data struct {
					Session string `json:"sessionId"`
				} `json:"data"`
			}
			conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			e = conn.ReadJSON(&f)
			fresh <- connection{conn, f.Data.Session, e}
		}()
		select {
		case v := <-fresh:
			if v.socket != nil {
				v.socket.Close()
			}
			t.Fatal("replacement crossed in-flight ACK", v.err)
		case <-time.After(50 * time.Millisecond):
		}
		unblock()
		var next connection
		select {
		case next = <-fresh:
		case <-time.After(4 * time.Second):
			t.Fatal("replacement did not complete")
		}
		if next.err != nil || next.id == oldID {
			t.Fatal("replacement failed", next.err)
		}
		defer next.socket.Close()
		// The old connection can have a confirmation serialized before cutover;
		// after ready on the replacement, it can no longer submit new ACKs.
		before := barrier.calls.Load()
		_ = old.WriteJSON(map[string]any{"type": "ack", "data": messaging.DeliveryACK{ConversationID: private, DeliveredSeq: 8}})
		send(next.socket, private, 0)
		confirmed(next.socket, private, 8)
		check(reader, private, 8)
		if barrier.calls.Load() != before+1 {
			t.Fatal("old socket reached ACK repository")
		}
		old.SetReadDeadline(time.Now().Add(time.Second))
		for {
			_, _, e := old.ReadMessage()
			if e != nil {
				if ne, ok := e.(net.Error); ok && ne.Timeout() {
					t.Fatal("old socket remained open")
				}
				break
			}
		}
	})
	t.Run("late_callback_after_deadline_keeps_owner_and_suppresses_receipt", func(t *testing.T) {
		conn, _ := dial()
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		defer unblock()
		var returned atomic.Bool
		barrier.mu.Lock()
		barrier.after = func(c context.Context) { close(entered); <-c.Done(); <-release; returned.Store(true) }
		barrier.mu.Unlock()
		send(conn, private, 1)
		lifecycleAwait(t, entered)
		check(reader, private, 8)
		// Keep the actual callback alive past the production resolve deadline.
		timer := time.NewTimer(2100 * time.Millisecond)
		defer timer.Stop()
		<-timer.C
		if returned.Load() {
			t.Fatal("callback escaped owner")
		}
		unblock()
		closedWithoutConfirmation(conn)
	})
	t.Run("unauthorized_group_and_forged_identity_close_without_updates", func(t *testing.T) {
		if _, e := pool.ExecContext(ctx, "UPDATE relation SET deleted_at=UTC_TIMESTAMP(6) WHERE owner_id=? AND target_id=? AND type=?", reader, group.ID, models.GroupRelation); e != nil {
			t.Fatal(e)
		}
		conn, _ := dial()
		send(conn, groupID, 1)
		closedWithoutConfirmation(conn)
		check(reader, groupID, 1)
		conn, _ = dial()
		if e := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"ack","data":{"conversationId":"`+private+`","deliveredSeq":"0","userId":"1"}}`)); e != nil {
			t.Fatal(e)
		}
		closedWithoutConfirmation(conn)
		conn, _ = dial()
		send(conn, private, 9)
		closedWithoutConfirmation(conn)
		check(reader, private, 8)
	})
	stopSessions()
	server.Close()
	handlers.Wait()
	for table, want := range map[string]int{"message": 9, "message_command": 9, "conversation": 2, "message_outbox": 18, "message_delivery_cursor": 3, "message_quarantine": 0} {
		var n int
		if e := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); e != nil || n != want {
			t.Fatal("unexpected final collection", table, n, e)
		}
	}
	var pending int
	if e := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM message_outbox WHERE status='pending' AND attempts=0").Scan(&pending); e != nil || pending != 18 {
		t.Fatal("ACK changed Outbox responsibility", pending, e)
	}
	if e := migrations.ApplyReliable(ctx, pool); e != nil {
		t.Fatal("schema changed", e)
	}
}
