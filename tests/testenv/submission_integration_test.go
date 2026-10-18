//go:build integration

package testenv

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
	"iChat/router"
	"iChat/utils"
)

type submissionHTTPResult struct {
	status int
	header http.Header
	body   messaging.SubmissionResponse
	err    error
}

func TestCommandSubmissionIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env, broker := startRabbitFixture(t, ctx)
	management, err := rabbitmq.NewManagement("http://"+broker.ManagementAddress, rabbitAdminUser, env.password)
	if err != nil {
		t.Fatal(err)
	}
	control := openPublisherConnection(t, ctx, broker)
	ch, err := control.Channel()
	if err != nil {
		t.Fatal(err)
	}
	if err := rabbitmq.EnsureLocal(ctx, ch, management, broker.VHost); err != nil {
		t.Fatal(err)
	}
	conf, err := env.Config()
	if err != nil {
		t.Fatal(err)
	}
	conf.JWT.Key, conf.JWT.TokenHourLifeSpan = "submission-fixture-"+env.RunID(), 1
	o := rabbitmq.DefaultSupervisorOptions()
	o.Session.Publisher.Slots = 1
	o.Session.ConnectTimeout = 3 * time.Second
	o.Session.OperationTimeout = 2 * time.Second
	o.Session.WriteTimeout = time.Second
	o.Session.CloseTimeout = 500 * time.Millisecond
	o.RetryMin, o.RetryMax = 50*time.Millisecond, 200*time.Millisecond
	// This suite exercises HTTP mapping of actual Return+ACK. Periodic drift
	// detection is covered separately; keep that timer from winning this race.
	o.TopologyCheckInterval = time.Minute
	startPublisher := func(t *testing.T, address string) *rabbitmq.Supervisor {
		t.Helper()
		s, err := rabbitmq.StartSupervisor(ctx, address, management, broker.VHost, o)
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
		wait, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		epoch, err := s.WaitReady(wait)
		if err != nil {
			t.Fatal(err)
		}
		return epoch
	}
	startHTTP := func(t *testing.T, s *rabbitmq.Supervisor) (*httptest.Server, *utils.TokenService, string, *redis.Client) {
		t.Helper()
		rds, err := utils.OpenRedis(ctx, conf)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { rds.Close() })
		tokens, err := utils.NewTokenService(conf, utils.NewRedisTokenBlacklist(rds))
		if err != nil {
			t.Fatal(err)
		}
		token, err := tokens.GenerateToken(42)
		if err != nil {
			t.Fatal(err)
		}
		r := gin.New()
		if err := router.RegisterSubmission(r, tokens, s, 3*time.Second); err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(r)
		t.Cleanup(server.Close)
		return server, tokens, token, rds
	}
	body := `{"clientMessageId":"8FD7C2EF-AE26-4BB5-91A5-32A302BB05F1","targetType":"private","targetId":"00034","media":"text","content":"submit:` + env.RunID() + `:中🙂\u0000\r\n","clientSentAt":"2026-10-18T09:00:00+11:00"}`
	request, err := messaging.DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	command, err := messaging.Normalize(42, request)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	send := func(base, token, body string) submissionHTTPResult {
		r := submissionHTTPResult{}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/auth/sendMessage", strings.NewReader(body))
		if err != nil {
			r.err = err
			return r
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			r.err = err
			return r
		}
		defer response.Body.Close()
		r.status, r.header = response.StatusCode, response.Header
		wire, err := io.ReadAll(io.LimitReader(response.Body, messaging.MaxBodyBytes+1))
		if err != nil {
			r.err = err
			return r
		}
		r.err = json.Unmarshal(wire, &r.body)
		return r
	}
	assertEmpty := func(t *testing.T) {
		t.Helper()
		if _, ok, err := ch.Get(rabbitmq.PersistQueue, false); err != nil || ok {
			t.Fatal("unexpected queued command", err)
		}
	}
	readAccepted := func(t *testing.T, submission messaging.Submission) {
		t.Helper()
		delivery := getTopologyDelivery(t, ctx, ch, rabbitmq.PersistQueue)
		envelope, err := messaging.DecodeEnvelope(delivery.Body)
		if err != nil || envelope.CommandID != command.CommandID || envelope.SenderID != "42" || envelope.TargetID != "34" || envelope.Content != command.Content || envelope.ClientSentAt != command.ClientSentAt || envelope.PayloadHash != command.PayloadHash || envelope.PublishAttemptID != submission.PublishAttemptID || delivery.CorrelationId != envelope.PublishAttemptID {
			t.Fatal("HTTP identity/content did not match durable envelope", err)
		}
		if err := delivery.Ack(false); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("confirmed_202_preserves_authenticated_normalized_envelope", func(t *testing.T) {
		s := startPublisher(t, broker.AMQPURL())
		ready(t, s)
		server, _, token, _ := startHTTP(t, s)
		result := send(server.URL, token, body)
		if result.err != nil || result.status != 202 || result.body.Code != 0 || result.body.Data.State != "accepted" || !result.body.Data.Matches(command) || result.header.Get("Cache-Control") != "no-store" {
			t.Fatal("confirmed HTTP send failed", result, result.err)
		}
		readAccepted(t, result.body.Data)
		assertEmpty(t)
	})
	t.Run("auth_and_input_failures_do_not_publish", func(t *testing.T) {
		s := startPublisher(t, broker.AMQPURL())
		ready(t, s)
		server, tokens, token, rds := startHTTP(t, s)
		for _, invalid := range []string{strings.Replace(body, `{`, `{"senderId":"99",`, 1), body + body, strings.Repeat(" ", messaging.MaxBodyBytes+1) + body} {
			if r := send(server.URL, token, invalid); r.err != nil || r.status != 400 || r.body.Data.ReasonCode != "INVALID_REQUEST" {
				t.Fatal("invalid request reached publisher", r)
			}
		}
		if r := send(server.URL, "broken-token", body); r.err != nil || r.status != 401 {
			t.Fatal("bad token accepted", r)
		}
		banned, err := tokens.GenerateToken(77)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tokens.RevokeToken(ctx, banned); err != nil {
			t.Fatal(err)
		}
		if r := send(server.URL, banned, body); r.err != nil || r.status != 401 {
			t.Fatal("blacklisted JWT accepted", r)
		}
		if err := rds.Close(); err != nil {
			t.Fatal(err)
		}
		if r := send(server.URL, token, body); r.err != nil || r.status != 503 || r.body.Data.ReasonCode != "DEPENDENCY_UNAVAILABLE" {
			t.Fatal("Redis failure allowed publication", r)
		}
		assertEmpty(t)
	})
	t.Run("lost_confirm_overload_and_same_key_explicit_retry", func(t *testing.T) {
		proxy := startSessionProxy(t, broker.Address)
		proxy.onlyFirst.Store(true)
		proxied := broker
		proxied.Address = proxy.address
		s := startPublisher(t, proxied.AMQPURL())
		initial := ready(t, s)
		server, _, token, _ := startHTTP(t, s)
		proxy.mode.Store(1)
		finished := make(chan submissionHTTPResult, 1)
		go func() { finished <- send(server.URL, token, body) }()
		select {
		case <-proxy.dropped:
		case <-time.After(time.Second):
			t.Fatal("real broker confirmation was not dropped")
		}
		busy := send(server.URL, token, body)
		if busy.err != nil || busy.status != 429 || busy.body.Data.State != "not_published" || busy.header.Get("Retry-After") != "1" {
			t.Fatal("outstanding slot did not reject with bounded overload", busy)
		}
		var unknown submissionHTTPResult
		select {
		case unknown = <-finished:
		case <-time.After(4 * time.Second):
			t.Fatal("lost confirm did not finish within deadline")
		}
		if unknown.err != nil || unknown.status != 503 || unknown.body.Data.State != "publish_unknown" || unknown.body.Data.ReasonCode != "PUBLISH_UNKNOWN" || !unknown.body.Data.Matches(command) {
			t.Fatal("lost confirmation became accepted or definite failure", unknown)
		}
		readAccepted(t, unknown.body.Data) // Broker received it, despite HTTP unknown.
		if epoch := ready(t, s); epoch <= initial {
			t.Fatal("unknown did not retire the epoch")
		}
		assertEmpty(t) // Recovery itself never retries the command.
		retry := send(server.URL, token, body)
		if retry.err != nil || retry.status != 202 || !retry.body.Data.Matches(command) || retry.body.Data.PublishAttemptID == unknown.body.Data.PublishAttemptID {
			t.Fatal("retry changed logical identity or reused physical attempt", retry)
		}
		readAccepted(t, retry.body.Data)
		assertEmpty(t)
	})
	t.Run("real_return_plus_ack_is_503_unroutable", func(t *testing.T) {
		s := startPublisher(t, broker.AMQPURL())
		ready(t, s)
		server, _, token, _ := startHTTP(t, s)
		if err := ch.QueueUnbind(rabbitmq.PersistQueue, rabbitmq.SubmitRoutingKey, rabbitmq.CommandsExchange, nil); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := ch.QueueBind(rabbitmq.PersistQueue, rabbitmq.SubmitRoutingKey, rabbitmq.CommandsExchange, false, nil); err != nil {
				t.Error(err)
			}
		})
		result := send(server.URL, token, body)
		if result.err != nil || result.status != 503 || result.body.Data.State != "unroutable" || result.body.Data.ReasonCode != "PUBLISH_UNROUTABLE" {
			t.Fatal("return plus ACK was mistaken for accepted", result)
		}
		assertEmpty(t)
	})
	t.Run("mysql_offline_does_not_block_confirmed_http_acceptance", func(t *testing.T) {
		if err := env.SetMySQLRunning(ctx, false); err != nil {
			t.Fatal(err)
		}
		s := startPublisher(t, broker.AMQPURL())
		ready(t, s)
		server, _, token, _ := startHTTP(t, s)
		result := send(server.URL, token, body)
		if result.err != nil || result.status != 202 || !result.body.Data.Matches(command) {
			t.Fatal("MySQL offline blocked confirmed HTTP send", result)
		}
		readAccepted(t, result.body.Data)
		assertEmpty(t)
	})
}
