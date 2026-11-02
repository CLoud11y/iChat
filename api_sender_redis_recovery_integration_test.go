//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"iChat/database"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
	"iChat/tests/clientsim"
	"iChat/workers"
)

type senderFailedRedisEvent struct {
	payload  []byte
	attempts uint64
}

func senderFailedRedisEvents(t *testing.T, f *senderResultFixture, entry clientsim.PendingEntry) map[string]senderFailedRedisEvent {
	t.Helper()
	rows, err := f.pool.QueryContext(f.ctx, `SELECT event_id,event_type,payload,attempts FROM message_outbox
 WHERE command_id=? AND status='pending' AND attempts>0 AND last_error=?
 AND lease_token IS NULL AND lease_until IS NULL AND completed_at IS NULL`, entry.CommandID, database.OutboxRetryPublishUnknown)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := make(map[string]senderFailedRedisEvent)
	for rows.Next() {
		var id, kind string
		var event senderFailedRedisEvent
		if err := rows.Scan(&id, &kind, &event.payload, &event.attempts); err != nil {
			t.Fatal(err)
		}
		expected, err := messaging.OutboxEventID(entry.CommandID, messaging.OutboxEventType(kind))
		if err != nil || id != expected {
			t.Fatal("failed notification changed original event identity", err)
		}
		result[id] = event
	}
	if err := rows.Err(); err != nil || len(result) != 2 {
		t.Fatal("Redis failures did not settle two retryable original events", len(result), err)
	}
	return result
}

func TestProductionSenderRedisRecoveryIntegration(t *testing.T) {
	for _, initial := range []clientsim.SendState{clientsim.Accepted, clientsim.PublishUnknown} {
		t.Run(string(initial), func(t *testing.T) {
			f := newSenderResultFixture(t)
			originalRedis := f.client.Options()
			relay := newAPIRedisProxy(t, originalRedis.Addr)
			relay.setBlocked(false)
			notifications := redis.NewClient(&redis.Options{Addr: relay.listener.Addr().String(),
				Password: originalRedis.Password, DB: originalRedis.DB, MaxRetries: -1,
				DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, PoolTimeout: time.Second})
			t.Cleanup(func() { notifications.Close() })
			oo := workers.DefaultOutboxServiceOptions()
			oo.Loop.PollInterval = 20 * time.Millisecond
			oo.Loop.Retry.Max = time.Second // Keep genuine retry deadlines within the read window.
			oo.ProbeInterval = 5 * time.Second
			var failedErr error
			pipeline := f.startSenderPipelineWithNotifications(t, notifications, oo, func(err error) bool {
				return failedErr != nil && err == failedErr
			})
			failedRole := pipeline.outbox
			relay.setBlocked(true)
			if err := notifications.Ping(f.ctx).Err(); err == nil {
				t.Fatal("Outbox Redis path did not fail")
			}
			if err := f.client.Ping(f.ctx).Err(); err != nil {
				t.Fatal("notification-only fault interrupted API Redis", err)
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
				t.Fatal("unexpected initial real publish result", err)
			}
			before := local.entry(t, f.ctx)
			if before.State != initial || before.Attempts != 1 || before.Result != nil || before.InFlight {
				t.Fatal("failed notification fabricated client terminal")
			}
			select {
			case <-failedRole.Done():
			case <-time.After(15 * time.Second):
				t.Fatal("Outbox role did not stop after actual Redis probe failure")
			}
			var failure *workers.OutboxError
			observedErr := failedRole.Err()
			if !errors.As(observedErr, &failure) || failure.Stage != "dependency probe" || failedRole.OutboxReady() {
				t.Fatal("unexpected failed Outbox role outcome", observedErr)
			}
			failedErr = observedErr // Cleanup accepts only this exact, already-joined failure.
			failedEvents := senderFailedRedisEvents(t, f, entry)
			if !reflect.DeepEqual(before, local.entry(t, f.ctx)) {
				t.Fatal("Outbox failure changed local command")
			}
			// Closing the only consumer before Redis recovers proves ACK did not
			// wait for notification delivery. Missed ACKs would requeue here.
			stop, cancelStop := context.WithTimeout(f.ctx, 5*time.Second)
			if err := pipeline.persist.Drain(stop); err != nil {
				t.Fatal(err)
			}
			if err := pipeline.persist.Close(stop); err != nil {
				t.Fatal(err)
			}
			if err := pipeline.session.WaitClosed(stop); err != nil {
				t.Fatal(err)
			}
			cancelStop()
			apiRealEventually(t, func() bool {
				queue, err := f.channel.QueueInspect(rabbitmq.PersistQueue)
				return err == nil && queue.Consumers == 0
			})
			queue, err := f.channel.QueueInspect(rabbitmq.PersistQueue)
			if err != nil || queue.Messages != 0 || queue.Consumers != 0 {
				t.Fatal("Redis failure blocked production MQ ACK", err)
			}
			policy := clientsim.DefaultRecoveryOptions()
			policy.MaxRounds, policy.MaxSubmissions = 1, 1
			if err := local.runner.Recover(f.ctx, entry.ClientMessageID, policy); err != nil {
				t.Fatal("healthy SQL/auth did not recover terminal during notification outage", err)
			}
			repo, err := database.NewMessageRepository(f.pool)
			if err != nil {
				t.Fatal(err)
			}
			status, err := repo.GetCommandStatus(f.ctx, uint64(f.users[0].ID), entry.ClientMessageID, entry.PayloadHash)
			if err != nil || status.State != messaging.StatusPersisted || !reflect.DeepEqual(local.entry(t, f.ctx).Result, &status) {
				t.Fatal("fallback did not save complete original SQL terminal", err)
			}
			if !reflect.DeepEqual(failedEvents, senderFailedRedisEvents(t, f, entry)) {
				t.Fatal("status query modified failed Outbox events")
			}
			local.close(t)
			local = f.openPipelineSenderWithOptions(t, directory, options)
			terminal := local.entry(t, f.ctx)
			if terminal.State != clientsim.Persisted || terminal.Attempts != 1 || terminal.Request != request || terminal.InFlight || !reflect.DeepEqual(terminal.Result, &status) {
				t.Fatal("fallback terminal did not survive disk restart")
			}
			relay.setBlocked(false)
			apiRealEventually(t, func() bool { return notifications.Ping(f.ctx).Err() == nil })
			setup, cancelSetup := context.WithTimeout(f.ctx, 5*time.Second)
			defer cancelSetup()
			if err := failedRole.Start(setup, f.ctx); !errors.Is(err, workers.ErrOutboxServiceStart) || failedRole.OutboxReady() {
				t.Fatal("single-use failed role restarted itself", err)
			}
			publisher, err := workers.NewRedisNotificationPublisher(notifications)
			if err != nil {
				t.Fatal(err)
			}
			replacement, err := workers.NewOutboxService(func(context.Context) (workers.OutboxResources, error) {
				return workers.OutboxResources{Store: pipeline.store, Publisher: publisher, ProbeStorage: f.pool.PingContext,
					ProbeNotification: func(c context.Context) error { return notifications.Ping(c).Err() }}, nil
			}, oo)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := drainSenderOutbox(c, replacement, pipeline.store); err != nil {
					t.Error(err)
				}
				if err := replacement.Close(c); err != nil {
					t.Error(err)
				}
			})
			pipeline.outbox = replacement // Old owner Done closed before replacement construction.
			if err := replacement.Start(setup, f.ctx); err != nil || !replacement.OutboxReady() {
				t.Fatal("new Outbox role did not activate", err)
			}
			body := f.readSenderResult(t, entry.ClientMessageID)
			for i := 0; i < 2; i++ {
				if query, err := local.runner.ObserveCommandResult(f.ctx, body); err != nil || query {
					t.Fatal("late complete notification did not reconcile idempotently", err)
				}
			}
			if !reflect.DeepEqual(terminal, local.entry(t, f.ctx)) {
				t.Fatal("late notification changed fallback terminal")
			}
			awaitSenderGuardTerminal(t, f, entry)
			for id, old := range failedEvents {
				var payload []byte
				var attempts uint64
				var state string
				var clean bool
				if err := f.pool.QueryRowContext(f.ctx, "SELECT payload,attempts,status,last_error IS NULL FROM message_outbox WHERE event_id=?", id).
					Scan(&payload, &attempts, &state, &clean); err != nil || !bytes.Equal(payload, old.payload) || attempts <= old.attempts || state != "completed" || !clean {
					t.Fatal("replacement did not complete same immutable event", err)
				}
			}
			local.close(t)
			local = f.openPipelineSenderWithOptions(t, directory, options)
			if !reflect.DeepEqual(terminal, local.entry(t, f.ctx)) {
				t.Fatal("late-notification terminal changed across disk restart")
			}
			trace := httpProxy.snapshot()
			if len(trace) != 2 || trace[0].method != http.MethodPost || trace[1].method != http.MethodGet || trace[1].status != 200 {
				t.Fatal("notification failure triggered a new submission")
			}
			if failedRole.Err() != failedErr || failedRole.OutboxReady() {
				t.Fatal("replacement erased failed owner's outcome")
			}
			finishSenderGuard(t, f, pipeline, status)
			t.Logf("initial=%s; notification Redis failed, 2 original events retried; old Outbox joined before new instance; SQL fallback before late real WebSocket result; 1 POST / 1 GET, attempts=1", initial)
		})
	}
}
