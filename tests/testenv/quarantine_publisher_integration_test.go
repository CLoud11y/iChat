//go:build integration

package testenv

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
)

func TestRabbitMQQuarantinePublisherIntegration(t *testing.T) {
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
	q, err := rabbitmq.NewQuarantineEnvelope(rabbitmq.ConsumerMessage{Body: []byte{0xff, 0, '{'},
		MessageID: "broken-" + env.RunID(), CorrelationID: "source-attempt", Exchange: rabbitmq.CommandsExchange, RoutingKey: rabbitmq.SubmitRoutingKey,
		Headers: amqp.Table{"binary": []byte{0, 255}, "large": int64(9007199254740993), "nested": amqp.Table{"array": []any{true, amqp.Decimal{Scale: 2, Value: -12}}}}}, "INVALID_DELIVERY", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	options := rabbitmq.DefaultSessionOptions()
	options.Publisher.Slots = 2
	options.OperationTimeout = time.Second
	options.WriteTimeout = 300 * time.Millisecond
	options.CloseTimeout = 300 * time.Millisecond
	dial := func(t *testing.T, address string, epoch uint64, o rabbitmq.SessionOptions) *rabbitmq.QuarantinePublisherSession {
		t.Helper()
		o.Publisher.ConnectionEpoch = epoch
		s, err := rabbitmq.DialQuarantinePublisher(ctx, address, o)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := s.Close(context.Background()); err != nil {
				t.Error(err)
			}
		})
		return s
	}
	read := func(t *testing.T, expected map[string]rabbitmq.QuarantinePublishResult) {
		t.Helper()
		for len(expected) > 0 {
			d := getTopologyDelivery(t, ctx, ch, rabbitmq.QuarantineQueue)
			r, ok := expected[d.CorrelationId]
			if !ok {
				t.Fatal("unexpected/duplicate target attempt")
			}
			delete(expected, d.CorrelationId)
			decoded, err := rabbitmq.DecodeQuarantineEnvelope(d.Body)
			if err != nil || decoded.TransportID() != q.TransportID() || d.MessageId != r.TransportID || d.ContentType != rabbitmq.QuarantineContentType || d.Type != rabbitmq.QuarantineMessageType || d.DeliveryMode != amqp.Persistent || !bytes.Equal(d.Body, q.Encode()) || d.Expiration != "" {
				t.Fatal("target audit snapshot changed", err)
			}
			if err := d.Ack(false); err != nil {
				t.Fatal(err)
			} // Fixture cleanup only; no business audit worker.
		}
	}
	t.Run("concurrent_slots_stable_snapshot_and_independent_api", func(t *testing.T) {
		s := dial(t, conf.AMQPURL(), 1, options)
		const count = 8
		var wg sync.WaitGroup
		results := make(chan rabbitmq.QuarantinePublishResult, count)
		failures := make(chan error, count)
		for i := 0; i < count; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					r, err := s.Publish(ctx, q)
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
		expected := map[string]rabbitmq.QuarantinePublishResult{}
		for r := range results {
			if r.Outcome != rabbitmq.PublishAccepted || r.TransportID != q.TransportID() || r.ConnectionEpoch != 1 || r.ChannelEpoch < 1 || r.ChannelEpoch > 2 || r.DeliveryTag == 0 || expected[r.PublishAttemptID].TransportID != "" {
				t.Fatal("invalid transfer confirmation", r)
			}
			expected[r.PublishAttemptID] = r
		}
		if len(expected) != count {
			t.Fatal("missing transfer")
		}
		read(t, expected)
		api, err := rabbitmq.DialPublisher(ctx, conf.AMQPURL(), options)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := api.Close(context.Background()); err != nil {
				t.Error(err)
			}
		})
		if err := s.Close(ctx); err != nil {
			t.Fatal(err)
		}
		command, err := messaging.Normalize(12, messaging.SendRequest{ClientMessageID: "8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1", TargetType: messaging.Private, TargetID: "34", Media: messaging.Text, Content: "independent-api"})
		if err != nil {
			t.Fatal(err)
		}
		r, err := api.Publish(ctx, command, time.Now())
		if err != nil || r.Outcome != rabbitmq.PublishAccepted {
			t.Fatal("quarantine shutdown affected API", r, err)
		}
		d := getTopologyDelivery(t, ctx, ch, rabbitmq.PersistQueue)
		if d.CorrelationId != r.PublishAttemptID {
			t.Fatal("API attempt changed")
		}
		if err := d.Ack(false); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("missing_target_binding_return_ack_is_not_transfer", func(t *testing.T) {
		o := options
		o.Publisher.Slots = 1
		s := dial(t, conf.AMQPURL(), 2, o)
		if err := ch.QueueUnbind(rabbitmq.QuarantineQueue, rabbitmq.QuarantineRoutingKey, rabbitmq.QuarantineExchange, nil); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := ch.QueueBind(rabbitmq.QuarantineQueue, rabbitmq.QuarantineRoutingKey, rabbitmq.QuarantineExchange, false, nil); err != nil {
				t.Error(err)
			}
		})
		r, err := s.Publish(ctx, q)
		if !errors.Is(err, rabbitmq.ErrPublisherReturn) || r.Outcome != rabbitmq.PublishUnroutable {
			t.Fatal("Return+ACK claimed target success", r, err)
		}
		if err := ch.QueueBind(rabbitmq.QuarantineQueue, rabbitmq.QuarantineRoutingKey, rabbitmq.QuarantineExchange, false, nil); err != nil {
			t.Fatal(err)
		}
		if r, err := s.Publish(ctx, q); !errors.Is(err, rabbitmq.ErrPublisherUnavailable) || r.Outcome != rabbitmq.PublishNotStarted {
			t.Fatal("retired slot reused", r, err)
		}
		fresh := dial(t, conf.AMQPURL(), 3, o)
		r, err = fresh.Publish(ctx, q)
		if err != nil || r.Outcome != rabbitmq.PublishAccepted || r.DeliveryTag != 1 {
			t.Fatal("fresh target session failed", r, err)
		}
		read(t, map[string]rabbitmq.QuarantinePublishResult{r.PublishAttemptID: r})
	})
	t.Run("target_capacity_nack_preserves_confirmed_audit_bytes", func(t *testing.T) {
		path := "/api/operator-policies/" + url.PathEscape(conf.VHost) + "/ichat.test.quarantine.capacity"
		if _, err := env.rabbitAPI(ctx, http.MethodPut, path, map[string]any{"pattern": "^" + regexp.QuoteMeta(rabbitmq.QuarantineQueue) + "$", "apply-to": "quorum_queues", "priority": 100, "definition": map[string]any{"max-length": 1}}, nil); err != nil {
			t.Fatal(err)
		}
		removed := false
		t.Cleanup(func() {
			if !removed {
				if _, err := env.rabbitAPI(ctx, http.MethodDelete, path, nil, nil); err != nil {
					t.Error(err)
				}
			}
		})
		o := options
		o.Publisher.Slots = 1
		s := dial(t, conf.AMQPURL(), 4, o)
		expected := map[string]rabbitmq.QuarantinePublishResult{}
		nacked := false
		for i := 0; i < 8; i++ {
			r, err := s.Publish(ctx, q)
			if errors.Is(err, rabbitmq.ErrPublisherNack) {
				if r.Outcome != rabbitmq.PublishNacked {
					t.Fatal("NACK claimed success")
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
			t.Fatal("capacity rejection was not exercised")
		}
		read(t, expected)
		if _, err := env.rabbitAPI(ctx, http.MethodDelete, path, nil, nil); err != nil {
			t.Fatal(err)
		}
		removed = true
	})
	t.Run("lost_actual_confirm_unknown_then_explicit_duplicate_transfer", func(t *testing.T) {
		proxy := startSessionProxy(t, conf.Address)
		proxied := conf
		proxied.Address = proxy.address
		o := options
		o.Publisher.Slots = 1
		o.OperationTimeout = 250 * time.Millisecond
		s := dial(t, proxied.AMQPURL(), 5, o)
		proxy.mode.Store(1)
		started := time.Now()
		unknown, err := s.Publish(ctx, q)
		if err == nil || unknown.Outcome != rabbitmq.PublishUnknown || time.Since(started) > 2*time.Second {
			t.Fatal("lost confirm declared transfer", unknown, err)
		}
		select {
		case <-proxy.dropped:
		default:
			t.Fatal("actual broker ACK was not intercepted")
		}
		select {
		case <-s.Done():
		default:
			t.Fatal("unknown socket stayed admissible")
		}
		if r, err := s.Publish(ctx, q); !errors.Is(err, rabbitmq.ErrPublisherUnavailable) || r.Outcome != rabbitmq.PublishNotStarted {
			t.Fatal("unknown session reused")
		}
		fresh := dial(t, conf.AMQPURL(), 6, o)
		confirmed, err := fresh.Publish(ctx, q)
		if err != nil || confirmed.Outcome != rabbitmq.PublishAccepted || confirmed.TransportID != unknown.TransportID || confirmed.PublishAttemptID == unknown.PublishAttemptID {
			t.Fatal("explicit retry changed audit identity", confirmed, err)
		}
		read(t, map[string]rabbitmq.QuarantinePublishResult{unknown.PublishAttemptID: unknown, confirmed.PublishAttemptID: confirmed})
		if proxy.accepted.Load() != 1 {
			t.Fatal("publisher silently reconnected/retried")
		}
		if err := s.Close(ctx); err != nil {
			t.Fatal(err)
		}
		proxy.Close()
	})
	for _, queue := range []string{rabbitmq.PersistQueue, rabbitmq.QuarantineQueue} {
		if _, ok, err := ch.Get(queue, false); err != nil || ok {
			t.Fatal("fixture left an unexpected target/source message", err)
		}
	}
	if err := rabbitmq.VerifyLocal(ctx, m, conf.VHost); err != nil {
		t.Fatal(err)
	}
}
