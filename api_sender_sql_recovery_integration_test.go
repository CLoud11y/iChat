//go:build integration

package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"testing"
	"time"

	"iChat/config"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
	"iChat/tests/clientsim"
)

// The raw TCP relay interrupts only the default API's real MySQL connection
// path. Fixture/consumer SQL, Redis and RabbitMQ remain real and reachable.
// The HTTP recorder forwards the resulting production 503s without alteration.
func TestProductionSenderSQLRecoveryIntegration(t *testing.T) {
	for _, initial := range []clientsim.SendState{clientsim.Accepted, clientsim.PublishUnknown} {
		t.Run(string(initial), func(t *testing.T) {
			var relay *apiRedisProxy // Existing protocol-agnostic TCP relay.
			f := newSenderResultFixtureWithAPIConfig(t, func(c *config.Config) {
				relay = newAPIRedisProxy(t, net.JoinHostPort(c.MYSQL.Host, c.MYSQL.Port))
				relay.setBlocked(false)
				host, port, err := net.SplitHostPort(relay.listener.Addr().String())
				if err != nil {
					t.Fatal(err)
				}
				c.MYSQL.Host, c.MYSQL.Port = host, port
			})
			if err := f.conn.Close(); err != nil {
				t.Fatal(err)
			}
			personal := "ichat:notify:v1:user:" + strconv.FormatUint(uint64(f.users[0].ID), 10)
			apiRealEventually(t, func() bool {
				counts, err := f.client.PubSubNumSub(f.ctx, personal).Result()
				return err == nil && counts[personal] == 0
			})
			ownedSQL := f.api.resources.sql
			warm, cancelWarm := context.WithTimeout(f.ctx, 3*time.Second)
			err := ownedSQL.PingContext(warm)
			cancelWarm()
			if err != nil {
				t.Fatal("original API pool did not connect through relay", err)
			}
			relay.mu.Lock()
			connected := len(relay.connections) > 0
			relay.mu.Unlock()
			if !connected {
				t.Fatal("API MySQL relay had no actual connection")
			}
			httpProxy := newSenderRecoveryProxyWithOptions(t, "http://"+apiAddress(f.api), senderRecoveryProxyOptions{
				dropFirstPost: initial == clientsim.PublishUnknown,
			})
			options, directory := senderGuardOptions(f, httpProxy), t.TempDir()
			local := f.openPipelineSenderWithOptions(t, directory, options)
			request := senderGuardRequest(f)
			entry, err := local.sender.Queue(f.ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			err = local.runner.Submit(f.ctx, entry.ClientMessageID)
			if (initial == clientsim.Accepted && err != nil) ||
				(initial == clientsim.PublishUnknown && !errors.Is(err, clientsim.ErrHTTPUnavailable)) {
				t.Fatal("unexpected initial actual publish result", err)
			}
			before := local.entry(t, f.ctx)
			if before.State != initial || before.Attempts != 1 || before.InFlight || before.Result != nil {
				t.Fatal("invalid initial durable state")
			}
			relay.setBlocked(true) // Close existing sockets and refuse new ones.
			probe, cancelProbe := context.WithTimeout(f.ctx, time.Second)
			err = ownedSQL.PingContext(probe)
			cancelProbe()
			if err == nil {
				t.Fatal("API SQL path remained reachable during interruption")
			}
			if err := f.pool.PingContext(f.ctx); err != nil {
				t.Fatal("API-only fault interrupted independent fixture pool", err)
			}
			policy := clientsim.DefaultRecoveryOptions()
			policy.MaxRounds, policy.MaxSubmissions = 2, 1
			expectSQLUnavailable := func() {
				t.Helper()
				if err := local.runner.Recover(f.ctx, entry.ClientMessageID, policy); !errors.Is(err, clientsim.ErrRecoveryExhausted) || !errors.Is(err, clientsim.ErrHTTPUnavailable) {
					t.Fatal("real SQL 503 did not exhaust query-only recovery", err)
				}
				if !reflect.DeepEqual(before, local.entry(t, f.ctx)) {
					t.Fatal("SQL error downgraded or fabricated durable state")
				}
			}
			expectSQLUnavailable()
			queue, err := f.channel.QueueInspect(rabbitmq.PersistQueue)
			if err != nil || queue.Messages != 1 || queue.Consumers != 0 {
				t.Fatal("SQL query failure republished original command", err)
			}
			for _, table := range []string{"message_command", "message", "message_outbox"} {
				var n int
				if err := f.pool.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil || n != 0 {
					t.Fatal("unconsumed accepted command was already persisted", table, n, err)
				}
			}
			// The independent production consumer commits while API SQL is still
			// unreachable. Neither commit nor Outbox completion informs the client.
			pipeline := f.startSenderPipeline(t)
			status := awaitSenderGuardTerminal(t, f, entry)
			expectSQLUnavailable()
			trace := httpProxy.snapshot()
			if len(trace) != 5 || trace[0].method != http.MethodPost {
				t.Fatal("SQL outage allowed a POST instead of four queries", len(trace))
			}
			for _, exchange := range trace[1:] {
				value, err := messaging.DecodeCommandStatusResponse(exchange.response)
				if err != nil || exchange.method != http.MethodGet || exchange.status != 503 || value.Code != -1 ||
					value.Data.ReasonCode != "DEPENDENCY_UNAVAILABLE" || value.Data.State != "" || value.Data.Message != nil ||
					value.Data.ClientMessageID != "" || value.Data.PayloadHash != "" || value.Data.PollAfterMs != 0 ||
					exchange.query.Get("clientMessageId") != entry.ClientMessageID || exchange.query.Get("payloadHash") != entry.PayloadHash {
					t.Fatal("production SQL failure exposed partial/pending result", err)
				}
			}
			local.close(t)
			local = f.openPipelineSenderWithOptions(t, directory, options)
			if !reflect.DeepEqual(before, local.entry(t, f.ctx)) {
				t.Fatal("SQL-unavailable checkpoint changed on disk restart")
			}
			relay.setBlocked(false)
			apiRealEventually(t, func() bool {
				c, cancel := context.WithTimeout(f.ctx, time.Second)
				defer cancel()
				return ownedSQL.PingContext(c) == nil
			})
			if f.api.resources.sql != ownedSQL {
				t.Fatal("recovery replaced original API pool")
			}
			policy.MaxRounds, policy.MaxSubmissions = 1, 1
			if err := local.runner.Recover(f.ctx, entry.ClientMessageID, policy); err != nil {
				t.Fatal("original API pool did not return committed terminal", err)
			}
			local.close(t)
			local = f.openPipelineSenderWithOptions(t, directory, options)
			got := local.entry(t, f.ctx)
			if got.State != clientsim.Persisted || got.Attempts != 1 || got.InFlight || got.Request != request ||
				got.ClientMessageID != entry.ClientMessageID || got.CommandID != entry.CommandID || got.PayloadHash != entry.PayloadHash ||
				!reflect.DeepEqual(got.Result, &status) {
				t.Fatal("recovered SQL terminal differs from original command/disk")
			}
			final := httpProxy.snapshot()
			if len(final) != 6 || final[5].method != http.MethodGet || final[5].status != 200 {
				t.Fatal("recovery republished instead of reading original terminal")
			}
			finishSenderGuard(t, f, pipeline, status)
			t.Logf("initial=%s; 1 actual publication / 4 real SQL 503 queries / 1 recovered terminal GET; same API pool; attempts=1 and original request retained", initial)
		})
	}
}
