package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/gorilla/websocket"
	"iChat/database"
	"iChat/messaging"
	"iChat/service"
)

type apiChatSource struct{}
type apiChatSubscription struct {
	channel chan *redis.Message
	once    sync.Once
}

func (*apiChatSource) Channels(context.Context, uint64, int) ([]string, error) {
	return []string{"ichat:notify:v1:user:42"}, nil
}
func (*apiChatSource) Subscribe(context.Context, []string) (database.Subscription, error) {
	return &apiChatSubscription{channel: make(chan *redis.Message)}, nil
}
func (s *apiChatSubscription) Channel() <-chan *redis.Message { return s.channel }
func (s *apiChatSubscription) Close() error                   { s.once.Do(func() { close(s.channel) }); return nil }

type apiChatReader struct{}

func (apiChatReader) AcknowledgeDelivery(context.Context, uint64, string, messaging.Sequence) (messaging.Sequence, error) {
	return 0, errors.New("unexpected fixture ACK")
}

func (apiChatReader) ResolveNotification(context.Context, uint64, messaging.NotificationHint) (messaging.ServerFrame, error) {
	return messaging.ServerFrame{}, nil
}
func apiFixtureChat(apiSQL, apiRedis) (*service.ReliableChat, error) {
	return service.NewReliableChat(&apiChatSource{}, apiChatReader{}, apiChatReader{}, service.DefaultReliableChatOptions())
}

type apiChatBlockedReader struct {
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (r *apiChatBlockedReader) ResolveNotification(context.Context, uint64, messaging.NotificationHint) (messaging.ServerFrame, error) {
	r.once.Do(func() { close(r.entered) })
	<-r.release
	return messaging.ServerFrame{}, nil
}

type apiChatCapturedSource struct {
	sub   *apiChatSubscription
	calls atomic.Int32
}

func (s *apiChatCapturedSource) Channels(context.Context, uint64, int) ([]string, error) {
	return []string{"ichat:notify:v1:user:42"}, nil
}
func (s *apiChatCapturedSource) Subscribe(context.Context, []string) (database.Subscription, error) {
	s.calls.Add(1)
	return s.sub, nil
}

func TestAPIChatRouteAuthenticationAndPendingOwnership(t *testing.T) {
	h := newAPIHarness()
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	reader := &apiChatBlockedReader{entered: make(chan struct{}), release: release}
	source := &apiChatCapturedSource{sub: &apiChatSubscription{channel: make(chan *redis.Message, 2)}}
	h.d.chat = func(apiSQL, apiRedis) (*service.ReliableChat, error) {
		return service.NewReliableChat(source, reader, apiChatReader{}, service.DefaultReliableChatOptions())
	}
	s := h.service(t)
	if err := s.Start(context.Background(), context.Background()); err != nil {
		t.Fatal(err)
	}
	base := "http://" + apiAddress(s)
	response, err := http.Get(base + "/auth/chat?token=" + apiToken(t, h))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 || source.calls.Load() != 0 {
		t.Fatal("query token reached subscriptions")
	}
	headers := http.Header{"Authorization": []string{"Bearer " + apiToken(t, h)}}
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(base, "http")+"/auth/chat", headers)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(time.Second))
	var ready map[string]any
	if err := conn.ReadJSON(&ready); err != nil || ready["type"] != "ready" {
		t.Fatal(err)
	}
	c, _ := messaging.Normalize(43, messaging.SendRequest{ClientMessageID: "12345678-1234-4234-8234-123456789abc", TargetType: messaging.Private, TargetID: "42", Media: messaging.Text, Content: "hint"})
	id, _ := messaging.OutboxEventID(c.CommandID, messaging.EventMessagePersisted)
	body := []byte(`{"schemaVersion":1,"eventType":"message.persisted","eventId":"` + id + `","commandId":"` + c.CommandID + `","conversationId":"` + c.ConversationID + `","messageId":"` + c.CommandID + `","seq":"1"}`)
	source.sub.channel <- &redis.Message{Channel: "ichat:notify:v1:user:42", Payload: string(body)}
	apiWait(t, reader.entered)
	observer, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Close(observer); !errors.Is(err, errAPIPending) {
		t.Fatal("pending request owner lost", err)
	}
	if h.sql.closed.Load() != 0 || h.redis.closed.Load() != 0 {
		t.Fatal("API closed clients with SQL callback active")
	}
	unblock()
	apiWait(t, s.Done())
	if h.sql.closed.Load() != 1 || h.redis.closed.Load() != 1 {
		t.Fatal("API did not close clients after join")
	}
}

type apiChatBlockedACK struct {
	entered chan struct{}
	release <-chan struct{}
}

func (a *apiChatBlockedACK) AcknowledgeDelivery(ctx context.Context, user uint64, id string, seq messaging.Sequence) (messaging.Sequence, error) {
	close(a.entered)
	<-a.release // Deliberately ignores cancellation to exercise the API owner.
	return seq, nil
}

func TestAPIChatACKPendingOwnership(t *testing.T) {
	h := newAPIHarness()
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	ack := &apiChatBlockedACK{entered: make(chan struct{}), release: release}
	h.d.chat = func(apiSQL, apiRedis) (*service.ReliableChat, error) {
		return service.NewReliableChat(&apiChatSource{}, apiChatReader{}, ack, service.DefaultReliableChatOptions())
	}
	s := h.service(t)
	if err := s.Start(context.Background(), context.Background()); err != nil {
		t.Fatal(err)
	}
	headers := http.Header{"Authorization": []string{"Bearer " + apiToken(t, h)}}
	conn, _, err := websocket.DefaultDialer.Dial("ws://"+apiAddress(s)+"/auth/chat", headers)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(time.Second))
	var ready map[string]any
	if err := conn.ReadJSON(&ready); err != nil || ready["type"] != "ready" {
		t.Fatal(ready, err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"ack","data":{"conversationId":"p:42:43","deliveredSeq":"1"}}`)); err != nil {
		t.Fatal(err)
	}
	apiWait(t, ack.entered)
	observer, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Close(observer); !errors.Is(err, errAPIPending) {
		t.Fatal("ACK owner lost", err)
	}
	if h.sql.closed.Load() != 0 || h.redis.closed.Load() != 0 {
		t.Fatal("clients closed while ACK callback active")
	}
	unblock()
	apiWait(t, s.Done())
	if h.sql.closed.Load() != 1 || h.redis.closed.Load() != 1 {
		t.Fatal("clients not released after ACK joined")
	}
	for {
		var frame struct {
			Type string `json:"type"`
		}
		if err := conn.ReadJSON(&frame); err != nil {
			break
		}
		if frame.Type == "ack.confirmed" {
			t.Fatal("canceled ACK callback confirmed")
		}
	}
}
