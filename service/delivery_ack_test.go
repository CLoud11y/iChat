package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"iChat/messaging"
)

type deliveryAckTestFunc func(context.Context, uint64, string, messaging.Sequence) (messaging.Sequence, error)

func unusedDeliveryACK(context.Context, uint64, string, messaging.Sequence) (messaging.Sequence, error) {
	return 0, errors.New("unexpected ACK")
}
func (f deliveryAckTestFunc) AcknowledgeDelivery(ctx context.Context, u uint64, c string, seq messaging.Sequence) (messaging.Sequence, error) {
	if f == nil {
		return 0, errors.New("unexpected ACK")
	}
	return f(ctx, u, c, seq)
}

func TestDeliveryACKSocketCommitAndFailures(t *testing.T) {
	for _, mode := range []string{"commit", "error", "unknown", "late", "panic", "invalid_receipt"} {
		t.Run(mode, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			source := &reliableTestSource{channels: []string{"ichat:notify:v1:user:1"}}
			var calls atomic.Int32
			acknowledger := deliveryAckTestFunc(func(ctx context.Context, u uint64, id string, seq messaging.Sequence) (messaging.Sequence, error) {
				calls.Add(1)
				if u != 1 || id != "p:1:2" || seq != 2 {
					t.Error("untrusted ACK identity/data")
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Error("missing ACK deadline")
				}
				close(entered)
				<-release
				switch mode {
				case "error", "unknown":
					return 0, errors.New("PRIVATE")
				case "late":
					<-ctx.Done()
					return 3, nil
				case "panic":
					panic("PRIVATE")
				case "invalid_receipt":
					return 1, nil
				}
				return 3, nil // Repeated/lower ACK returns the existing committed value.
			})
			c, err := NewReliableChat(source, notificationReaderFunc(func(context.Context, uint64, messaging.NotificationHint) (messaging.ServerFrame, error) {
				return messaging.ServerFrame{}, nil
			}), acknowledger, chatTestOptions())
			if err != nil {
				t.Fatal(err)
			}
			server, finished := chatTestServer(t, c, func(context.Context) (uint64, error) { return 1, nil })
			conn, _ := chatDial(t, server)
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"ack","data":{"conversationId":"p:1:2","deliveredSeq":"2"}}`)); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("ACK not entered")
			}
			type observed struct {
				body []byte
				err  error
			}
			result := make(chan observed, 1)
			go func() {
				for {
					_, body, err := conn.ReadMessage()
					if err != nil {
						result <- observed{err: err}
						return
					}
					var f struct {
						Type string `json:"type"`
					}
					json.Unmarshal(body, &f)
					if f.Type == "ack.confirmed" {
						result <- observed{body: body}
						return
					}
				}
			}()
			// Actual socket observation while the SQL callback is still blocked.
			select {
			case got := <-result:
				t.Fatal("premature ACK/socket result", got)
			case <-time.After(10 * time.Millisecond):
			}
			unblock()
			var observation observed
			select {
			case observation = <-result:
			case <-time.After(time.Second):
				t.Fatal("ACK/socket did not settle")
			}
			if mode == "commit" {
				body := observation.body
				var got struct {
					Type string                `json:"type"`
					Data messaging.DeliveryACK `json:"data"`
				}
				if err := json.Unmarshal(body, &got); err != nil || observation.err != nil || got.Data.ConversationID != "p:1:2" || got.Data.DeliveredSeq != 3 {
					t.Fatal(string(body), err)
				}
				conn.Close()
			} else {
				if observation.err == nil {
					t.Fatal("false confirmation", string(observation.body))
				}
			}
			chatEventually(t, func() bool { return finished.Load() == 1 })
			if calls.Load() != 1 {
				t.Fatal(calls.Load())
			}
		})
	}
}

func TestDeliveryACKSessionGateAndLateOwner(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slot := &reliableSlot{gate: make(chan struct{}, 1)}
	old := &reliableSession{ctx: ctx, cancel: cancel, id: "old", user: 1, slot: slot, queue: make(chan reliableOutbound, 2)}
	slot.current = old
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	c := &ReliableChat{options: DefaultReliableChatOptions(), acknowledger: deliveryAckTestFunc(func(context.Context, uint64, string, messaging.Sequence) (messaging.Sequence, error) {
		calls.Add(1)
		close(entered)
		<-release
		return 1, nil
	})}
	done := make(chan error, 1)
	go func() {
		done <- c.acknowledge(old, "old", messaging.DeliveryACK{ConversationID: "p:1:2", DeliveredSeq: 1})
	}()
	<-entered
	// Activation cannot pass an ACK's outstanding SQL/COMMIT callback.
	select {
	case slot.gate <- struct{}{}:
		<-slot.gate
		t.Fatal("replacement crossed ACK transaction")
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	slot.gate <- struct{}{}
	slot.mu.Lock()
	slot.current = &reliableSession{id: "new"}
	slot.mu.Unlock()
	<-slot.gate
	if err := c.acknowledge(old, "old", messaging.DeliveryACK{ConversationID: "p:1:2", DeliveredSeq: 1}); err == nil {
		t.Fatal("replaced session accepted")
	}
	slot.mu.Lock()
	slot.current = old
	slot.mu.Unlock()
	if err := c.acknowledge(old, "new", messaging.DeliveryACK{ConversationID: "p:1:2", DeliveredSeq: 1}); err == nil {
		t.Fatal("foreign session id accepted")
	}
	if calls.Load() != 1 {
		t.Fatal("old/foreign session reached SQL")
	}
}

func TestDeliveryACKRequiresRepository(t *testing.T) {
	source := &reliableTestSource{channels: []string{"ichat:notify:v1:user:1"}}
	reader := notificationReaderFunc(func(context.Context, uint64, messaging.NotificationHint) (messaging.ServerFrame, error) {
		return messaging.ServerFrame{}, nil
	})
	for _, acknowledger := range []DeliveryAcknowledger{nil, deliveryAckTestFunc(nil)} {
		if c, e := NewReliableChat(source, reader, acknowledger, DefaultReliableChatOptions()); !errors.Is(e, ErrReliableChatInput) || c != nil {
			t.Fatal("missing ACK dependency accepted", e)
		}
	}
}
