package service

import (
	"context"
	"encoding/json"
	"errors"
	"iChat/database"
	"iChat/models"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
)

func quietChatLogger() *logrus.Logger { l := logrus.New(); l.SetOutput(io.Discard); return l }

func TestChatBeforeUpgrade(t *testing.T) {
	for _, scenario := range []string{"missing_store", "missing_offline", "nil_redis", "private_failure", "group_failure", "canceled", "upgrade_failure", "no_identity"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p := &testSubscription{messages: make(chan *redis.Message)}
			g := &testSubscription{messages: make(chan *redis.Message)}
			stub := &subscriptionStub{private: p, groups: g}
			var provider MessageSubscriptions = stub
			offlineCalls := 0
			offline := func(uint) { offlineCalls++ }
			uid := uint(42)
			wantPrivate, wantGroup := 1, 1
			switch scenario {
			case "missing_store":
				provider = nil
				wantPrivate, wantGroup = 0, 0
			case "missing_offline":
				offline = nil
				wantPrivate, wantGroup = 0, 0
			case "nil_redis":
				provider = database.NewMessageManager(nil, nil)
				wantPrivate, wantGroup = 0, 0
			case "private_failure":
				stub.privateErr = errors.New("private SQL detail")
				wantGroup = 0
			case "group_failure":
				stub.groupErr = errors.New("private SQL detail")
			case "canceled":
				cancel()
			case "no_identity":
				uid = 0
				wantPrivate, wantGroup = 0, 0
			}
			response := contactRequest(Chat(provider, offline, quietChatLogger()), `{}`, ctx, uid)
			switch scenario {
			case "upgrade_failure":
				if response.Code != http.StatusBadRequest {
					t.Fatalf("upgrade status=%d", response.Code)
				}
			case "no_identity":
				if response.Code != http.StatusUnauthorized {
					t.Fatalf("identity status=%d", response.Code)
				}
			default:
				assertAccountUnavailable(t, response)
			}
			if offlineCalls != 0 || p.closed != wantPrivate || g.closed != wantGroup {
				t.Fatalf("offline=%d private=%d group=%d", offlineCalls, p.closed, g.closed)
			}
			if response.Header().Get("Upgrade") != "" {
				t.Fatal("failed setup upgraded socket")
			}
		})
	}
}

type socketSubscription struct {
	messages chan *redis.Message
	closed   chan struct{}
	once     sync.Once
}

func newSocketSubscription() *socketSubscription {
	return &socketSubscription{messages: make(chan *redis.Message, 4), closed: make(chan struct{})}
}
func (s *socketSubscription) Channel() <-chan *redis.Message { return s.messages }
func (s *socketSubscription) Close() error                   { s.once.Do(func() { close(s.closed) }); return nil }

type socketPair struct {
	private, groups *socketSubscription
	ctx             context.Context
	uid             uint
}
type socketProvider struct {
	mu          sync.Mutex
	pending     map[context.Context]*socketPair
	created     chan *socketPair
	emptyGroups bool
}

func (s *socketProvider) Subscribe(ctx context.Context, uid uint) (database.Subscription, error) {
	pair := &socketPair{private: newSocketSubscription(), groups: newSocketSubscription(), ctx: ctx, uid: uid}
	if s.emptyGroups {
		pair.groups.messages = nil
	}
	s.mu.Lock()
	s.pending[ctx] = pair
	s.mu.Unlock()
	return pair.private, nil
}
func (s *socketProvider) SubscribeGroups(ctx context.Context, uid uint) (database.Subscription, error) {
	s.mu.Lock()
	pair := s.pending[ctx]
	delete(s.pending, ctx)
	s.mu.Unlock()
	if pair == nil || pair.uid != uid {
		return nil, errors.New("context/identity mismatch")
	}
	s.created <- pair
	return pair.groups, nil
}

func waitSocketEvent(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("WebSocket cleanup did not finish")
	}
}
func assertSocketResourcesClosed(t *testing.T, pair *socketPair) {
	t.Helper()
	waitSocketEvent(t, pair.private.closed)
	waitSocketEvent(t, pair.groups.closed)
	if pair.ctx.Err() == nil {
		t.Fatal("subscription context still active")
	}
}
func readSocketControl(t *testing.T, conn *websocket.Conn) models.CtrlMsg {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var got models.CtrlMsg
	if err := conn.ReadJSON(&got); err != nil {
		t.Fatal(err)
	}
	return got
}
func sendSocketFixture(t *testing.T, pair *socketPair, conn *websocket.Conn, group bool) {
	t.Helper()
	msg := models.Message{Id: "ws-fixture", SenderId: 7, ReceiverId: 42, Type: models.PrivateType, Content: "hello", Media: models.PictureMedia}
	sub, kind := pair.private, "simple"
	if group {
		msg.Type = models.GroupType
		sub, kind = pair.groups, "group"
	}
	raw, _ := json.Marshal(msg)
	sub.messages <- &redis.Message{Payload: string(raw)}
	got := readSocketControl(t, conn)
	if got.Type != kind {
		t.Fatalf("control type=%s", got.Type)
	}
	data, _ := json.Marshal(got.Data)
	var info models.MsgInfo
	_ = json.Unmarshal(data, &info)
	if info.Id != msg.Id || info.Content != msg.Content || info.Type != "picture" || info.FromUser.Id != 7 {
		t.Fatalf("message changed: %+v", info)
	}
}

func TestChatSocketLifecycle(t *testing.T) {
	oldMessages, oldUsers := database.Mmanager, database.Umanager
	database.Mmanager, database.Umanager = nil, nil
	t.Cleanup(func() { database.Mmanager, database.Umanager = oldMessages, oldUsers })
	for _, scenario := range []string{"client_close", "subscription_close", "request_cancel", "replacement", "logout", "empty_groups"} {
		t.Run(scenario, func(t *testing.T) {
			provider := &socketProvider{pending: make(map[context.Context]*socketPair), created: make(chan *socketPair, 2), emptyGroups: scenario == "empty_groups"}
			var offline atomic.Int32
			done := make(chan struct{}, 2)
			cancellers := make(chan context.CancelFunc, 2)
			handler := Chat(provider, func(uid uint) {
				if uid != 42 {
					t.Error("wrong offline UID")
				}
				offline.Add(1)
			}, quietChatLogger())
			r := gin.New()
			r.GET("/", func(c *gin.Context) {
				ctx, cancel := context.WithCancel(c.Request.Context())
				defer cancel()
				c.Request = c.Request.WithContext(ctx)
				cancellers <- cancel
				c.Set("uid", uint(42))
				handler(c)
				done <- struct{}{}
			})
			server := httptest.NewServer(r)
			t.Cleanup(server.Close)
			connect := func() (*websocket.Conn, *socketPair) {
				t.Helper()
				conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = conn.Close() })
				var pair *socketPair
				select {
				case pair = <-provider.created:
				case <-time.After(time.Second):
					t.Fatal("no subscriptions")
				}
				sendSocketFixture(t, pair, conn, false)
				return conn, pair
			}
			conn, pair := connect()
			cancel := <-cancellers
			if scenario != "empty_groups" {
				sendSocketFixture(t, pair, conn, true)
			}
			switch scenario {
			case "replacement":
				newConn, newPair := connect()
				waitSocketEvent(t, done)
				assertSocketResourcesClosed(t, pair)
				if offline.Load() != 0 {
					t.Fatal("old session marked replacement offline")
				}
				sendSocketFixture(t, newPair, newConn, false)
				_ = newConn.Close()
				waitSocketEvent(t, done)
				assertSocketResourcesClosed(t, newPair)
			case "request_cancel":
				cancel()
				waitSocketEvent(t, done)
				assertSocketResourcesClosed(t, pair)
			case "subscription_close":
				close(pair.private.messages)
				waitSocketEvent(t, done)
				assertSocketResourcesClosed(t, pair)
			case "logout":
				if !disconnectWebsocket(42, []byte(`{"type":"offline"}`)) {
					t.Fatal("disconnect not queued")
				}
				if got := readSocketControl(t, conn); got.Type != "offline" {
					t.Fatalf("disconnect frame=%v", got)
				}
				waitSocketEvent(t, done)
				assertSocketResourcesClosed(t, pair)
			default:
				_ = conn.Close()
				waitSocketEvent(t, done)
				assertSocketResourcesClosed(t, pair)
			}
			if offline.Load() != 1 {
				t.Fatalf("offline calls=%d", offline.Load())
			}
			websocketSessions.RLock()
			remaining := websocketSessions.items[42]
			websocketSessions.RUnlock()
			if remaining != nil {
				t.Fatal("session registry leaked")
			}
		})
	}
}

type blockingChatHook struct {
	entered, release chan struct{}
	once             sync.Once
}

func (*blockingChatHook) Levels() []logrus.Level { return logrus.AllLevels }
func (h *blockingChatHook) Fire(entry *logrus.Entry) error {
	if entry.Message == "unsupported websocket control message" {
		h.once.Do(func() { close(h.entered) })
		<-h.release
	}
	return nil
}

func TestChatWaitsForReaderBeforeOffline(t *testing.T) {
	provider := &socketProvider{pending: make(map[context.Context]*socketPair), created: make(chan *socketPair, 1)}
	logger := quietChatLogger()
	hook := &blockingChatHook{entered: make(chan struct{}), release: make(chan struct{})}
	logger.AddHook(hook)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(hook.release) }) }
	defer release()
	done := make(chan struct{}, 1)
	cancellers := make(chan context.CancelFunc, 1)
	var offline atomic.Int32
	handler := Chat(provider, func(uint) { offline.Add(1) }, logger)
	r := gin.New()
	r.GET("/", func(c *gin.Context) {
		ctx, cancel := context.WithCancel(c.Request.Context())
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		cancellers <- cancel
		c.Set("uid", uint(42))
		handler(c)
		done <- struct{}{}
	})
	server := httptest.NewServer(r)
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Release the controlled reader before server.Close even if an assertion fails.
	defer release()
	pair := <-provider.created
	cancel := <-cancellers
	if err := conn.WriteJSON(models.CtrlMsg{Type: "hold-reader"}); err != nil {
		t.Fatal(err)
	}
	waitSocketEvent(t, hook.entered)
	cancel()
	waitSocketEvent(t, pair.private.closed)
	waitSocketEvent(t, pair.groups.closed)
	select {
	case <-done:
		t.Fatal("handler returned while reader was still blocked")
	case <-time.After(30 * time.Millisecond):
	}
	if offline.Load() != 0 {
		t.Fatal("offline ran before reader exited")
	}
	release()
	waitSocketEvent(t, done)
	if offline.Load() != 1 {
		t.Fatalf("offline calls=%d", offline.Load())
	}
}
