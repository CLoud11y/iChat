//go:build integration

package testenv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"iChat/messaging"
	"iChat/messaging/rabbitmq"

	amqp "github.com/rabbitmq/amqp091-go"
)

// This suite verifies the input boundary only. ACKs below explicitly clean up
// fixture deliveries; no audit SQL transaction or production worker is run.
func TestQuarantineInputIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env, conf := startRabbitFixture(t, ctx)
	m, err := rabbitmq.NewManagement("http://"+conf.ManagementAddress, rabbitAdminUser, env.password)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := openPublisherConnection(t, ctx, conf).Channel()
	if err != nil {
		t.Fatal(err)
	}
	if err := rabbitmq.EnsureLocal(ctx, ch, m, conf.VHost); err != nil {
		t.Fatal(err)
	}
	if err := ch.Confirm(false); err != nil {
		t.Fatal(err)
	}
	confirms := ch.NotifyPublish(make(chan amqp.Confirmation, 1))
	returns := ch.NotifyReturn(make(chan amqp.Return, 1))
	publish := func(t *testing.T, exchange, key string, p amqp.Publishing) {
		t.Helper()
		tag := ch.GetNextPublishSeqNo()
		if err := ch.PublishWithContext(ctx, exchange, key, true, false, p); err != nil {
			t.Fatal(err)
		}
		select {
		case c, ok := <-confirms:
			if !ok || !c.Ack || c.DeliveryTag != tag {
				t.Fatal("fixture publication was not confirmed")
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		select {
		case <-returns:
			t.Fatal("fixture publication was returned")
		default:
		}
	}
	open := func(t *testing.T, queue string) *rabbitmq.ConsumerChannel {
		t.Helper()
		s, err := rabbitmq.DialConsumer(ctx, conf.AMQPURL(), m, conf.VHost, rabbitmq.DefaultConnectionOptions())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := s.Close(context.Background()); err != nil {
				t.Error(err)
			}
		})
		c, err := s.OpenConsumer(ctx, queue, 1)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	next := func(t *testing.T, c *rabbitmq.ConsumerChannel) *rabbitmq.ConsumerDelivery {
		t.Helper()
		d, err := c.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	ack := func(t *testing.T, d *rabbitmq.ConsumerDelivery) {
		t.Helper()
		if err := d.Ack(ctx); err != nil {
			t.Fatal(err)
		}
	}
	equalDelivery := func(t *testing.T, got, want rabbitmq.ConsumerMessage) bool {
		t.Helper()
		// AMQP timestamps carry seconds, not a Go Location pointer. Compare
		// the complete typed protocol snapshots with their UTC normalization.
		stamp := time.Unix(1700000002, 0).UTC()
		a, err := rabbitmq.NewQuarantineEnvelope(got, "INVALID_DELIVERY", stamp)
		if err != nil {
			t.Fatal(err)
		}
		b, err := rabbitmq.NewQuarantineEnvelope(want, "INVALID_DELIVERY", stamp)
		if err != nil {
			t.Fatal(err)
		}
		return bytes.Equal(a.Encode(), b.Encode())
	}
	source := rabbitmq.ConsumerMessage{Body: []byte{0xff, 0, '{'}, ContentType: "application/json", DeliveryMode: amqp.Persistent,
		MessageID: "broken-" + env.RunID(), CorrelationID: "original-attempt", Exchange: rabbitmq.CommandsExchange, RoutingKey: rabbitmq.SubmitRoutingKey,
		Headers: amqp.Table{"binary": []byte{0, 255}, "large": int64(9007199254740993), "nested": amqp.Table{"values": []any{true, amqp.Decimal{Scale: 2, Value: -12}}}}}
	command, err := messaging.Normalize(12, messaging.SendRequest{ClientMessageID: "8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1", TargetType: messaging.Private, TargetID: "34", Media: messaging.Text, Content: "中🙂\x00 input"})
	if err != nil {
		t.Fatal(err)
	}
	wire, err := messaging.NewEnvelope(command, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	valid := source
	valid.Body, err = json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	valid.MessageID, valid.CorrelationID = wire.CommandID, wire.PublishAttemptID
	check := func(t *testing.T, q rabbitmq.QuarantineInput, original rabbitmq.QuarantineEnvelope, identified bool) {
		t.Helper()
		want, err := original.StableContent()
		if err != nil {
			t.Fatal(err)
		}
		got, err := q.Envelope().StableContent()
		if err != nil || !bytes.Equal(got, want) || q.Envelope().TransportID() != original.TransportID() {
			t.Fatal("stable source content/identity changed", err)
		}
		id, ok := q.Identity()
		if ok != identified || (ok && (id.SenderID != command.SenderID || id.ClientMessageID != command.ClientMessageID || id.CommandID != command.CommandID || id.PayloadHash != command.PayloadHash)) {
			t.Fatal("unexpected business identity enrichment")
		}
	}
	for _, tc := range []struct {
		name       string
		source     rabbitmq.ConsumerMessage
		identified bool
	}{{"wrapper_invalid_body_redelivery", source, false}, {"wrapper_valid_source", valid, true}} {
		t.Run(tc.name, func(t *testing.T) {
			original, err := rabbitmq.NewQuarantineEnvelope(tc.source, "INVALID_DELIVERY", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			p, err := rabbitmq.DialQuarantinePublisher(ctx, conf.AMQPURL(), rabbitmq.DefaultSessionOptions())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := p.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})
			if _, err := p.Publish(ctx, original); err != nil {
				t.Fatal(err)
			}
			c := open(t, rabbitmq.QuarantineQueue)
			d := next(t, c)
			q, err := rabbitmq.ParseQuarantineInput(d.Message, time.Now())
			if err != nil || q.Format() != rabbitmq.QuarantineWrapped {
				t.Fatal("real wrapper rejected", err)
			}
			check(t, q, original, tc.identified)
			copy, err := q.Delivery()
			if err != nil || !equalDelivery(t, copy, d.Message) {
				t.Fatal("wrapper delivery changed", err)
			}
			if _, err := c.Next(ctx); !errors.Is(err, rabbitmq.ErrConsumerBusy) {
				t.Fatal("parsing settled delivery", err)
			}
			if err := c.Close(ctx); err != nil {
				t.Fatal(err)
			}
			c = open(t, rabbitmq.QuarantineQueue)
			d = next(t, c)
			if !d.Message.Redelivered || !bytes.Equal(d.Message.Body, original.Encode()) {
				t.Fatal("unacknowledged wrapper lost")
			}
			q, err = rabbitmq.ParseQuarantineInput(d.Message, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			check(t, q, original, tc.identified)
			ack(t, d)
		})
	}
	for _, tc := range []struct {
		name, reason string
		source       rabbitmq.ConsumerMessage
		identified   bool
	}{{"dlx_rejected_bad_body", "REJECTED", source, false}, {"dlx_expired_bad_body", "EXPIRED", source, false}, {"dlx_rejected_valid_source", "REJECTED", valid, true}} {
		t.Run(tc.name, func(t *testing.T) {
			src := tc.source
			if tc.reason == "EXPIRED" {
				src.Expiration = "50"
			}
			original, err := rabbitmq.NewQuarantineEnvelope(src, "INVALID_DELIVERY", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			publish(t, rabbitmq.CommandsExchange, rabbitmq.SubmitRoutingKey, amqp.Publishing{Body: src.Body, Headers: src.Headers, ContentType: src.ContentType, DeliveryMode: src.DeliveryMode, MessageId: src.MessageID, CorrelationId: src.CorrelationID, Expiration: src.Expiration})
			if tc.reason == "REJECTED" {
				c := open(t, rabbitmq.PersistQueue)
				if err := next(t, c).Reject(ctx, false); err != nil {
					t.Fatal(err)
				}
				if err := c.Close(ctx); err != nil {
					t.Fatal(err)
				}
			}
			c := open(t, rabbitmq.QuarantineQueue)
			d := next(t, c)
			q, err := rabbitmq.ParseQuarantineInput(d.Message, time.Now())
			if err != nil || q.Format() != rabbitmq.QuarantineDeadLetter || q.Envelope().ErrorCode() != "BROKER_"+tc.reason {
				t.Fatal("real DLX rejected or misclassified", err)
			}
			check(t, q, original, tc.identified)
			copy, err := q.Delivery()
			if err != nil || !equalDelivery(t, copy, d.Message) || !bytes.Equal(copy.Body, src.Body) || copy.Headers["x-death"] == nil || copy.Expiration != "" {
				t.Fatal("DLX bytes/metadata changed", err)
			}
			ack(t, d)
		})
	}
	t.Run("tampered_wrapper_retained_without_fallback", func(t *testing.T) {
		q, err := rabbitmq.NewQuarantineEnvelope(source, "INVALID_DELIVERY", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		publish(t, rabbitmq.QuarantineExchange, rabbitmq.QuarantineRoutingKey, amqp.Publishing{Body: q.Encode(), ContentType: rabbitmq.QuarantineContentType, Type: rabbitmq.QuarantineMessageType, DeliveryMode: amqp.Persistent, MessageId: "tampered", CorrelationId: "0123456789abcdef0123456789abcdef"})
		c := open(t, rabbitmq.QuarantineQueue)
		for attempt := 0; attempt < 2; attempt++ {
			d := next(t, c)
			if _, err := rabbitmq.ParseQuarantineInput(d.Message, time.Now()); !errors.Is(err, rabbitmq.ErrQuarantineInput) {
				t.Fatal("tampered wrapper accepted", err)
			}
			if !bytes.Equal(d.Message.Body, q.Encode()) || d.Message.MessageID != "tampered" || (attempt == 1 && !d.Message.Redelivered) {
				t.Fatal("invalid input lost or changed")
			}
			if _, err := c.Next(ctx); !errors.Is(err, rabbitmq.ErrConsumerBusy) {
				t.Fatal("parse failure settled delivery", err)
			}
			if attempt == 0 {
				if err := c.Close(ctx); err != nil {
					t.Fatal(err)
				}
				c = open(t, rabbitmq.QuarantineQueue)
			} else {
				ack(t, d)
			}
		}
	})
	for _, queue := range []string{rabbitmq.PersistQueue, rabbitmq.QuarantineQueue} {
		if _, ok, err := ch.Get(queue, false); err != nil || ok {
			t.Fatal("fixture left queued input", err)
		}
	}
	if err := rabbitmq.VerifyLocal(ctx, m, conf.VHost); err != nil {
		t.Fatal(err)
	}
}
