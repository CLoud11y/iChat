//go:build integration

package testenv

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
)

func TestRabbitMQPublisherIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env, conf := startRabbitFixture(t, ctx)
	m, err := rabbitmq.NewManagement("http://"+conf.ManagementAddress, rabbitAdminUser, env.password)
	if err != nil {
		t.Fatal(err)
	}
	setup := openPublisherConnection(t, ctx, conf)
	ch, err := setup.Channel()
	if err != nil {
		t.Fatal(err)
	}
	if err := rabbitmq.EnsureLocal(ctx, ch, m, conf.VHost); err != nil {
		t.Fatal(err)
	}
	c, err := messaging.Normalize(12, messaging.SendRequest{ClientMessageID: "8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1", TargetType: messaging.Private, TargetID: "34", Media: messaging.Text, Content: "publisher:" + env.RunID() + ":中🙂\x00"})
	if err != nil {
		t.Fatal(err)
	}
	newPool := func(t *testing.T, slots int, epoch uint64) *rabbitmq.Publisher {
		t.Helper()
		conn := openPublisherConnection(t, ctx, conf)
		p, err := rabbitmq.NewPublisher(conn, rabbitmq.PublisherOptions{Slots: slots, ConnectionEpoch: epoch})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		return p
	}
	read := func(t *testing.T, expected map[string]rabbitmq.PublishResult) {
		t.Helper()
		for len(expected) > 0 {
			d := getTopologyDelivery(t, ctx, ch, rabbitmq.PersistQueue)
			r, ok := expected[d.CorrelationId]
			if !ok {
				t.Fatal("unconfirmed or duplicate attempt in queue")
			}
			delete(expected, d.CorrelationId)
			e, err := messaging.DecodeEnvelope(d.Body)
			if err != nil || e.CommandID != c.CommandID || e.PayloadHash != c.PayloadHash || e.Content != c.Content || e.PublishAttemptID != r.PublishAttemptID || d.MessageId != r.CommandID || d.ContentType != "application/json" || d.DeliveryMode != amqp.Persistent {
				t.Fatal("published identity or body changed", err)
			}
			if err := d.Ack(false); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Run("concurrent_slots_and_fresh_retry_attempts", func(t *testing.T) {
		p := newPool(t, 2, 1)
		var wg sync.WaitGroup
		const count = 12
		results := make(chan rabbitmq.PublishResult, count)
		failures := make(chan error, count)
		for i := 0; i < count; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					r, err := p.Publish(ctx, c, time.Now())
					if errors.Is(err, rabbitmq.ErrPublisherBusy) {
						select {
						case <-ctx.Done():
							failures <- ctx.Err()
							return
						case <-time.After(time.Millisecond):
							continue
						}
					}
					if err != nil {
						failures <- err
						return
					}
					results <- r
					return
				}
			}()
		}
		wg.Wait()
		close(results)
		close(failures)
		for err := range failures {
			t.Fatal(err)
		}
		expected := map[string]rabbitmq.PublishResult{}
		for r := range results {
			if r.Outcome != rabbitmq.PublishAccepted || r.ConnectionEpoch != 1 || r.ChannelEpoch < 1 || r.ChannelEpoch > 2 || r.CommandID != c.CommandID || r.DeliveryTag == 0 {
				t.Fatal("invalid acceptance", r)
			}
			if _, exists := expected[r.PublishAttemptID]; exists {
				t.Fatal("retry reused publish attempt")
			}
			expected[r.PublishAttemptID] = r
		}
		if len(expected) != count {
			t.Fatal("concurrent attempt missing")
		}
		read(t, expected)
	})
	t.Run("mandatory_return_ack_never_accepted_and_slot_retired", func(t *testing.T) {
		p := newPool(t, 1, 2)
		if err := ch.QueueUnbind(rabbitmq.PersistQueue, rabbitmq.SubmitRoutingKey, rabbitmq.CommandsExchange, nil); err != nil {
			t.Fatal(err)
		}
		r, err := p.Publish(ctx, c, time.Now())
		if !errors.Is(err, rabbitmq.ErrPublisherReturn) || r.Outcome != rabbitmq.PublishUnroutable {
			t.Fatal("unroutable return+ACK accepted", r, err)
		}
		if err := ch.QueueBind(rabbitmq.PersistQueue, rabbitmq.SubmitRoutingKey, rabbitmq.CommandsExchange, false, nil); err != nil {
			t.Fatal(err)
		}
		if r, err := p.Publish(ctx, c, time.Now()); !errors.Is(err, rabbitmq.ErrPublisherUnavailable) || r.Outcome != rabbitmq.PublishNotStarted {
			t.Fatal("returned channel reused", r, err)
		}
		if _, ok, err := ch.Get(rabbitmq.PersistQueue, false); err != nil || ok {
			t.Fatal("unroutable attempt or retired slot left a message", err)
		}
		fresh := newPool(t, 1, 3)
		r, err = fresh.Publish(ctx, c, time.Now())
		if err != nil || r.Outcome != rabbitmq.PublishAccepted || r.ConnectionEpoch != 3 || r.DeliveryTag != 1 {
			t.Fatal("explicit new session could not publish", r, err)
		}
		read(t, map[string]rabbitmq.PublishResult{r.PublishAttemptID: r})
	})
	t.Run("reject_publish_nack_preserves_previously_confirmed_messages", func(t *testing.T) {
		// Fault injection only: shrink the limit in this owned test vhost.
		// This intentionally fails P3.2's policy-free topology contract and is
		// never an initialization shortcut or production capacity setting.
		path := "/api/operator-policies/" + url.PathEscape(conf.VHost) + "/ichat.test.capacity"
		if _, err := env.rabbitAPI(ctx, http.MethodPut, path, map[string]any{"pattern": `^ichat\.message\.persist\.v1$`, "apply-to": "quorum_queues", "priority": 100, "definition": map[string]any{"max-length": 1}}, nil); err != nil {
			t.Fatal(err)
		}
		p := newPool(t, 1, 4)
		expected := map[string]rabbitmq.PublishResult{}
		nacked := false
		for i := 0; i < 8; i++ {
			r, err := p.Publish(ctx, c, time.Now())
			if errors.Is(err, rabbitmq.ErrPublisherNack) {
				if r.Outcome != rabbitmq.PublishNacked {
					t.Fatal("NACK classified accepted")
				}
				nacked = true
				break
			}
			if err != nil || r.Outcome != rabbitmq.PublishAccepted {
				t.Fatal("unexpected capacity result", r, err)
			}
			expected[r.PublishAttemptID] = r
		}
		if !nacked || len(expected) == 0 {
			t.Fatal("broker did not enforce reject-publish after accepting earlier messages")
		}
		if r, err := p.Publish(ctx, c, time.Now()); !errors.Is(err, rabbitmq.ErrPublisherUnavailable) || r.Outcome != rabbitmq.PublishNotStarted {
			t.Fatal("NACK slot reused", r, err)
		}
		read(t, expected)
		if _, err := env.rabbitAPI(ctx, http.MethodDelete, path, nil, nil); err != nil {
			t.Fatal(err)
		}
		if err := rabbitmq.VerifyLocal(ctx, m, conf.VHost); err != nil {
			t.Fatal(err)
		}
	})
}

func openPublisherConnection(t *testing.T, ctx context.Context, conf RabbitMQConfig) *amqp.Connection {
	t.Helper()
	var socket net.Conn
	conn, err := amqp.DialConfig(conf.AMQPURL(), amqp.Config{Recovery: nil, Heartbeat: 2 * time.Second, Dial: func(network, address string) (net.Conn, error) {
		var err error
		socket, err = (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		if err := socket.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
			socket.Close()
			return nil, err
		}
		return socket, nil
	}})
	if err != nil {
		if socket != nil {
			socket.Close()
		}
		t.Fatal(err)
	}
	deadline, _ := ctx.Deadline()
	// AMQP open clears handshake deadlines. Bound subsequent fixture RPCs.
	if err := socket.SetDeadline(deadline); err != nil {
		socket.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.CloseDeadline(time.Now().Add(3 * time.Second)); err != nil && !errors.Is(err, amqp.ErrClosed) {
			t.Error(err)
		}
		socket.Close()
	})
	return conn
}
