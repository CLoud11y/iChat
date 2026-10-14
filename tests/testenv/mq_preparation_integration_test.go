//go:build integration

package testenv

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	"iChat/application"
	"iChat/config"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"

	amqp "github.com/rabbitmq/amqp091-go"
)

func TestMQPreparationIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	env, broker := startRabbitFixture(t, ctx)
	m, err := rabbitmq.NewManagement("http://"+broker.ManagementAddress, rabbitAdminUser, env.password)
	if err != nil {
		t.Fatal(err)
	}
	control := openPublisherConnection(t, ctx, broker)
	ch, err := control.Channel()
	if err != nil {
		t.Fatal(err)
	}
	if err := rabbitmq.EnsureLocal(ctx, ch, m, broker.VHost); err != nil {
		t.Fatal(err)
	}
	c := config.Config{RABBITMQ: config.RabbitMQConfig{
		URL: broker.AMQPURL(), VHost: broker.VHost,
		ManagementURL: "http://" + broker.ManagementAddress, ManagementUsername: rabbitAdminUser, ManagementPassword: env.password,
		PublisherSlots: 1, ConsumerWorkers: 2, ConsumerPrefetch: 3,
		PublishTimeout: 2 * time.Second, ConsumerOperationTimeout: 2 * time.Second,
		ConnectTimeout: 10 * time.Second, WriteTimeout: time.Second, CloseTimeout: time.Second, Heartbeat: time.Second,
		RetryMin: 50 * time.Millisecond, RetryMax: 200 * time.Millisecond,
	}}
	o := application.MQOptions{SetupTimeout: 10 * time.Second, CloseTimeout: 2 * time.Second}
	connections := func(t *testing.T) map[string]bool {
		t.Helper()
		var records []struct{ Name, User, VHost string }
		if _, err := env.rabbitAPI(ctx, http.MethodGet, "/api/connections", nil, &records); err != nil {
			t.Fatal(err)
		}
		result := map[string]bool{}
		for _, record := range records {
			if record.User == broker.Username && record.VHost == broker.VHost {
				result[record.Name] = true
			}
		}
		return result
	}
	waitCount := func(t *testing.T, count int) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if len(connections(t)) == count {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("unexpected broker connection count", count, len(connections(t)))
	}
	assertIdle := func(t *testing.T) {
		t.Helper()
		// Subscription list is current broker state, independent of sampled stats.
		var subscriptions []any
		if _, err := env.rabbitAPI(ctx, http.MethodGet, "/api/consumers/"+url.PathEscape(broker.VHost), nil, &subscriptions); err != nil || len(subscriptions) != 0 {
			t.Fatal("preparation created a subscription", err)
		}
		for _, queue := range []string{rabbitmq.PersistQueue, rabbitmq.QuarantineQueue} {
			deadline := time.Now().Add(15 * time.Second)
			for {
				var stats struct {
					Ready     *int `json:"messages_ready"`
					Unacked   *int `json:"messages_unacknowledged"`
					Consumers *int `json:"consumers"`
				}
				if _, err := env.rabbitAPI(ctx, http.MethodGet, "/api/queues/"+url.PathEscape(broker.VHost)+"/"+queue, nil, &stats); err != nil {
					t.Fatal(err)
				}
				if stats.Ready != nil && stats.Unacked != nil && stats.Consumers != nil && *stats.Ready == 1 && *stats.Unacked == 0 && *stats.Consumers == 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("preparation claimed or lost a pending message", queue)
				}
				time.Sleep(50 * time.Millisecond)
			}
		}
	}
	command, err := messaging.Normalize(12, messaging.SendRequest{ClientMessageID: "8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1", TargetType: messaging.Private, TargetID: "34", Media: messaging.Text, Content: "mq-preparation:" + env.RunID() + ":中🙂\x00"})
	if err != nil {
		t.Fatal(err)
	}
	pubOptions := rabbitmq.DefaultSessionOptions()
	pubOptions.Publisher.Slots = 1
	initial, err := rabbitmq.DialPublisher(ctx, broker.AMQPURL(), pubOptions)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = initial.Close(context.Background()) })
	accepted, err := initial.Publish(ctx, command, time.Now())
	if err != nil || accepted.Outcome != rabbitmq.PublishAccepted {
		t.Fatal("fixture command was not accepted", err)
	}
	if err := initial.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := ch.Confirm(false); err != nil {
		t.Fatal(err)
	}
	confirms := ch.NotifyPublish(make(chan amqp.Confirmation, 1))
	auditBody := "fixture audit:" + env.RunID()
	if err := ch.PublishWithContext(ctx, rabbitmq.QuarantineExchange, rabbitmq.QuarantineRoutingKey, true, false, amqp.Publishing{DeliveryMode: amqp.Persistent, Body: []byte(auditBody)}); err != nil {
		t.Fatal(err)
	}
	select {
	case confirmed, ok := <-confirms:
		if !ok || !confirmed.Ack {
			t.Fatal("fixture audit was not confirmed")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	waitCount(t, 1) // Only this fixture's control connection remains.
	assertIdle(t)

	for _, mode := range []config.RuntimeMode{config.ModeLegacy, config.ModeAPI, config.ModeWorker, config.ModeAll} {
		t.Run(string(mode), func(t *testing.T) {
			conf := c
			conf.RUNTIME.Mode = mode
			setup, cancelSetup := context.WithCancel(ctx)
			r, err := application.PrepareMQ(setup, conf, o)
			cancelSetup() // A successful handoff must survive setup cancellation.
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := r.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})
			count := 1
			if mode == config.ModeAPI || mode == config.ModeAll {
				ready, cancelReady := context.WithTimeout(ctx, 10*time.Second)
				defer cancelReady()
				if r.Publisher() == nil {
					t.Fatal("missing publisher")
				}
				if _, err := r.Publisher().WaitReady(ready); err != nil {
					t.Fatal(err)
				}
				count++
			} else if r.Publisher() != nil {
				t.Fatal("unused publisher was constructed")
			}
			wantConsumers := 0
			if mode == config.ModeWorker || mode == config.ModeAll {
				wantConsumers = 3
			}
			if len(r.Consumers()) != wantConsumers {
				t.Fatal("incorrect consumer count")
			}
			for _, consumer := range r.Consumers() {
				if err := consumer.Session.Verify(ctx); err != nil {
					t.Fatal("prepared consumer was unusable after setup cancellation", err)
				}
				count++
			}
			waitCount(t, count)
			assertIdle(t)
			if err := r.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			waitCount(t, 1)
			assertIdle(t)
		})
	}

	for _, mode := range []config.RuntimeMode{config.ModeWorker, config.ModeAll} {
		t.Run(string(mode)+"_partial_handshake_timeout", func(t *testing.T) {
			proxy := startSessionProxy(t, broker.Address)
			forwarded := uint64(1)
			if mode == config.ModeAll {
				forwarded++ // Publisher plus the first persistence connection.
			}
			proxy.stallAfter.Store(forwarded)
			proxied := broker
			proxied.Address = proxy.address
			conf := c
			conf.RUNTIME.Mode, conf.RABBITMQ.URL = mode, proxied.AMQPURL()
			// Broker connection records are sampled about every five seconds.
			// Keep the partial state alive long enough to observe it before rollback.
			options := application.MQOptions{SetupTimeout: 8 * time.Second, CloseTimeout: 2 * time.Second}
			setup, cancelSetup := context.WithCancel(ctx)
			var result *application.MQResources
			var prepareErr error
			done := make(chan struct{})
			start := time.Now()
			go func() {
				result, prepareErr = application.PrepareMQ(setup, conf, options)
				close(done)
			}()
			t.Cleanup(func() {
				cancelSetup()
				<-done
				if result != nil {
					if err := result.Close(context.Background()); err != nil {
						t.Error(err)
					}
				}
			})
			waitCount(t, 1+int(forwarded)) // Prove partial resources exist before rollback.
			select {
			case <-done:
			case <-time.After(11 * time.Second):
				t.Fatal("construction did not interrupt the actual handshake")
			}
			if result != nil || !errors.Is(prepareErr, application.ErrMQPrepare) || !errors.Is(prepareErr, context.DeadlineExceeded) || errors.Is(prepareErr, application.ErrMQClose) || time.Since(start) > 11*time.Second {
				t.Fatal("partial preparation was not bounded/fully rolled back", prepareErr)
			}
			if proxy.accepted.Load() != forwarded+1 {
				t.Fatal("failure did not occur in the later handshake", proxy.accepted.Load())
			}
			waitCount(t, 1)
			assertIdle(t)
		})
	}
	// Only the fixture control connection consumes/ACKs, after all preparation
	// tests finish. Verify complete pending contents survived every close/failure.
	d := getTopologyDelivery(t, ctx, ch, rabbitmq.PersistQueue)
	envelope, err := messaging.DecodeEnvelope(d.Body)
	if err != nil || envelope.CommandID != command.CommandID || envelope.Content != command.Content || envelope.PublishAttemptID != accepted.PublishAttemptID || envelope.PayloadHash != command.PayloadHash {
		t.Fatal("prepared connections changed/lost pending command", err)
	}
	if err := d.Ack(false); err != nil {
		t.Fatal(err)
	}
	audit := getTopologyDelivery(t, ctx, ch, rabbitmq.QuarantineQueue)
	if string(audit.Body) != auditBody || audit.DeliveryMode != amqp.Persistent {
		t.Fatal("prepared connections changed/lost audit data")
	}
	if err := audit.Ack(false); err != nil {
		t.Fatal(err)
	}
}
