package service

import (
	"context"
	"encoding/json"
	"errors"
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
	"iChat/database"
	"iChat/messaging"
)

type reliableTestSub struct {
	messages chan *redis.Message
	closed   chan struct{}
	once     sync.Once
}

func (s *reliableTestSub) Channel() <-chan *redis.Message { return s.messages }
func (s *reliableTestSub) Close() error                   { s.once.Do(func() { close(s.closed) }); return nil }

type reliableTestSource struct {
	mu       sync.Mutex
	channels []string
	subs     []*reliableTestSub
	fail     bool
	opening  chan struct{}
	release  <-chan struct{}
}

func (s *reliableTestSource) Channels(context.Context, uint64, int) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.channels...), nil
}
func (s *reliableTestSource) Subscribe(context.Context, []string) (database.Subscription, error) {
	sub := &reliableTestSub{messages: make(chan *redis.Message, 32), closed: make(chan struct{})}
	s.mu.Lock()
	s.subs = append(s.subs, sub)
	fail, opening, release := s.fail, s.opening, s.release
	s.mu.Unlock()
	if opening != nil {
		close(opening)
		<-release
	}
	if fail {
		return sub, errors.New("PRIVATE")
	}
	return sub, nil
}
func (s *reliableTestSource) subscription(t *testing.T, index int) *reliableTestSub {
	t.Helper()
	var result *reliableTestSub
	chatEventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.subs) <= index {
			return false
		}
		result = s.subs[index]
		return true
	})
	return result
}
func chatEventually(t *testing.T, f func() bool) {
	t.Helper()
	until := time.Now().Add(2 * time.Second)
	for !f() {
		if time.Now().After(until) {
			t.Fatal("chat state did not converge")
		}
		time.Sleep(time.Millisecond)
	}
}
func chatTestOptions() ReliableChatOptions {
	o := DefaultReliableChatOptions()
	o.SetupTimeout = 200 * time.Millisecond
	o.ResolveTimeout = 100 * time.Millisecond
	o.WriteTimeout = 100 * time.Millisecond
	o.RefreshInterval = 30 * time.Millisecond
	o.HeartbeatInterval = 100 * time.Millisecond
	o.ReadTimeout = time.Second
	o.QueueSize = 2
	return o
}
func chatTestFrame(t *testing.T) (messaging.Notification, messaging.ServerFrame) {
	t.Helper()
	c, e := messaging.Normalize(2, messaging.SendRequest{ClientMessageID: "12345678-1234-4234-8234-123456789abc", TargetType: messaging.Private, TargetID: "1", Media: messaging.Text, Content: "private-chat"})
	if e != nil {
		t.Fatal(e)
	}
	id, _ := messaging.OutboxEventID(c.CommandID, messaging.EventMessagePersisted)
	body, _ := json.Marshal(messaging.MessagePersistedEvent{SchemaVersion: 1, EventID: id, CommandID: c.CommandID, ConversationID: c.ConversationID, MessageID: c.CommandID, Seq: 1, SenderID: c.SenderID, TargetID: c.TargetID, TargetType: c.TargetType})
	n, e := messaging.DecodeOutboxNotification(messaging.OutboxRecord{EventID: id, EventType: messaging.EventMessagePersisted, CommandID: c.CommandID, ConversationID: &c.ConversationID, MessageID: &c.CommandID, Payload: body})
	if e != nil {
		t.Fatal(e)
	}
	m, _ := messaging.NewMessageDTO(c, 1, time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC))
	f, e := messaging.NewMessageFrame(m)
	if e != nil {
		t.Fatal(e)
	}
	return n, f
}
func chatTestServer(t *testing.T, c *ReliableChat, auth func(context.Context) (uint64, error)) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var finished atomic.Int32
	r := gin.New()
	r.GET("/chat", func(g *gin.Context) { defer finished.Add(1); c.Serve(g, auth) })
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)
	return server, &finished
}
func chatDial(t *testing.T, server *httptest.Server) (*websocket.Conn, string) {
	t.Helper()
	conn, response, e := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/chat", nil)
	if e != nil {
		if response != nil {
			response.Body.Close()
		}
		t.Fatal(e)
	}
	t.Cleanup(func() { conn.Close() })
	var ready struct {
		Type string `json:"type"`
		Data struct {
			Protocol int    `json:"protocolVersion"`
			Session  string `json:"sessionId"`
		} `json:"data"`
	}
	conn.SetReadDeadline(time.Now().Add(time.Second))
	if e := conn.ReadJSON(&ready); e != nil || ready.Type != "ready" || ready.Data.Protocol != 1 || len(ready.Data.Session) != 32 {
		t.Fatal("ready not first frame", e)
	}
	return conn, ready.Data.Session
}
func chatReadType(t *testing.T, conn *websocket.Conn, kind string) []byte {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(time.Second))
	for {
		_, body, e := conn.ReadMessage()
		if e != nil {
			t.Fatal(e)
		}
		var f struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(body, &f) != nil {
			t.Fatal("invalid frame")
		}
		if f.Type == kind {
			return body
		}
		if f.Type == "ping" {
			conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"pong","data":{}}`))
		}
	}
}

func TestReliableChatReadyRefreshHeartbeatAndJoin(t *testing.T) {
	source := &reliableTestSource{channels: []string{"ichat:notify:v1:user:1"}}
	n, f := chatTestFrame(t)
	reader := notificationReaderFunc(func(context.Context, uint64, messaging.NotificationHint) (messaging.ServerFrame, error) {
		return f, nil
	})
	c, e := NewReliableChat(source, reader, deliveryAckTestFunc(unusedDeliveryACK), chatTestOptions())
	if e != nil {
		t.Fatal(e)
	}
	server, finished := chatTestServer(t, c, func(context.Context) (uint64, error) { return 1, nil })
	conn, _ := chatDial(t, server)
	first := source.subscription(t, 0)
	first.messages <- &redis.Message{Channel: n.Channel(), Payload: string(n.Body())}
	if string(chatReadType(t, conn, "message")) != string(f.Bytes()) {
		t.Fatal("writer changed canonical frame")
	}
	conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"ping","data":{}}`))
	chatReadType(t, conn, "pong")
	source.mu.Lock()
	source.channels = []string{"ichat:notify:v1:user:1", "ichat:notify:v1:group:5"}
	source.mu.Unlock()
	second := source.subscription(t, 1)
	chatEventually(t, func() bool {
		select {
		case <-first.closed:
			return true
		default:
			return false
		}
	})
	chatReadType(t, conn, "sync.required")
	// Successful setup cancellation must not own the live subscription.
	select {
	case <-second.closed:
		t.Fatal("refresh setup canceled live stream")
	default:
	}
	conn.Close()
	chatEventually(t, func() bool { return finished.Load() == 1 })
	c.mu.Lock()
	count, slots := c.count, len(c.slots)
	c.mu.Unlock()
	if count != 0 || slots != 0 {
		t.Fatal("session registry retained finished owner")
	}
}

func TestReliableChatReplacementFencesLateSQLAndRetainsOldOwner(t *testing.T) {
	source := &reliableTestSource{channels: []string{"ichat:notify:v1:user:1"}}
	n, f := chatTestFrame(t)
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	var calls atomic.Int32
	reader := notificationReaderFunc(func(context.Context, uint64, messaging.NotificationHint) (messaging.ServerFrame, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return f, nil
	})
	c, _ := NewReliableChat(source, reader, deliveryAckTestFunc(unusedDeliveryACK), chatTestOptions())
	server, finished := chatTestServer(t, c, func(context.Context) (uint64, error) { return 1, nil })
	old, id1 := chatDial(t, server)
	first := source.subscription(t, 0)
	first.messages <- &redis.Message{Channel: n.Channel(), Payload: string(n.Body())}
	chatEventually(t, func() bool {
		select {
		case <-entered:
			return true
		default:
			return false
		}
	})
	fresh, id2 := chatDial(t, server)
	if id1 == id2 {
		t.Fatal("replacement reused session ID")
	}
	old.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, e := old.ReadMessage(); e == nil {
		t.Fatal("old connection stayed open")
	}
	if finished.Load() != 0 {
		t.Fatal("late callback owner returned early")
	}
	close(release)
	chatEventually(t, func() bool { return finished.Load() == 1 })
	second := source.subscription(t, 1)
	second.messages <- &redis.Message{Channel: n.Channel(), Payload: string(n.Body())}
	chatReadType(t, fresh, "message")
	fresh.Close()
	chatEventually(t, func() bool { return finished.Load() == 2 })
}

func TestReliableChatQueueSaturationCancelsButJoinsCallbacks(t *testing.T) {
	source := &reliableTestSource{channels: []string{"ichat:notify:v1:user:1"}}
	n, f := chatTestFrame(t)
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	reader := notificationReaderFunc(func(context.Context, uint64, messaging.NotificationHint) (messaging.ServerFrame, error) {
		close(entered)
		<-release
		return f, nil
	})
	c, _ := NewReliableChat(source, reader, deliveryAckTestFunc(unusedDeliveryACK), chatTestOptions())
	server, finished := chatTestServer(t, c, func(context.Context) (uint64, error) { return 1, nil })
	conn, _ := chatDial(t, server)
	sub := source.subscription(t, 0)
	sub.messages <- &redis.Message{Channel: n.Channel(), Payload: string(n.Body())}
	chatEventually(t, func() bool {
		select {
		case <-entered:
			return true
		default:
			return false
		}
	})
	for range 5 {
		sub.messages <- &redis.Message{Channel: n.Channel(), Payload: string(n.Body())}
	}
	chatEventually(t, func() bool {
		select {
		case <-sub.closed:
			return true
		default:
			return false
		}
	})
	if finished.Load() != 0 {
		t.Fatal("slow callback was abandoned")
	}
	close(release)
	chatEventually(t, func() bool { return finished.Load() == 1 })
	conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, e := conn.ReadMessage(); e == nil {
		t.Fatal("saturated session stayed open")
	}
}

func TestReliableChatSetupFailuresAndPartialSubscription(t *testing.T) {
	for _, mode := range []string{"auth", "channels", "subscribe", "late_open"} {
		t.Run(mode, func(t *testing.T) {
			source := &reliableTestSource{channels: []string{"ichat:notify:v1:user:1"}}
			if mode == "channels" {
				source.channels = nil
			}
			if mode == "subscribe" {
				source.fail = true
			}
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			if mode == "late_open" {
				source.opening, source.release = make(chan struct{}), release
			}
			reader := notificationReaderFunc(func(context.Context, uint64, messaging.NotificationHint) (messaging.ServerFrame, error) {
				return messaging.ServerFrame{}, nil
			})
			o := chatTestOptions()
			o.SetupTimeout = 30 * time.Millisecond
			c, _ := NewReliableChat(source, reader, deliveryAckTestFunc(unusedDeliveryACK), o)
			server, finished := chatTestServer(t, c, func(context.Context) (uint64, error) {
				if mode == "auth" {
					return 0, errors.New("invalid")
				}
				return 1, nil
			})
			client := &http.Client{Timeout: 50 * time.Millisecond}
			response, e := client.Get(server.URL + "/chat")
			if mode == "late_open" {
				if e == nil {
					response.Body.Close()
					t.Fatal("late source unexpectedly returned")
				}
				if finished.Load() != 0 {
					t.Fatal("late source owner abandoned")
				}
				unblock()
				chatEventually(t, func() bool { return finished.Load() == 1 })
			} else {
				if e != nil {
					t.Fatal(e)
				}
				response.Body.Close()
				if response.StatusCode < 400 {
					t.Fatal("failed setup upgraded")
				}
				chatEventually(t, func() bool { return finished.Load() == 1 })
			}
			source.mu.Lock()
			subs := append([]*reliableTestSub(nil), source.subs...)
			source.mu.Unlock()
			for _, sub := range subs {
				select {
				case <-sub.closed:
				default:
					t.Fatal("partial subscription not closed")
				}
			}
		})
	}
}

func TestReliableChatRefreshFailureAndUnsupportedFrames(t *testing.T) {
	for _, mode := range []string{"revoked", "partial_refresh", "ack", "duplicate", "binary", "reader_panic"} {
		t.Run(mode, func(t *testing.T) {
			source := &reliableTestSource{channels: []string{"ichat:notify:v1:user:1"}}
			var authCalls atomic.Int32
			c, e := NewReliableChat(source, notificationReaderFunc(func(context.Context, uint64, messaging.NotificationHint) (messaging.ServerFrame, error) {
				if mode == "reader_panic" {
					panic("PRIVATE")
				}
				t.Error("unexpected SQL read")
				return messaging.ServerFrame{}, nil
			}), deliveryAckTestFunc(unusedDeliveryACK), chatTestOptions())
			if e != nil {
				t.Fatal(e)
			}
			server, finished := chatTestServer(t, c, func(context.Context) (uint64, error) {
				if authCalls.Add(1) > 1 && mode == "revoked" {
					return 0, errors.New("revoked")
				}
				return 1, nil
			})
			conn, _ := chatDial(t, server)
			switch mode {
			case "reader_panic":
				n, _ := chatTestFrame(t)
				source.subscription(t, 0).messages <- &redis.Message{Channel: n.Channel(), Payload: string(n.Body())}
			case "partial_refresh":
				source.mu.Lock()
				source.fail = true
				source.channels = append(source.channels, "ichat:notify:v1:group:5")
				source.mu.Unlock()
				source.subscription(t, 1)
			case "ack":
				conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"ack","data":{"deliveredSeq":"1"}}`))
			case "duplicate":
				conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"ping","type":"pong","data":{}}`))
			case "binary":
				conn.WriteMessage(websocket.BinaryMessage, []byte("not supported"))
			}
			chatEventually(t, func() bool { return finished.Load() == 1 })
			source.mu.Lock()
			subs := append([]*reliableTestSub(nil), source.subs...)
			source.mu.Unlock()
			for _, sub := range subs {
				select {
				case <-sub.closed:
				default:
					t.Fatal("failed refresh retained a handle")
				}
			}
		})
	}
}
