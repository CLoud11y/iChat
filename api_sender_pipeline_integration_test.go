//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"iChat/database"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
	"iChat/migrations"
	"iChat/tests/clientsim"
	"iChat/workers"
)

// A fixture-only admission gate lets normal-path suites stop between actual
// SQL claims. Cancellation during a claim may correctly report commit unknown;
// that fault window belongs to the dedicated Outbox acceptance suites.
type senderOutboxStore struct {
	*database.OutboxRepository
	claim chan struct{}
}

func (s *senderOutboxStore) Claim(ctx context.Context, o database.OutboxClaimOptions) ([]database.OutboxLease, error) {
	select {
	case s.claim <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-s.claim }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.OutboxRepository.Claim(ctx, o)
}

type senderPipeline struct {
	persist *workers.PersistenceService
	outbox  *workers.OutboxService
	session *rabbitmq.ConsumerSession
	store   *senderOutboxStore
}

func (p *senderPipeline) drainOutbox(ctx context.Context) error {
	select {
	case p.store.claim <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-p.store.claim }()
	return p.outbox.Drain(ctx)
}

// Both services run their production loops. The fixture never receives a
// delivery, calls PersistCommand/RunBatch, or acknowledges a broker delivery.
// This explicit assembly does not register default worker/all factories.
func (f *senderResultFixture) startSenderPipeline(t *testing.T) *senderPipeline {
	t.Helper()
	setup, stop := context.WithTimeout(f.ctx, 10*time.Second)
	defer stop()
	session, err := rabbitmq.DialConsumer(setup, f.address, f.management, f.vhost, rabbitmq.DefaultConnectionOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := session.Close(c); err != nil {
			t.Error(err)
		}
		if err := session.WaitClosed(c); err != nil {
			t.Error(err)
		}
	})
	po := workers.DefaultPersistenceOptions()
	po.PollTimeout = 100 * time.Millisecond
	persist, err := workers.NewPersistenceService(f.pool, []workers.PersistenceConsumer{{Session: session, Prefetch: 1}}, po)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := persist.Close(c); err != nil {
			t.Error(err)
		}
	})
	if err := persist.Start(setup, f.ctx); err != nil {
		t.Fatal(err)
	}
	store, err := database.NewOutboxRepository(f.pool)
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := workers.NewRedisNotificationPublisher(f.client)
	if err != nil {
		t.Fatal(err)
	}
	oo := workers.DefaultOutboxServiceOptions()
	oo.Loop.PollInterval = 20 * time.Millisecond
	gate := &senderOutboxStore{OutboxRepository: store, claim: make(chan struct{}, 1)}
	outbox, err := workers.NewOutboxService(func(context.Context) (workers.OutboxResources, error) {
		return workers.OutboxResources{Store: gate, Publisher: publisher, ProbeStorage: f.pool.PingContext,
			ProbeNotification: func(c context.Context) error { return f.client.Ping(c).Err() }}, nil
	}, oo)
	if err != nil {
		t.Fatal(err)
	}
	pipeline := &senderPipeline{persist: persist, outbox: outbox, session: session, store: gate}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := pipeline.drainOutbox(c); err != nil {
			t.Error(err)
		}
		if err := outbox.Close(c); err != nil {
			t.Error(err)
		}
	})
	if err := outbox.Start(setup, f.ctx); err != nil {
		t.Fatal(err)
	}
	if !persist.ConsumerReady() || !outbox.OutboxReady() {
		t.Fatal("production sender pipeline not ready")
	}
	return pipeline
}

func (f *senderResultFixture) readSenderResult(t *testing.T, key string) []byte {
	t.Helper()
	f.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, body, err := f.conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		var frame struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(body, &frame); err != nil {
			t.Fatal("invalid production frame", err)
		}
		switch frame.Type {
		case "ping":
			if err := f.conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"pong","data":{}}`)); err != nil {
				t.Fatal(err)
			}
		case "message", "sync.required": // Receive-side storage belongs to P6.4.
		case "command.result":
			value, err := messaging.DecodeCommandResult(body, uint64(f.users[0].ID))
			if err != nil || value.ClientMessageID != key {
				t.Fatal("unexpected production result", err)
			}
			return body
		default:
			t.Fatal("unexpected production frame type", frame.Type)
		}
	}
}

type pipelineSender struct {
	store  *clientsim.FileStore
	sender *clientsim.Sender
	runner *clientsim.HTTPRunner
}

func (f *senderResultFixture) openPipelineSender(t *testing.T, directory string) *pipelineSender {
	t.Helper()
	o := clientsim.DefaultHTTPOptions("http://"+apiAddress(f.api), func(context.Context) (string, error) { return f.token, nil })
	o.MaxConcurrent = 1
	return f.openPipelineSenderWithOptions(t, directory, o)
}

func (f *senderResultFixture) openPipelineSenderWithOptions(t *testing.T, directory string, o clientsim.HTTPOptions) *pipelineSender {
	t.Helper()
	store, err := clientsim.OpenFileStore(f.ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	sender, err := clientsim.OpenSender(f.ctx, uint64(f.users[0].ID), store)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := clientsim.NewHTTPRunner(sender, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runner.Close)
	return &pipelineSender{store: store, sender: sender, runner: runner}
}

func (p *pipelineSender) entry(t *testing.T, ctx context.Context) clientsim.PendingEntry {
	t.Helper()
	entries, err := p.sender.Entries(ctx)
	if err != nil || len(entries) != 1 {
		t.Fatal("unexpected durable local collection", len(entries), err)
	}
	return entries[0]
}

func (p *pipelineSender) close(t *testing.T) {
	t.Helper()
	p.runner.Close()
	if err := p.store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestProductionSenderPipelineIntegration(t *testing.T) {
	f := newSenderResultFixture(t)
	pipeline := f.startSenderPipeline(t)
	repo, err := database.NewMessageRepository(f.pool)
	if err != nil {
		t.Fatal(err)
	}
	original := make(map[string]messaging.CommandStatus)
	for i, mode := range []string{"persisted_before_HTTP", "rejected_before_HTTP", "offline_group_restart_query"} {
		if !t.Run(mode, func(t *testing.T) {
			target, kind, state, reason, eventCount := f.users[1].ID, messaging.Private, clientsim.Persisted, "", 2
			if i == 1 {
				target, state, reason, eventCount = f.users[2].ID, clientsim.Rejected, "NOT_FRIENDS", 1
			}
			if i == 2 {
				target, kind = f.group.ID, messaging.Group
				if err := f.conn.Close(); err != nil {
					t.Fatal(err)
				}
				// Prove Redis will publish this command's result with no receiver.
				channel := "ichat:notify:v1:user:" + strconv.FormatUint(uint64(f.users[0].ID), 10)
				apiRealEventually(t, func() bool {
					counts, err := f.client.PubSubNumSub(f.ctx, channel).Result()
					return err == nil && counts[channel] == 0
				})
			}
			directory := t.TempDir()
			local := f.openPipelineSender(t, directory)
			request := messaging.SendRequest{ClientMessageID: fmt.Sprintf("ABCD5678-1234-4234-8234-%012d", i+1),
				TargetType: kind, TargetID: "000" + strconv.FormatUint(uint64(target), 10), Media: messaging.Text,
				Content: fmt.Sprintf("sender-pipeline-%d \r\n中🙂\x00", i), ClientSentAt: "2026-11-01T12:34:56.123456789+11:00"}
			entry, err := local.sender.Queue(f.ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			barrier := &senderResultReceipt{entered: make(chan struct{}), release: make(chan struct{}), returned: make(chan struct{})}
			f.publisher.mu.Lock()
			f.publisher.barriers[entry.CommandID] = barrier
			f.publisher.mu.Unlock()
			t.Cleanup(barrier.open)
			defer barrier.open()
			done := make(chan error, 1)
			go func() { done <- local.runner.Submit(f.ctx, entry.ClientMessageID) }()
			select {
			case <-barrier.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("real broker confirmation not observed")
			}
			var status messaging.CommandStatus
			apiRealEventually(t, func() bool {
				status, err = repo.GetCommandStatus(f.ctx, uint64(f.users[0].ID), entry.ClientMessageID, entry.PayloadHash)
				return err == nil && status.State == messaging.StatusState(state)
			})
			if status.ReasonCode != reason {
				t.Fatal("wrong SQL terminal reason")
			}
			original[entry.CommandID] = status
			apiRealEventually(t, func() bool {
				var count int
				err := f.pool.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM message_outbox WHERE command_id=? AND status='completed'", entry.CommandID).Scan(&count)
				return err == nil && count == eventCount
			})
			if i != 2 {
				body := f.readSenderResult(t, entry.ClientMessageID)
				if query, err := local.runner.ObserveCommandResult(f.ctx, body); err != nil || query {
					t.Fatal("full production result did not reconcile", err)
				}
				got := local.entry(t, f.ctx)
				if got.State != state || !got.InFlight || !reflect.DeepEqual(got.Result, &status) {
					t.Fatal("consumer/Outbox terminal not saved before HTTP receipt")
				}
				if query, err := local.runner.ObserveCommandResult(f.ctx, body); err != nil || query {
					t.Fatal("duplicate production result not idempotent", err)
				}
			} else if got := local.entry(t, f.ctx); got.State != clientsim.Submitting || got.Result != nil {
				t.Fatal("offline client learned an unobserved terminal")
			}
			select {
			case <-done:
				t.Fatal("HTTP returned before receipt release")
			default:
			}
			barrier.open()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("production HTTP receipt not joined")
			}
			if i == 2 && local.entry(t, f.ctx).State != clientsim.Accepted {
				t.Fatal("acceptance fabricated persistence")
			}
			local.close(t)
			local = f.openPipelineSender(t, directory)
			if i == 2 {
				got := local.entry(t, f.ctx)
				if got.State != clientsim.Accepted || got.Result != nil || got.InFlight {
					t.Fatal("restart changed unobserved acceptance")
				}
				policy := clientsim.DefaultRecoveryOptions()
				policy.MaxRounds, policy.MaxSubmissions = 1, 0
				if err := local.runner.Recover(f.ctx, entry.ClientMessageID, policy); err != nil {
					t.Fatal("production status did not recover missed notification", err)
				}
				local.close(t)
				local = f.openPipelineSender(t, directory)
			}
			got := local.entry(t, f.ctx)
			if got.State != state || got.InFlight || got.Attempts != 1 || got.Request != request ||
				got.ClientMessageID != entry.ClientMessageID || got.PayloadHash != entry.PayloadHash || got.CommandID != entry.CommandID ||
				!reflect.DeepEqual(got.Result, &status) {
				t.Fatal("reopened local terminal differs from original request/SQL")
			}
		}) {
			t.FailNow()
		}
	}
	// Join the only consumer connection before inspecting the queue. Any missed
	// production ACK would requeue on close and fail the non-consuming inspection.
	c, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	if err := pipeline.persist.Drain(c); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.persist.Close(c); err != nil {
		t.Fatal(err)
	}
	apiWait(t, pipeline.persist.Done())
	if err := pipeline.session.WaitClosed(c); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.drainOutbox(c); err != nil {
		t.Fatal(err)
	}
	apiWait(t, pipeline.outbox.Done())
	// Broker-side cancellation is observed independently of the local join.
	apiRealEventually(t, func() bool {
		queue, err := f.channel.QueueInspect(rabbitmq.PersistQueue)
		return err == nil && queue.Consumers == 0
	})
	queue, err := f.channel.QueueInspect(rabbitmq.PersistQueue)
	if err != nil || queue.Messages != 0 || queue.Consumers != 0 {
		t.Fatal("production consumer did not settle all deliveries", queue.Messages, queue.Consumers, err)
	}
	for table, want := range map[string]int{"message_command": 3, "message": 2, "conversation": 2, "message_outbox": 5, "message_delivery_cursor": 0, "message_quarantine": 0} {
		var n int
		if err := f.pool.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil || n != want {
			t.Fatal("wrong pipeline collection", table, n, err)
		}
	}
	var completed, watermarks int
	if err := f.pool.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM message_outbox WHERE status='completed'").Scan(&completed); err != nil || completed != 5 {
		t.Fatal("unfinished production Outbox", completed, err)
	}
	if err := f.pool.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM conversation WHERE last_seq=1").Scan(&watermarks); err != nil || watermarks != 2 {
		t.Fatal("unexpected conversation sequence allocation", watermarks, err)
	}
	for _, before := range original {
		after, err := repo.GetCommandStatus(f.ctx, uint64(f.users[0].ID), before.ClientMessageID, before.PayloadHash)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("original SQL terminal changed", err)
		}
	}
	if err := migrations.ApplyReliable(f.ctx, f.pool); err != nil {
		t.Fatal("schema fingerprint changed", err)
	}
	if err := f.api.Close(c); err != nil {
		t.Fatal(err)
	}
	apiWait(t, f.api.Done())
	if err := f.pool.PingContext(f.ctx); err != nil {
		t.Fatal("service closed borrowed fixture SQL", err)
	}
	if err := f.client.Ping(f.ctx).Err(); err != nil {
		t.Fatal("service closed borrowed fixture Redis", err)
	}
	t.Log("final 3 commands / 2 messages / 2 conversations (last_seq=1) / 5 completed Outbox / 0 cursors / 0 quarantine; 3 durable local terminals; consumer joined, queue ready=0 consumers=0")
}
