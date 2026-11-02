//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"

	"iChat/messaging"
	"iChat/messaging/rabbitmq"
	"iChat/tests/clientsim"
)

func TestProductionSenderPublisherRecoveryIntegration(t *testing.T) {
	var relay *apiRedisProxy // Existing raw TCP relay, carrying only publisher AMQP.
	f := newSenderResultFixtureWithOptions(t, nil, func(original string) string {
		u, err := url.Parse(original)
		if err != nil || u.Host == "" {
			t.Fatal("invalid owned publisher URI")
		}
		relay = newAPIRedisProxy(t, u.Host)
		relay.setBlocked(false)
		u.Host = relay.listener.Addr().String()
		return u.String()
	})
	ready, cancelReady := context.WithTimeout(f.ctx, 3*time.Second)
	firstEpoch, err := f.publisher.WaitReady(ready)
	cancelReady()
	if err != nil || firstEpoch == 0 {
		t.Fatal("publisher not initially ready", err)
	}
	relay.setBlocked(true) // No business publication has started on this connection.
	apiRealEventually(t, func() bool { return !f.publisher.Status().Ready })
	if state := f.publisher.Status().State; state == rabbitmq.SupervisorFailed || state == rabbitmq.SupervisorClosed {
		t.Fatal("publisher outage terminated managed recovery")
	}
	httpProxy := newSenderRecoveryProxyWithOptions(t, "http://"+apiAddress(f.api), senderRecoveryProxyOptions{
		allowNotPublished: true, holdSecondPost: true,
	})
	defer httpProxy.open()
	options, directory := senderGuardOptions(f, httpProxy), t.TempDir()
	local := f.openPipelineSenderWithOptions(t, directory, options)
	request := senderGuardRequest(f)
	entry, err := local.sender.Queue(f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	command, err := messaging.Normalize(uint64(f.users[0].ID), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.runner.Submit(f.ctx, entry.ClientMessageID); !errors.Is(err, clientsim.ErrHTTPUnavailable) {
		t.Fatal("unavailable publisher did not return controlled failure", err)
	}
	notPublished := local.entry(t, f.ctx)
	if notPublished.State != clientsim.NotPublished || notPublished.Attempts != 1 || notPublished.InFlight || notPublished.Result != nil || notPublished.Request != request {
		t.Fatal("real not_published receipt was not saved correctly")
	}
	trace := httpProxy.snapshot()
	if len(trace) != 1 || trace[0].status != 503 || trace[0].method != http.MethodPost {
		t.Fatal("unexpected unavailable publisher HTTP sequence")
	}
	first, err := messaging.DecodeSubmissionResponse(trace[0].response)
	if err != nil || first.Code != -1 || first.Data.State != "not_published" || first.Data.ReasonCode != "DEPENDENCY_UNAVAILABLE" ||
		first.Data.PublishAttemptID != "" || !first.Data.Matches(command) {
		t.Fatal("production publisher failure did not prove no attempt started", err)
	}
	assertEmpty := func() {
		t.Helper()
		queue, err := f.channel.QueueInspect(rabbitmq.PersistQueue)
		if err != nil || queue.Messages != 0 || queue.Consumers != 0 {
			t.Fatal("publisher recovery implicitly published a command", err)
		}
		for _, table := range []string{"message_command", "message", "message_outbox"} {
			var n int
			if err := f.pool.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil || n != 0 {
				t.Fatal("unstarted publish created business rows", table, n, err)
			}
		}
	}
	assertEmpty()
	local.close(t)
	local = f.openPipelineSenderWithOptions(t, directory, options)
	if !reflect.DeepEqual(notPublished, local.entry(t, f.ctx)) {
		t.Fatal("not_published checkpoint did not survive disk restart")
	}
	policy := clientsim.DefaultRecoveryOptions()
	policy.MaxRounds, policy.MaxSubmissions = 1, 1
	if err := local.runner.Recover(f.ctx, entry.ClientMessageID, policy); !errors.Is(err, clientsim.ErrRecoveryExhausted) {
		t.Fatal("SQL query was gated by publisher availability", err)
	}
	trace = httpProxy.snapshot()
	if len(trace) != 2 || trace[1].method != http.MethodGet || trace[1].status != 200 {
		t.Fatal("offline publisher prevented authoritative status query")
	}
	statusQuery, err := messaging.NormalizeStatusQuery(uint64(f.users[0].ID), entry.ClientMessageID, entry.PayloadHash)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := messaging.DecodeCommandStatusResponse(trace[1].response)
	if err != nil || pending.Code != 0 || pending.Data.State != messaging.StatusPendingOrUnknown || pending.Data.ValidateFor(statusQuery) != nil {
		t.Fatal("offline publisher did not return matching pending status", err)
	}
	checkpoint := local.entry(t, f.ctx)
	if checkpoint.State != clientsim.PublishUnknown || checkpoint.Attempts != 1 || checkpoint.Request != request || checkpoint.InFlight || !reflect.DeepEqual(checkpoint.Result, &pending.Data) {
		t.Fatal("pending query changed stable request or attempt count")
	}
	local.close(t)
	local = f.openPipelineSenderWithOptions(t, directory, options)
	if !reflect.DeepEqual(checkpoint, local.entry(t, f.ctx)) {
		t.Fatal("pending checkpoint did not survive restart")
	}
	relay.setBlocked(false)
	ready, cancelReady = context.WithTimeout(f.ctx, 20*time.Second)
	newEpoch, err := f.publisher.WaitReady(ready)
	cancelReady()
	if err != nil || newEpoch <= firstEpoch {
		t.Fatal("publisher did not recover with a fresh epoch", err)
	}
	assertEmpty() // Reconnecting the production supervisor never replays a POST.
	if len(httpProxy.snapshot()) != 2 {
		t.Fatal("network recovery started implicit client HTTP")
	}
	pipeline := f.startSenderPipeline(t)
	policy.MaxRounds, policy.MaxSubmissions = 3, 1
	done := make(chan error, 1)
	go func() { done <- local.runner.Recover(f.ctx, entry.ClientMessageID, policy) }()
	select {
	case <-httpProxy.held:
	case err := <-done:
		t.Fatal("recovery ended before actual same-key acceptance", err)
	case <-time.After(8 * time.Second):
		t.Fatal("recovered publisher did not accept original request")
	}
	status := awaitSenderGuardTerminal(t, f, entry)
	httpProxy.open()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("same-key recovery did not finish at original terminal", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("publisher recovery did not join")
	}
	terminal := local.entry(t, f.ctx)
	if terminal.State != clientsim.Persisted || terminal.Attempts != 2 || terminal.InFlight || terminal.Request != request ||
		terminal.CommandID != entry.CommandID || terminal.PayloadHash != entry.PayloadHash || !reflect.DeepEqual(terminal.Result, &status) {
		t.Fatal("recovered publisher changed original logical request")
	}
	body := f.readSenderResult(t, entry.ClientMessageID)
	if query, err := local.runner.ObserveCommandResult(f.ctx, body); err != nil || query || !reflect.DeepEqual(terminal, local.entry(t, f.ctx)) {
		t.Fatal("late notification changed recovered terminal", err)
	}
	local.close(t)
	local = f.openPipelineSenderWithOptions(t, directory, options)
	if !reflect.DeepEqual(terminal, local.entry(t, f.ctx)) {
		t.Fatal("publisher recovery terminal changed on disk restart")
	}
	trace = httpProxy.snapshot()
	if len(trace) != 5 || trace[2].method != http.MethodGet || trace[3].method != http.MethodPost || trace[3].status != 202 ||
		trace[4].method != http.MethodGet || trace[4].status != 200 || !bytes.Equal(trace[0].request, trace[3].request) {
		t.Fatal("recovery changed POST bytes or query/submit sequence")
	}
	permission, err := messaging.DecodeCommandStatusResponse(trace[2].response)
	if err != nil || trace[2].status != 200 || permission.Code != 0 || permission.Data.State != messaging.StatusPendingOrUnknown ||
		permission.Data.ValidateFor(statusQuery) != nil || trace[3].at.Sub(trace[2].at) < time.Duration(permission.Data.PollAfterMs)*time.Millisecond {
		t.Fatal("recovery lacked valid pending evidence or server wait floor", err)
	}
	accepted, err := messaging.DecodeSubmissionResponse(trace[3].response)
	if err != nil || accepted.Code != 0 || accepted.Data.State != "accepted" || !accepted.Data.Matches(command) ||
		!messaging.ValidPublishAttemptID(accepted.Data.PublishAttemptID) {
		t.Fatal("fresh publisher acceptance changed identity", err)
	}
	finishSenderGuard(t, f, pipeline, status)
	t.Logf("publisher epoch %d -> %d; initial real 503 not_published without attempt ID; reconnect queue empty; HTTP POST503 / GETpending / GETpending / POST202 / GETpersisted; attempts=2, exactly 1 logical message", firstEpoch, newEpoch)
}
