//go:build integration

package testenv

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
)

func TestRabbitMQSupervisorIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
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
	c, err := messaging.Normalize(12, messaging.SendRequest{ClientMessageID: "8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1", TargetType: messaging.Private, TargetID: "34", Media: messaging.Text, Content: "supervisor:" + env.RunID() + ":中🙂\x00"})
	if err != nil {
		t.Fatal(err)
	}
	o := rabbitmq.DefaultSupervisorOptions()
	o.Session.Publisher.Slots = 1
	o.Session.ConnectTimeout = time.Second
	o.Session.OperationTimeout = time.Second
	o.Session.CloseTimeout = 500 * time.Millisecond
	o.Session.WriteTimeout = 500 * time.Millisecond
	o.RetryMin = 50 * time.Millisecond
	o.RetryMax = 200 * time.Millisecond
	start := func(t *testing.T, address string) *rabbitmq.Supervisor {
		t.Helper()
		s, err := rabbitmq.StartSupervisor(ctx, address, m, conf.VHost, o)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := s.Close(closeCtx); err != nil {
				t.Error(err)
			}
		})
		return s
	}
	ready := func(t *testing.T, s *rabbitmq.Supervisor) uint64 {
		t.Helper()
		wait, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		epoch, err := s.WaitReady(wait)
		if err != nil {
			t.Fatal("supervisor did not become ready", err, s.Status())
		}
		return epoch
	}
	read := func(t *testing.T, channel *amqp.Channel, r rabbitmq.PublishResult) {
		t.Helper()
		d := getTopologyDelivery(t, ctx, channel, rabbitmq.PersistQueue)
		e, err := messaging.DecodeEnvelope(d.Body)
		if err != nil || e.CommandID != c.CommandID || e.PublishAttemptID != r.PublishAttemptID || e.PayloadHash != c.PayloadHash || e.Content != c.Content || d.CorrelationId != r.PublishAttemptID {
			t.Fatal("recovery changed command/attempt contents", err)
		}
		if err := d.Ack(false); err != nil {
			t.Fatal(err)
		}
	}
	assertEmpty := func(t *testing.T, channel *amqp.Channel) {
		t.Helper()
		if _, ok, err := channel.Get(rabbitmq.PersistQueue, false); err != nil || ok {
			t.Fatal("recovery automatically published an extra message", err)
		}
	}
	t.Run("unknown_reconnects_with_new_epoch_without_replay", func(t *testing.T) {
		proxy := startSessionProxy(t, conf.Address)
		proxy.onlyFirst.Store(true)
		proxied := conf
		proxied.Address = proxy.address
		s := start(t, proxied.AMQPURL())
		firstEpoch := ready(t, s)
		proxy.mode.Store(1)
		r, err := s.Publish(ctx, c, time.Now())
		if err == nil || r.Outcome != rabbitmq.PublishUnknown || r.ConnectionEpoch != firstEpoch {
			t.Fatal("missing confirm became accepted/definite", r, err)
		}
		select {
		case <-proxy.dropped:
		default:
			t.Fatal("real broker ACK was not dropped")
		}
		newEpoch := ready(t, s)
		if newEpoch <= firstEpoch {
			t.Fatal("reconnect reused connection epoch")
		}
		read(t, ch, r)
		assertEmpty(t, ch)
		retry, err := s.Publish(ctx, c, time.Now())
		if err != nil || retry.Outcome != rabbitmq.PublishAccepted || retry.ConnectionEpoch != newEpoch || retry.DeliveryTag != 1 || retry.PublishAttemptID == r.PublishAttemptID || retry.CommandID != r.CommandID {
			t.Fatal("new epoch retry correlation failed", retry, err)
		}
		read(t, ch, retry)
		assertEmpty(t, ch)
	})
	t.Run("topology_drift_keeps_reconnect_unready_without_repair", func(t *testing.T) {
		s := start(t, conf.AMQPURL())
		initial := ready(t, s)
		if err := ch.QueueUnbind(rabbitmq.PersistQueue, rabbitmq.SubmitRoutingKey, rabbitmq.CommandsExchange, nil); err != nil {
			t.Fatal(err)
		}
		r, err := s.Publish(ctx, c, time.Now())
		if !errors.Is(err, rabbitmq.ErrPublisherReturn) || r.Outcome != rabbitmq.PublishUnroutable {
			t.Fatal("route drift accepted", r, err)
		}
		wait, cancel := context.WithTimeout(ctx, 350*time.Millisecond)
		defer cancel()
		if _, err := s.WaitReady(wait); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("wrong topology became ready", err)
		}
		if s.Status().Ready {
			t.Fatal("mismatched topology remained ready")
		}
		var bindings []any
		if _, err := env.rabbitAPI(ctx, http.MethodGet, "/api/exchanges/"+url.PathEscape(conf.VHost)+"/"+rabbitmq.CommandsExchange+"/bindings/source", nil, &bindings); err != nil || len(bindings) != 0 {
			t.Fatal("supervisor repaired missing binding", err)
		}
		if r, err := s.Publish(ctx, c, time.Now()); !errors.Is(err, rabbitmq.ErrPublisherUnavailable) || r.Outcome != rabbitmq.PublishNotStarted {
			t.Fatal("unready supervisor wrote", r, err)
		}
		if err := ch.QueueBind(rabbitmq.PersistQueue, rabbitmq.SubmitRoutingKey, rabbitmq.CommandsExchange, false, nil); err != nil {
			t.Fatal(err)
		}
		recovered := ready(t, s)
		if recovered <= initial {
			t.Fatal("topology recovery reused epoch")
		}
		r, err = s.Publish(ctx, c, time.Now())
		if err != nil || r.ConnectionEpoch != recovered || r.DeliveryTag != 1 {
			t.Fatal("repaired topology did not restore admission", r, err)
		}
		read(t, ch, r)
		assertEmpty(t, ch)
	})
	t.Run("broker_restart_preserves_pending_message_and_recovers", func(t *testing.T) {
		s := start(t, conf.AMQPURL())
		initial := ready(t, s)
		r, err := s.Publish(ctx, c, time.Now())
		if err != nil || r.Outcome != rabbitmq.PublishAccepted {
			t.Fatal(err)
		}
		resources, err := env.resources(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := env.verify(ctx, resources); err != nil {
			t.Fatal(err)
		}
		if _, err := env.run(ctx, env.composeArgs("stop", "rabbitmq")...); err != nil {
			t.Fatal(err)
		}
		deadline := time.NewTimer(3 * time.Second)
		defer deadline.Stop()
		for s.Status().Ready {
			select {
			case <-deadline.C:
				t.Fatal("stopped broker still ready")
			case <-time.After(10 * time.Millisecond):
			}
		}
		if r, err := s.Publish(ctx, c, time.Now()); !errors.Is(err, rabbitmq.ErrPublisherUnavailable) || r.Outcome != rabbitmq.PublishNotStarted {
			t.Fatal("offline broker accepted new work", r, err)
		}
		resources, err = env.resources(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := env.verify(ctx, resources); err != nil {
			t.Fatal(err)
		}
		if _, err := env.run(ctx, env.composeArgs("up", "--detach", "--wait", "--wait-timeout", "180", "rabbitmq")...); err != nil {
			t.Fatal(err)
		}
		actualAMQP, err := env.address(ctx, "rabbitmq", "5672")
		if err != nil {
			t.Fatal(err)
		}
		actualManagement, err := env.address(ctx, "rabbitmq", "15672")
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("fixture restart AMQP %s -> %s; management %s -> %s", conf.Address, actualAMQP, conf.ManagementAddress, actualManagement)
		if actualAMQP != conf.Address || actualManagement != conf.ManagementAddress {
			// Docker may reallocate dynamically-published host ports on start.
			// Restore this run's original endpoints with an owned-test override;
			// the supervisor itself must never switch URLs or credentials.
			_, amqpPort, err := net.SplitHostPort(conf.Address)
			if err != nil {
				t.Fatal(err)
			}
			_, managementPort, err := net.SplitHostPort(conf.ManagementAddress)
			if err != nil {
				t.Fatal(err)
			}
			override := filepath.Join(t.TempDir(), "restart-ports.yml")
			content := fmt.Sprintf("services:\n  rabbitmq:\n    ports: !override\n      - '127.0.0.1:%s:5672'\n      - '127.0.0.1:%s:15672'\n", amqpPort, managementPort)
			if err := os.WriteFile(override, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			resources, err = env.resources(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := env.verify(ctx, resources); err != nil {
				t.Fatal(err)
			}
			if _, err := env.run(ctx, env.composeArgs("stop", "rabbitmq")...); err != nil {
				t.Fatal(err)
			}
			if _, err := env.run(ctx, env.composeArgs("--file", override, "up", "--detach", "--wait", "--wait-timeout", "180", "rabbitmq")...); err != nil {
				t.Fatal(err)
			}
		}
		recovered := ready(t, s)
		if recovered <= initial {
			t.Fatal("broker restart reused epoch")
		}
		fresh := openPublisherConnection(t, ctx, conf)
		reader, err := fresh.Channel()
		if err != nil {
			t.Fatal(err)
		}
		read(t, reader, r)
		assertEmpty(t, reader)
		next, err := s.Publish(ctx, c, time.Now())
		if err != nil || next.ConnectionEpoch != recovered || next.DeliveryTag != 1 {
			t.Fatal("restarted broker session did not publish", next, err)
		}
		read(t, reader, next)
		assertEmpty(t, reader)
		if err := rabbitmq.VerifyLocal(ctx, m, conf.VHost); err != nil {
			t.Fatal(err)
		}
	})
}
