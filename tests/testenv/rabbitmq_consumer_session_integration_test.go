//go:build integration

package testenv

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	"iChat/messaging"
	"iChat/messaging/rabbitmq"
)

func TestRabbitMQConsumerSessionIntegration(t *testing.T) {
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
	command, err := messaging.Normalize(12, messaging.SendRequest{ClientMessageID: "8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1", TargetType: messaging.Private, TargetID: "34", Media: messaging.Text, Content: "consumer-session:" + env.RunID() + ":中🙂\x00"})
	if err != nil {
		t.Fatal(err)
	}
	consumerOptions := rabbitmq.DefaultConnectionOptions()
	consumerOptions.OperationTimeout = 2 * time.Second
	consumerOptions.WriteTimeout, consumerOptions.CloseTimeout = 300*time.Millisecond, 300*time.Millisecond
	producerOptions := rabbitmq.DefaultSessionOptions()
	producerOptions.Publisher.Slots = 1
	producerOptions.OperationTimeout = 2 * time.Second
	producerOptions.WriteTimeout, producerOptions.CloseTimeout = 300*time.Millisecond, 300*time.Millisecond
	dialConsumer := func(t *testing.T, address string, o rabbitmq.ConnectionOptions) *rabbitmq.ConsumerSession {
		t.Helper()
		s, err := rabbitmq.DialConsumer(ctx, address, m, conf.VHost, o)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := s.Close(context.Background()); err != nil && !errors.Is(err, context.DeadlineExceeded) {
				t.Error(err)
			}
		})
		return s
	}
	dialProducer := func(t *testing.T, address string) *rabbitmq.PublisherSession {
		t.Helper()
		s, err := rabbitmq.DialPublisher(ctx, address, producerOptions)
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
	read := func(t *testing.T, result rabbitmq.PublishResult) {
		t.Helper()
		// Readback/ACK is performed by the fixture's existing control connection,
		// not by ConsumerSession: this phase exposes no delivery/ACK API yet.
		d := getTopologyDelivery(t, ctx, ch, rabbitmq.PersistQueue)
		e, err := messaging.DecodeEnvelope(d.Body)
		if err != nil || e.CommandID != command.CommandID || e.PublishAttemptID != result.PublishAttemptID || e.Content != command.Content || e.PayloadHash != command.PayloadHash {
			t.Fatal("accepted/unknown message was lost or changed", err)
		}
		if err := d.Ack(false); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("distinct_broker_connections_and_independent_close", func(t *testing.T) {
		connections := func(t *testing.T) map[string]bool {
			t.Helper()
			var records []struct {
				Name  string
				User  string
				VHost string
			}
			if _, err := env.rabbitAPI(ctx, http.MethodGet, "/api/connections", nil, &records); err != nil {
				t.Fatal(err)
			}
			owned := map[string]bool{}
			for _, record := range records {
				if record.User == conf.Username && record.VHost == conf.VHost {
					owned[record.Name] = true
				}
			}
			return owned
		}
		waitCount := func(t *testing.T, count int) map[string]bool {
			t.Helper()
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				owned := connections(t)
				if len(owned) == count {
					return owned
				}
				time.Sleep(20 * time.Millisecond)
			}
			t.Fatal("broker did not report expected owned connection count")
			return nil
		}
		baseline := waitCount(t, 1) // Fixture control connection.
		consumer := dialConsumer(t, conf.AMQPURL(), consumerOptions)
		withConsumer := waitCount(t, 2)
		var consumerName string
		for name := range withConsumer {
			if !baseline[name] {
				consumerName = name
			}
		}
		if consumerName == "" {
			t.Fatal("consumer did not open a new broker connection")
		}
		producer := dialProducer(t, conf.AMQPURL())
		withBoth := waitCount(t, 3)
		var producerName string
		for name := range withBoth {
			if !withConsumer[name] {
				producerName = name
			}
		}
		if producerName == "" || producerName == consumerName {
			t.Fatal("producer reused consumer connection")
		}
		if err := consumer.Verify(ctx); err != nil {
			t.Fatal(err)
		}
		if err := consumer.Close(ctx); err != nil {
			t.Fatal(err)
		}
		remaining := waitCount(t, 2)
		if remaining[consumerName] || !remaining[producerName] {
			t.Fatal("consumer close affected producer ownership")
		}
		result, err := producer.Publish(ctx, command, time.Now())
		if err != nil || result.Outcome != rabbitmq.PublishAccepted {
			t.Fatal("consumer close retired producer", result, err)
		}
		read(t, result)
	})
	t.Run("producer_confirm_blackhole_does_not_stop_consumer_rpc", func(t *testing.T) {
		consumer := dialConsumer(t, conf.AMQPURL(), consumerOptions)
		proxy := startSessionProxy(t, conf.Address)
		proxied := conf
		proxied.Address = proxy.address
		producer := dialProducer(t, proxied.AMQPURL())
		proxy.mode.Store(1)
		request, requestCancel := context.WithCancel(ctx)
		defer requestCancel()
		type publication struct {
			result rabbitmq.PublishResult
			err    error
		}
		done := make(chan publication, 1)
		go func() {
			result, err := producer.Publish(request, command, time.Now())
			done <- publication{result, err}
		}()
		select {
		case <-proxy.dropped:
		case <-time.After(time.Second):
			t.Fatal("real broker ACK was not intercepted")
		}
		verifyCtx, verifyCancel := context.WithTimeout(ctx, time.Second)
		defer verifyCancel()
		if err := consumer.Verify(verifyCtx); err != nil {
			t.Fatal("publisher confirm blackhole blocked consumer", err)
		}
		select {
		case <-done:
			t.Fatal("producer did not remain pending during consumer verification")
		default:
		}
		requestCancel()
		select {
		case result := <-done:
			if result.err == nil || result.result.Outcome != rabbitmq.PublishUnknown {
				t.Fatal("lost confirm became definite", result.result, result.err)
			}
			read(t, result.result)
		case <-time.After(time.Second):
			t.Fatal("producer abort left a pending call")
		}
		if err := consumer.Verify(ctx); err != nil {
			t.Fatal("producer abort retired independent consumer", err)
		}
		if _, ok, err := ch.Get(rabbitmq.PersistQueue, false); err != nil || ok {
			t.Fatal("consumer verification or recovery published an extra message", err)
		}
	})
	t.Run("consumer_rpc_blackhole_does_not_stop_producer", func(t *testing.T) {
		proxy := startSessionProxy(t, conf.Address)
		proxied := conf
		proxied.Address = proxy.address
		o := consumerOptions
		o.OperationTimeout = 300 * time.Millisecond
		consumer := dialConsumer(t, proxied.AMQPURL(), o)
		producer := dialProducer(t, conf.AMQPURL())
		proxy.mode.Store(1)
		done := make(chan error, 1)
		go func() { done <- consumer.Verify(ctx) }()
		deadline := time.Now().Add(time.Second)
		for proxy.framesDropped.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if proxy.framesDropped.Load() == 0 {
			t.Fatal("consumer RPC reply was not intercepted")
		}
		result, err := producer.Publish(ctx, command, time.Now())
		if err != nil || result.Outcome != rabbitmq.PublishAccepted {
			t.Fatal("consumer blackhole blocked producer", result, err)
		}
		read(t, result)
		select {
		case err := <-done:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("consumer blackhole did not fail by deadline", err)
			}
		case <-time.After(time.Second):
			t.Fatal("consumer RPC was abandoned")
		}
		select {
		case <-consumer.Done():
		default:
			t.Fatal("consumer failure did not stop its admission")
		}
		if err := consumer.Verify(ctx); !errors.Is(err, rabbitmq.ErrConsumerUnavailable) {
			t.Fatal("failed consumer was reused", err)
		}
		result, err = producer.Publish(ctx, command, time.Now())
		if err != nil || result.Outcome != rabbitmq.PublishAccepted {
			t.Fatal("consumer abort retired producer", result, err)
		}
		read(t, result)
	})
	t.Run("topology_drift_retires_consumer_without_repair", func(t *testing.T) {
		consumer := dialConsumer(t, conf.AMQPURL(), consumerOptions)
		if err := ch.QueueUnbind(rabbitmq.PersistQueue, rabbitmq.SubmitRoutingKey, rabbitmq.CommandsExchange, nil); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := ch.QueueBind(rabbitmq.PersistQueue, rabbitmq.SubmitRoutingKey, rabbitmq.CommandsExchange, false, nil); err != nil {
				t.Error(err)
			}
		})
		if err := consumer.Verify(ctx); !errors.Is(err, rabbitmq.ErrConsumerTopology) {
			t.Fatal("route drift passed consumer verification", err)
		}
		select {
		case <-consumer.Done():
		default:
			t.Fatal("topology drift did not retire consumer")
		}
		if s, err := rabbitmq.DialConsumer(ctx, conf.AMQPURL(), m, conf.VHost, consumerOptions); s != nil || !errors.Is(err, rabbitmq.ErrConsumerTopology) {
			if s != nil {
				s.Close(context.Background())
			}
			t.Fatal("new consumer accepted mismatched topology", err)
		}
		var bindings []any
		if _, err := env.rabbitAPI(ctx, http.MethodGet, "/api/exchanges/"+url.PathEscape(conf.VHost)+"/"+rabbitmq.CommandsExchange+"/bindings/source", nil, &bindings); err != nil || len(bindings) != 0 {
			t.Fatal("consumer repaired topology", err)
		}
		if err := ch.QueueBind(rabbitmq.PersistQueue, rabbitmq.SubmitRoutingKey, rabbitmq.CommandsExchange, false, nil); err != nil {
			t.Fatal(err)
		}
		if err := consumer.Verify(ctx); !errors.Is(err, rabbitmq.ErrConsumerUnavailable) {
			t.Fatal("retired consumer automatically recovered", err)
		}
		fresh := dialConsumer(t, conf.AMQPURL(), consumerOptions)
		if err := fresh.Verify(ctx); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("consumer_close_response_blackhole_is_bounded", func(t *testing.T) {
		proxy := startSessionProxy(t, conf.Address)
		proxied := conf
		proxied.Address = proxy.address
		consumer := dialConsumer(t, proxied.AMQPURL(), consumerOptions)
		producer := dialProducer(t, conf.AMQPURL())
		proxy.mode.Store(3)
		started := time.Now()
		if err := consumer.Close(ctx); err == nil {
			t.Fatal("missing consumer close response reported graceful completion")
		}
		if time.Since(started) > time.Second {
			t.Fatal("consumer close was not bounded")
		}
		select {
		case <-proxy.dropped:
		default:
			t.Fatal("real consumer close response was not intercepted")
		}
		result, err := producer.Publish(ctx, command, time.Now())
		if err != nil || result.Outcome != rabbitmq.PublishAccepted {
			t.Fatal("consumer close affected producer", result, err)
		}
		read(t, result)
	})
	if err := rabbitmq.VerifyLocal(ctx, m, conf.VHost); err != nil {
		t.Fatal(err)
	}
}
