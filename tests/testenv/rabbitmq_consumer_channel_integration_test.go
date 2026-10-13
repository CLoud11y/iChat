//go:build integration

package testenv

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	"iChat/messaging"
	"iChat/messaging/rabbitmq"
)

// Only the ACK-loss scenario enables this parser, before dialing. Existing
// proxy scenarios retain their original io.Copy upstream path. AMQP is plain
// loopback within this fixture; frames/payloads are never logged or persisted.
func (p *sessionProxy) upstreamWithoutConsumerAcks(client, broker net.Conn) {
	write := func(data []byte) bool {
		for len(data) > 0 {
			n, err := broker.Write(data)
			if err != nil || n == 0 {
				return false
			}
			data = data[n:]
		}
		return true
	}
	protocol := make([]byte, 8)
	if _, err := io.ReadFull(client, protocol); err != nil || !write(protocol) {
		return
	}
	for {
		header := make([]byte, 7)
		if _, err := io.ReadFull(client, header); err != nil {
			return
		}
		size := binary.BigEndian.Uint32(header[3:])
		if size > 1<<20 {
			return
		}
		payload := make([]byte, int(size)+1)
		if _, err := io.ReadFull(client, payload); err != nil {
			return
		}
		if header[0] == 1 && size >= 4 && binary.BigEndian.Uint16(payload) == 60 && binary.BigEndian.Uint16(payload[2:]) == 80 {
			p.consumerAcksDropped.Add(1)
			continue
		}
		if !write(append(header, payload...)) {
			return
		}
	}
}

func TestRabbitMQConsumerChannelIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	env, conf := startRabbitFixture(t, ctx)
	m, err := rabbitmq.NewManagement("http://"+conf.ManagementAddress, rabbitAdminUser, env.password)
	if err != nil {
		t.Fatal(err)
	}
	control := openPublisherConnection(t, ctx, conf)
	ch, err := control.Channel()
	if err != nil {
		t.Fatal(err)
	}
	if err := rabbitmq.EnsureLocal(ctx, ch, m, conf.VHost); err != nil {
		t.Fatal(err)
	}
	command, err := messaging.Normalize(12, messaging.SendRequest{ClientMessageID: "8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1", TargetType: messaging.Private, TargetID: "34", Media: messaging.Text, Content: "consumer-channel:" + env.RunID() + ":中🙂\x00"})
	if err != nil {
		t.Fatal(err)
	}
	options := rabbitmq.DefaultConnectionOptions()
	options.OperationTimeout = 2 * time.Second
	options.WriteTimeout, options.CloseTimeout = 300*time.Millisecond, 500*time.Millisecond
	publisherOptions := rabbitmq.DefaultSessionOptions()
	publisherOptions.Publisher.Slots = 1
	publisherOptions.WriteTimeout, publisherOptions.CloseTimeout = 300*time.Millisecond, 500*time.Millisecond
	producer, err := rabbitmq.DialPublisher(ctx, conf.AMQPURL(), publisherOptions)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { producer.Close(context.Background()) })
	open := func(t *testing.T, address, queue string, prefetch int) *rabbitmq.ConsumerChannel {
		t.Helper()
		session, err := rabbitmq.DialConsumer(ctx, address, m, conf.VHost, options)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { session.Close(context.Background()) })
		stream, err := session.OpenConsumer(ctx, queue, prefetch)
		if err != nil {
			t.Fatal(err)
		}
		return stream
	}
	publish := func(t *testing.T) rabbitmq.PublishResult {
		t.Helper()
		result, err := producer.Publish(ctx, command, time.Now())
		if err != nil || result.Outcome != rabbitmq.PublishAccepted {
			t.Fatal("fixture publish failed", result, err)
		}
		return result
	}
	next := func(t *testing.T, stream *rabbitmq.ConsumerChannel) *rabbitmq.ConsumerDelivery {
		t.Helper()
		delivery, err := stream.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return delivery
	}
	match := func(t *testing.T, delivery *rabbitmq.ConsumerDelivery, expected rabbitmq.PublishResult) {
		t.Helper()
		message := delivery.Message
		envelope, err := messaging.DecodeEnvelope(message.Body)
		if err != nil || envelope.CommandID != command.CommandID || envelope.PublishAttemptID != expected.PublishAttemptID ||
			envelope.Content != command.Content || envelope.PayloadHash != command.PayloadHash || message.MessageID != expected.CommandID ||
			message.CorrelationID != expected.PublishAttemptID || message.ContentType != "application/json" || message.DeliveryMode != 2 {
			t.Fatal("consumer delivery lost/changed body, properties or attempt", err)
		}
	}
	awaitQueue := func(t *testing.T, queue string, ready, unacked int) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			var actual struct {
				Ready   *int `json:"messages_ready"`
				Unacked *int `json:"messages_unacknowledged"`
				Total   *int `json:"messages"`
			}
			if _, err := env.rabbitAPI(ctx, http.MethodGet, "/api/queues/"+url.PathEscape(conf.VHost)+"/"+queue, nil, &actual); err != nil {
				t.Fatal(err)
			}
			if actual.Ready != nil && actual.Unacked != nil && actual.Total != nil && *actual.Ready == ready && *actual.Unacked == unacked && *actual.Total == ready+unacked {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatal("broker did not reach expected ready/unacknowledged message counts")
	}
	t.Run("prefetch_and_sequential_manual_ack", func(t *testing.T) {
		expected := map[string]rabbitmq.PublishResult{}
		for i := 0; i < 3; i++ {
			result := publish(t)
			expected[result.PublishAttemptID] = result
		}
		stream := open(t, conf.AMQPURL(), rabbitmq.PersistQueue, 2)
		first := next(t, stream)
		awaitQueue(t, rabbitmq.PersistQueue, 1, 2)
		if _, err := stream.Next(ctx); !errors.Is(err, rabbitmq.ErrConsumerBusy) {
			t.Fatal("worker advanced before manual ACK", err)
		}
		copy := *first
		for i := 0; i < 3; i++ {
			delivery := first
			if i != 0 {
				delivery = next(t, stream)
			}
			result, ok := expected[delivery.Message.CorrelationID]
			if !ok {
				t.Fatal("unexpected or repeated attempt")
			}
			match(t, delivery, result)
			delete(expected, result.PublishAttemptID)
			if err := delivery.Ack(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if err := copy.Ack(ctx); !errors.Is(err, rabbitmq.ErrConsumerDeliverySettled) {
			t.Fatal("copied handle duplicated ACK", err)
		}
		if err := stream.Close(ctx); err != nil {
			t.Fatal(err)
		}
		awaitQueue(t, rabbitmq.PersistQueue, 0, 0)
	})
	t.Run("close_requeues_active_and_buffered_deliveries_and_fences_old_handle", func(t *testing.T) {
		expected := map[string]rabbitmq.PublishResult{}
		for i := 0; i < 3; i++ {
			result := publish(t)
			expected[result.PublishAttemptID] = result
		}
		old := open(t, conf.AMQPURL(), rabbitmq.PersistQueue, 2)
		held := next(t, old)
		awaitQueue(t, rabbitmq.PersistQueue, 1, 2)
		if err := old.Close(ctx); err != nil {
			t.Fatal(err)
		}
		fresh := open(t, conf.AMQPURL(), rabbitmq.PersistQueue, 1)
		recoveredHeld := false
		for i := 0; i < 3; i++ {
			delivery := next(t, fresh)
			if i == 0 && delivery.Message.DeliveryTag != 1 {
				t.Fatal("fresh consumer channel did not restart tag sequence")
			}
			if err := held.Ack(ctx); !errors.Is(err, rabbitmq.ErrConsumerUnavailable) {
				t.Fatal("old handle acknowledged new channel", err)
			}
			result, ok := expected[delivery.Message.CorrelationID]
			if !ok {
				t.Fatal("redelivery was lost or duplicated")
			}
			match(t, delivery, result)
			if delivery.Message.CorrelationID == held.Message.CorrelationID {
				recoveredHeld = delivery.Message.Redelivered
			}
			delete(expected, result.PublishAttemptID)
			if err := delivery.Ack(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if !recoveredHeld || len(expected) != 0 {
			t.Fatal("held delivery was not preserved and marked redelivered")
		}
		if err := fresh.Close(ctx); err != nil {
			t.Fatal(err)
		}
		awaitQueue(t, rabbitmq.PersistQueue, 0, 0)
	})
	t.Run("single_reject_requeue_then_reliable_quarantine", func(t *testing.T) {
		expected := publish(t)
		stream := open(t, conf.AMQPURL(), rabbitmq.PersistQueue, 1)
		first := next(t, stream)
		match(t, first, expected)
		if err := first.Reject(ctx, true); err != nil {
			t.Fatal(err)
		}
		retry := next(t, stream)
		match(t, retry, expected)
		if !retry.Message.Redelivered {
			t.Fatal("reject/requeue was not redelivered")
		}
		if err := retry.Reject(ctx, false); err != nil {
			t.Fatal(err)
		}
		audit := open(t, conf.AMQPURL(), rabbitmq.QuarantineQueue, 1)
		quarantined := next(t, audit)
		match(t, quarantined, expected)
		if quarantined.Message.Headers["x-death"] == nil {
			t.Fatal("reliable DLX audit headers missing")
		}
		if err := quarantined.Reject(ctx, false); !errors.Is(err, rabbitmq.ErrConsumerConfig) {
			t.Fatal("quarantine responsibility could be discarded", err)
		}
		// Explicit fixture disposition only: production must persist audit first.
		if err := quarantined.Ack(ctx); err != nil {
			t.Fatal(err)
		}
		if err := stream.Close(ctx); err != nil {
			t.Fatal(err)
		}
		if err := audit.Close(ctx); err != nil {
			t.Fatal(err)
		}
		awaitQueue(t, rabbitmq.PersistQueue, 0, 0)
		awaitQueue(t, rabbitmq.QuarantineQueue, 0, 0)
	})
	t.Run("publisher_confirm_blackhole_does_not_block_consumer_ack", func(t *testing.T) {
		stream := open(t, conf.AMQPURL(), rabbitmq.PersistQueue, 1)
		proxy := startSessionProxy(t, conf.Address)
		proxied := conf
		proxied.Address = proxy.address
		p, err := rabbitmq.DialPublisher(ctx, proxied.AMQPURL(), publisherOptions)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { p.Close(context.Background()) })
		proxy.mode.Store(1)
		request, requestCancel := context.WithCancel(ctx)
		defer requestCancel()
		type publication struct {
			result rabbitmq.PublishResult
			err    error
		}
		done := make(chan publication, 1)
		go func() { result, err := p.Publish(request, command, time.Now()); done <- publication{result, err} }()
		select {
		case <-proxy.dropped:
		case <-time.After(time.Second):
			t.Fatal("actual publisher ACK was not intercepted")
		}
		delivery := next(t, stream)
		if err := delivery.Ack(ctx); err != nil {
			t.Fatal("independent consumer ACK blocked", err)
		}
		if err := stream.Close(ctx); err != nil {
			t.Fatal(err)
		}
		// Consumer ACK preceded graceful connection.close on the same socket.
		// An independent read after close would recover any unacked message.
		if _, ok, err := ch.Get(rabbitmq.PersistQueue, false); err != nil || ok {
			t.Fatal("consumer ACK did not clear responsibility", err)
		}
		select {
		case <-done:
			t.Fatal("producer did not stay pending while consumer ACK completed")
		default:
		}
		requestCancel()
		select {
		case result := <-done:
			if result.err == nil || result.result.Outcome != rabbitmq.PublishUnknown {
				t.Fatal("lost publisher confirm was definite")
			}
			match(t, delivery, result.result)
		case <-time.After(time.Second):
			t.Fatal("publisher cancellation did not join")
		}
	})
	t.Run("lost_ack_after_successful_local_write_is_redelivered", func(t *testing.T) {
		expected := publish(t)
		proxy := startSessionProxy(t, conf.Address)
		proxy.dropConsumerAcks.Store(true)
		proxied := conf
		proxied.Address = proxy.address
		old := open(t, proxied.AMQPURL(), rabbitmq.PersistQueue, 1)
		held := next(t, old)
		match(t, held, expected)
		if err := held.Ack(ctx); err != nil {
			t.Fatal("local write should complete before proxy drops it", err)
		}
		deadline := time.Now().Add(time.Second)
		for proxy.consumerAcksDropped.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if proxy.consumerAcksDropped.Load() != 1 {
			t.Fatal("actual consumer basic.ack was not dropped exactly once")
		}
		if err := old.Close(ctx); err != nil {
			t.Fatal(err)
		}
		fresh := open(t, conf.AMQPURL(), rabbitmq.PersistQueue, 1)
		retry := next(t, fresh)
		match(t, retry, expected)
		if !retry.Message.Redelivered {
			t.Fatal("lost ACK was not redelivered")
		}
		if err := held.Ack(ctx); !errors.Is(err, rabbitmq.ErrConsumerUnavailable) {
			t.Fatal("old ACK handle was reused", err)
		}
		if err := retry.Ack(ctx); err != nil {
			t.Fatal(err)
		}
		if err := fresh.Close(ctx); err != nil {
			t.Fatal(err)
		}
		awaitQueue(t, rabbitmq.PersistQueue, 0, 0)
	})
	if err := rabbitmq.VerifyLocal(ctx, m, conf.VHost); err != nil {
		t.Fatal(err)
	}
}
