//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"iChat/database"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
	"iChat/migrations"
	"iChat/tests/clientsim"
)

func senderGuardRequest(f *senderResultFixture) messaging.SendRequest {
	return messaging.SendRequest{ClientMessageID: "ABCD3456-1234-4234-8234-000000000001", TargetType: messaging.Private,
		TargetID: "000" + strconv.FormatUint(uint64(f.users[1].ID), 10), Media: messaging.Text,
		Content: "production-recovery-guard \r\n中🙂\x00", ClientSentAt: "2026-11-02T12:34:56.123456789+11:00"}
}

func senderGuardOptions(f *senderResultFixture, proxy *senderRecoveryProxy) clientsim.HTTPOptions {
	o := clientsim.DefaultHTTPOptions(proxy.server.URL, func(context.Context) (string, error) { return f.token, nil })
	o.MaxConcurrent, o.RequestTimeout = 1, 10*time.Second
	return o
}

func awaitSenderGuardTerminal(t *testing.T, f *senderResultFixture, entry clientsim.PendingEntry) messaging.CommandStatus {
	t.Helper()
	repo, err := database.NewMessageRepository(f.pool)
	if err != nil {
		t.Fatal(err)
	}
	var status messaging.CommandStatus
	apiRealEventually(t, func() bool {
		status, err = repo.GetCommandStatus(f.ctx, uint64(f.users[0].ID), entry.ClientMessageID, entry.PayloadHash)
		return err == nil && status.State == messaging.StatusPersisted
	})
	apiRealEventually(t, func() bool {
		var completed int
		err := f.pool.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM message_outbox WHERE command_id=? AND status='completed'", entry.CommandID).Scan(&completed)
		return err == nil && completed == 2
	})
	return status
}

func finishSenderGuard(t *testing.T, f *senderResultFixture, pipeline *senderPipeline, original messaging.CommandStatus) {
	t.Helper()
	c, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	if err := pipeline.persist.Drain(c); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.persist.Close(c); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.session.WaitClosed(c); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.drainOutbox(c); err != nil {
		t.Fatal(err)
	}
	assertSenderGuardCollections(t, f, original)
}

func assertSenderGuardCollections(t *testing.T, f *senderResultFixture, original messaging.CommandStatus) {
	t.Helper()
	apiRealEventually(t, func() bool {
		queue, err := f.channel.QueueInspect(rabbitmq.PersistQueue)
		return err == nil && queue.Consumers == 0
	})
	queue, err := f.channel.QueueInspect(rabbitmq.PersistQueue)
	if err != nil || queue.Messages != 0 || queue.Consumers != 0 {
		t.Fatal("guard scenario did not settle original publication", err)
	}
	for table, want := range map[string]int{"message_command": 1, "message": 1, "conversation": 1, "message_outbox": 2, "message_delivery_cursor": 0, "message_quarantine": 0} {
		var n int
		if err := f.pool.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil || n != want {
			t.Fatal("guard changed logical collection", table, n, err)
		}
	}
	var completed, watermark int
	if err := f.pool.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM message_outbox WHERE status='completed'").Scan(&completed); err != nil || completed != 2 {
		t.Fatal("guard left unfinished Outbox", err)
	}
	if err := f.pool.QueryRowContext(f.ctx, "SELECT last_seq FROM conversation").Scan(&watermark); err != nil || watermark != 1 {
		t.Fatal("guard allocated another sequence", err)
	}
	repo, err := database.NewMessageRepository(f.pool)
	if err != nil {
		t.Fatal(err)
	}
	after, err := repo.GetCommandStatus(f.ctx, uint64(f.users[0].ID), original.ClientMessageID, original.PayloadHash)
	if err != nil || !reflect.DeepEqual(after, original) {
		t.Fatal("guard changed original SQL terminal", err)
	}
	if err := migrations.ApplyReliable(f.ctx, f.pool); err != nil {
		t.Fatal("schema fingerprint changed", err)
	}
	t.Log("final 1 command / 1 message / 1 conversation last_seq=1 / 2 completed Outbox / 0 cursor / 0 quarantine; original SQL terminal unchanged; queue ready=0 consumers=0")
}

func TestProductionSenderRecoveryGuardsIntegration(t *testing.T) {
	t.Run("lost_status_responses_never_authorize_repost", testSenderLostStatusGuard)
	t.Run("same_key_conflict_blocks_original_overwrite", testSenderConflictGuard)
	t.Run("terminal_notification_precedes_late_pending", testSenderLatePendingGuard)
}

func testSenderLostStatusGuard(t *testing.T) {
	f := newSenderResultFixture(t)
	var drop atomic.Bool
	drop.Store(true)
	proxy := newSenderRecoveryProxyWithOptions(t, "http://"+apiAddress(f.api), senderRecoveryProxyOptions{
		dropFirstPost: true, statusAction: func(senderRecoveryExchange) senderRecoveryAction {
			if drop.Load() {
				return senderRecoveryDrop
			}
			return senderRecoveryForward
		},
	})
	options, directory := senderGuardOptions(f, proxy), t.TempDir()
	local := f.openPipelineSenderWithOptions(t, directory, options)
	request := senderGuardRequest(f)
	entry, err := local.sender.Queue(f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.runner.Submit(f.ctx, entry.ClientMessageID); !errors.Is(err, clientsim.ErrHTTPUnavailable) {
		t.Fatal("initial accepted response not lost", err)
	}
	before := local.entry(t, f.ctx)
	policy := clientsim.DefaultRecoveryOptions()
	policy.MaxRounds, policy.MaxSubmissions = 3, 2
	if err := local.runner.Recover(f.ctx, entry.ClientMessageID, policy); !errors.Is(err, clientsim.ErrRecoveryExhausted) || !errors.Is(err, clientsim.ErrHTTPUnavailable) {
		t.Fatal("lost status recovery did not exhaust without resending", err)
	}
	if !reflect.DeepEqual(before, local.entry(t, f.ctx)) || before.State != clientsim.PublishUnknown || before.Attempts != 1 {
		t.Fatal("failed query changed durable command")
	}
	trace := proxy.snapshot()
	if len(trace) != 4 || trace[0].method != http.MethodPost {
		t.Fatal("failed queries triggered another submission", len(trace))
	}
	query, err := messaging.NormalizeStatusQuery(uint64(f.users[0].ID), entry.ClientMessageID, entry.PayloadHash)
	if err != nil {
		t.Fatal(err)
	}
	for _, exchange := range trace[1:] {
		value, err := messaging.DecodeCommandStatusResponse(exchange.response)
		if exchange.method != http.MethodGet || exchange.status != 200 || err != nil || value.Code != 0 ||
			value.Data.State != messaging.StatusPendingOrUnknown || value.Data.ValidateFor(query) != nil {
			t.Fatal("lost response was not an actual matching pending query", err)
		}
	}
	queue, err := f.channel.QueueInspect(rabbitmq.PersistQueue)
	if err != nil || queue.Messages != 1 || queue.Consumers != 0 {
		t.Fatal("failed query republished original command", err)
	}
	local.close(t)
	local = f.openPipelineSenderWithOptions(t, directory, options)
	if !reflect.DeepEqual(before, local.entry(t, f.ctx)) {
		t.Fatal("restart lost original unknown after failed queries")
	}
	drop.Store(false)
	pipeline := f.startSenderPipeline(t)
	status := awaitSenderGuardTerminal(t, f, entry)
	f.readSenderResult(t, entry.ClientMessageID) // Explicitly discard, do not apply.
	if local.entry(t, f.ctx).State != clientsim.PublishUnknown {
		t.Fatal("unobserved notification changed sender state")
	}
	policy.MaxRounds, policy.MaxSubmissions = 1, 1
	if err := local.runner.Recover(f.ctx, entry.ClientMessageID, policy); err != nil {
		t.Fatal("repaired status query did not recover original terminal", err)
	}
	local.close(t)
	local = f.openPipelineSenderWithOptions(t, directory, options)
	got := local.entry(t, f.ctx)
	if got.Request != request || got.Attempts != 1 || got.InFlight || got.State != clientsim.Persisted || !reflect.DeepEqual(got.Result, &status) {
		t.Fatal("failed-query terminal did not survive restart")
	}
	if final := proxy.snapshot(); len(final) != 5 || final[4].method != http.MethodGet {
		t.Fatal("repaired query republished instead of reconciling")
	}
	finishSenderGuard(t, f, pipeline, status)
	t.Log("1 actual publication; 3 actual pending responses lost; 0 reposts despite budget=2; repaired GET saved terminal")
}

func testSenderConflictGuard(t *testing.T) {
	f := newSenderResultFixture(t)
	pipeline := f.startSenderPipeline(t)
	sourceDirectory := t.TempDir()
	source := f.openPipelineSender(t, sourceDirectory)
	request := senderGuardRequest(f)
	entry, err := source.sender.Queue(f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.runner.Submit(f.ctx, entry.ClientMessageID); err != nil {
		t.Fatal(err)
	}
	body := f.readSenderResult(t, entry.ClientMessageID)
	if query, err := source.runner.ObserveCommandResult(f.ctx, body); err != nil || query {
		t.Fatal("source result did not persist", err)
	}
	status := awaitSenderGuardTerminal(t, f, entry)
	changed := request
	changed.Content += "-changed"
	if _, err := source.sender.Queue(f.ctx, changed); !errors.Is(err, clientsim.ErrKeyConflict) {
		t.Fatal("existing local request allowed key reuse", err)
	}
	proxy := newSenderRecoveryProxyWithOptions(t, "http://"+apiAddress(f.api), senderRecoveryProxyOptions{})
	options, directory := senderGuardOptions(f, proxy), t.TempDir()
	fork := f.openPipelineSenderWithOptions(t, directory, options)
	conflict, err := fork.sender.Queue(f.ctx, changed)
	if err != nil || conflict.CommandID != entry.CommandID || conflict.PayloadHash == entry.PayloadHash {
		t.Fatal("invalid independent conflicting checkpoint", err)
	}
	// Model a restored, interrupted local dispatch. No conflicting HTTP POST or
	// MQ delivery is issued; this suite verifies the authoritative query gate.
	attempt, err := fork.sender.BeginSubmit(f.ctx, conflict.ClientMessageID)
	if err != nil {
		t.Fatal(err)
	}
	if err := fork.sender.CompleteUnknown(f.ctx, attempt); err != nil {
		t.Fatal(err)
	}
	policy := clientsim.DefaultRecoveryOptions()
	policy.MaxRounds, policy.MaxSubmissions = 3, 2
	if err := fork.runner.Recover(f.ctx, conflict.ClientMessageID, policy); !errors.Is(err, clientsim.ErrBlocked) {
		t.Fatal("real conflict did not block recovery", err)
	}
	trace := proxy.snapshot()
	if len(trace) != 1 || trace[0].method != http.MethodGet || trace[0].status != 409 {
		t.Fatal("conflicting checkpoint was submitted instead of queried")
	}
	value, err := messaging.DecodeCommandStatusResponse(trace[0].response)
	q, queryErr := messaging.NormalizeStatusQuery(uint64(f.users[0].ID), conflict.ClientMessageID, conflict.PayloadHash)
	if err != nil || queryErr != nil || value.Code != -1 || value.Data.State != messaging.StatusConflict || value.Data.ValidateFor(q) != nil ||
		value.Data.Message != nil || bytes.Contains(trace[0].response, []byte(entry.PayloadHash)) {
		t.Fatal("conflict leaked or replaced original result", err)
	}
	blocked := fork.entry(t, f.ctx)
	if blocked.State != clientsim.Conflict || blocked.Request != changed || blocked.Attempts != 1 || blocked.InFlight || !reflect.DeepEqual(blocked.Result, &value.Data) {
		t.Fatal("conflict checkpoint not durably retained")
	}
	if _, err := fork.runner.ObserveCommandResult(f.ctx, body); !errors.Is(err, clientsim.ErrProtocol) {
		t.Fatal("original-hash notification overwrote conflicting request", err)
	}
	if !reflect.DeepEqual(blocked, fork.entry(t, f.ctx)) {
		t.Fatal("wrong-hash notice changed conflict")
	}
	fork.close(t)
	fork = f.openPipelineSenderWithOptions(t, directory, options)
	if !reflect.DeepEqual(blocked, fork.entry(t, f.ctx)) {
		t.Fatal("conflict did not survive disk restart")
	}
	if err := fork.runner.Recover(f.ctx, conflict.ClientMessageID, policy); !errors.Is(err, clientsim.ErrBlocked) {
		t.Fatal("reopened conflict resumed automatic recovery", err)
	}
	if err := fork.runner.Submit(f.ctx, conflict.ClientMessageID); !errors.Is(err, clientsim.ErrBlocked) {
		t.Fatal("reopened conflict allowed direct submission", err)
	}
	if len(proxy.snapshot()) != 1 {
		t.Fatal("blocked conflict performed additional HTTP")
	}
	source.close(t)
	source = f.openPipelineSender(t, sourceDirectory)
	if got := source.entry(t, f.ctx); got.Request != request || got.State != clientsim.Persisted || got.Attempts != 1 || !reflect.DeepEqual(got.Result, &status) {
		t.Fatal("conflicting checkpoint changed original disk terminal")
	}
	finishSenderGuard(t, f, pipeline, status)
	t.Log("1 original publication; conflicting checkpoint only GET 409; no conflicting POST; original and conflict disks remain separate")
}

func testSenderLatePendingGuard(t *testing.T) {
	f := newSenderResultFixture(t)
	proxy := newSenderRecoveryProxyWithOptions(t, "http://"+apiAddress(f.api), senderRecoveryProxyOptions{
		dropFirstPost: true, statusAction: func(senderRecoveryExchange) senderRecoveryAction { return senderRecoveryHold },
	})
	defer proxy.open()
	options, directory := senderGuardOptions(f, proxy), t.TempDir()
	local := f.openPipelineSenderWithOptions(t, directory, options)
	request := senderGuardRequest(f)
	entry, err := local.sender.Queue(f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.runner.Submit(f.ctx, entry.ClientMessageID); !errors.Is(err, clientsim.ErrHTTPUnavailable) {
		t.Fatal("initial accepted response not lost", err)
	}
	local.close(t)
	local = f.openPipelineSenderWithOptions(t, directory, options)
	policy := clientsim.DefaultRecoveryOptions()
	policy.MaxRounds, policy.MaxSubmissions = 3, 2
	done := make(chan error, 1)
	go func() { done <- local.runner.Recover(f.ctx, entry.ClientMessageID, policy) }()
	select {
	case <-proxy.held:
	case err := <-done:
		t.Fatal("recovery did not wait for held status", err)
	case <-time.After(5 * time.Second):
		t.Fatal("production pending query not observed")
	}
	trace := proxy.snapshot()
	if len(trace) != 2 || trace[1].method != http.MethodGet || trace[1].status != 200 {
		t.Fatal("invalid held query sequence")
	}
	pending, err := messaging.DecodeCommandStatusResponse(trace[1].response)
	if err != nil || pending.Code != 0 || pending.Data.State != messaging.StatusPendingOrUnknown {
		t.Fatal("held query was not actual pending", err)
	}
	pipeline := f.startSenderPipeline(t)
	body := f.readSenderResult(t, entry.ClientMessageID)
	if query, err := local.runner.ObserveCommandResult(f.ctx, body); err != nil || query {
		t.Fatal("early complete notification did not reconcile", err)
	}
	status := awaitSenderGuardTerminal(t, f, entry)
	before := local.entry(t, f.ctx)
	if before.State != clientsim.Persisted || before.Attempts != 1 || !reflect.DeepEqual(before.Result, &status) {
		t.Fatal("notification terminal absent while query blocked")
	}
	select {
	case <-done:
		t.Fatal("query returned before pending release")
	default:
	}
	proxy.open()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("late pending did not finish at known terminal", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("late pending recovery did not join")
	}
	if !reflect.DeepEqual(before, local.entry(t, f.ctx)) || len(proxy.snapshot()) != 2 {
		t.Fatal("late pending downgraded terminal or authorized repost")
	}
	local.close(t)
	local = f.openPipelineSenderWithOptions(t, directory, options)
	if !reflect.DeepEqual(before, local.entry(t, f.ctx)) {
		t.Fatal("late-pending terminal did not survive restart")
	}
	if err := local.runner.Recover(f.ctx, entry.ClientMessageID, policy); err != nil || len(proxy.snapshot()) != 2 {
		t.Fatal("known terminal performed further recovery HTTP", err)
	}
	finishSenderGuard(t, f, pipeline, status)
	t.Log("1 actual publication; complete WebSocket terminal saved before held GET pending; 0 reposts despite budget=2")
}
