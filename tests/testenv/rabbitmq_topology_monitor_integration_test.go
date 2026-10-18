//go:build integration

package testenv

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"iChat/messaging"
	"iChat/messaging/rabbitmq"
)

func TestRabbitMQTopologyMonitorIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env, conf := startRabbitFixture(t, ctx)
	m, err := rabbitmq.NewManagement("http://"+conf.ManagementAddress, rabbitAdminUser, env.password)
	if err != nil {
		t.Fatal(err)
	}
	conn := openPublisherConnection(t, ctx, conf)
	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	if err := rabbitmq.EnsureLocal(ctx, ch, m, conf.VHost); err != nil {
		t.Fatal(err)
	}
	o := rabbitmq.DefaultSupervisorOptions()
	o.TopologyCheckInterval = 100 * time.Millisecond
	o.Session.OperationTimeout = 2 * time.Second
	o.Session.ConnectTimeout = 2 * time.Second
	o.Session.CloseTimeout = 500 * time.Millisecond
	o.RetryMin, o.RetryMax = 100*time.Millisecond, 300*time.Millisecond
	start := func(t *testing.T, management *rabbitmq.Management) *rabbitmq.Supervisor {
		t.Helper()
		s, err := rabbitmq.StartSupervisor(ctx, conf.AMQPURL(), management, conf.VHost, o)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
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
			t.Fatal("monitor did not restore readiness", err, s.Status())
		}
		return epoch
	}
	waitFor := func(t *testing.T, condition func() bool) {
		t.Helper()
		wait, cancel := context.WithTimeout(ctx, 6*time.Second)
		defer cancel()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for !condition() {
			select {
			case <-wait.Done():
				t.Fatal("continuous topology condition did not become true")
			case <-ticker.C:
			}
		}
	}
	command, err := messaging.Normalize(12, messaging.SendRequest{ClientMessageID: "8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1", TargetType: messaging.Private, TargetID: "34", Media: messaging.Text, Content: "topology-monitor:" + env.RunID()})
	if err != nil {
		t.Fatal(err)
	}
	assertUnavailable := func(t *testing.T, s *rabbitmq.Supervisor) {
		t.Helper()
		if r, err := s.Publish(ctx, command, time.Now()); !errors.Is(err, rabbitmq.ErrPublisherUnavailable) || r.Outcome != rabbitmq.PublishNotStarted {
			t.Fatal("unverified topology admitted a command", r, err)
		}
		wait, cancel := context.WithTimeout(ctx, 350*time.Millisecond)
		defer cancel()
		if _, err := s.WaitReady(wait); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("unrepaired topology became ready", err)
		}
	}
	t.Run("idle_quarantine_binding_drift_preserves_accepted_without_replay", func(t *testing.T) {
		s := start(t, m)
		initial := ready(t, s)
		accepted, err := s.Publish(ctx, command, time.Now())
		if err != nil || accepted.Outcome != rabbitmq.PublishAccepted {
			t.Fatal("initial command not accepted", accepted, err)
		}
		if err := ch.QueueUnbind(rabbitmq.QuarantineQueue, rabbitmq.QuarantineRoutingKey, rabbitmq.QuarantineExchange, nil); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := ch.QueueBind(rabbitmq.QuarantineQueue, rabbitmq.QuarantineRoutingKey, rabbitmq.QuarantineExchange, false, nil); err != nil {
				t.Error(err)
			}
		})
		// No Publish occurs between removing the unused route and this check.
		waitFor(t, func() bool { return !s.Status().Ready })
		assertUnavailable(t, s)
		var bindings []any
		if _, err := env.rabbitAPI(ctx, http.MethodGet, "/api/exchanges/"+url.PathEscape(conf.VHost)+"/"+rabbitmq.QuarantineExchange+"/bindings/source", nil, &bindings); err != nil || len(bindings) != 0 {
			t.Fatal("monitor repaired a missing binding", err)
		}
		if err := ch.QueueBind(rabbitmq.QuarantineQueue, rabbitmq.QuarantineRoutingKey, rabbitmq.QuarantineExchange, false, nil); err != nil {
			t.Fatal(err)
		}
		if epoch := ready(t, s); epoch <= initial {
			t.Fatal("repaired route reused the retired epoch", epoch)
		}
		delivery := getTopologyDelivery(t, ctx, ch, rabbitmq.PersistQueue)
		envelope, err := messaging.DecodeEnvelope(delivery.Body)
		if err != nil || envelope.PublishAttemptID != accepted.PublishAttemptID || envelope.CommandID != command.CommandID {
			t.Fatal("accepted responsibility changed during retirement", err)
		}
		if err := delivery.Ack(false); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := ch.Get(rabbitmq.PersistQueue, false); err != nil || ok {
			t.Fatal("monitor automatically replayed a command", err)
		}
	})
	t.Run("management_response_blackhole_revokes_healthy_amqp_and_recovers", func(t *testing.T) {
		target, err := url.Parse("http://" + conf.ManagementAddress)
		if err != nil {
			t.Fatal(err)
		}
		proxy := httputil.NewSingleHostReverseProxy(target)
		proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) { w.WriteHeader(http.StatusBadGateway) }
		var blackhole atomic.Bool
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			if blackhole.Load() {
				<-r.Context().Done()
				return
			}
			proxy.ServeHTTP(w, r)
		}))
		t.Cleanup(server.Close)
		monitored, err := rabbitmq.NewManagement(server.URL, rabbitAdminUser, env.password)
		if err != nil {
			t.Fatal(err)
		}
		s := start(t, monitored)
		initial := ready(t, s)
		baseline := requests.Load()
		waitFor(t, func() bool { return requests.Load() > baseline })
		if status := s.Status(); !status.Ready || status.ConnectionEpoch != initial {
			t.Fatal("healthy periodic verification retired the connection", status)
		}
		blackhole.Store(true)
		waitFor(t, func() bool { return !s.Status().Ready })
		if _, err := ch.QueueInspect(rabbitmq.PersistQueue); err != nil {
			t.Fatal("management-only failure also lost AMQP fixture", err)
		}
		assertUnavailable(t, s)
		blackhole.Store(false)
		if epoch := ready(t, s); epoch <= initial {
			t.Fatal("management recovery reused retired epoch", epoch)
		}
		if _, ok, err := ch.Get(rabbitmq.PersistQueue, false); err != nil || ok {
			t.Fatal("management checks produced a command", err)
		}
	})
}
