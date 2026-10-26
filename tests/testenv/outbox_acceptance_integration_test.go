//go:build integration

package testenv

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	mysql "github.com/go-sql-driver/mysql"
	amqp "github.com/rabbitmq/amqp091-go"
	"iChat/application"
	"iChat/database"
	"iChat/health"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
	"iChat/migrations"
	"iChat/workers"
)

// All interception is fixture-only. Every actual operation goes through the
// production repository/publisher; callbacks select loss/barrier boundaries.
type outboxFaultStore struct {
	*database.OutboxRepository
	afterClaim                                 func([]database.OutboxLease)
	beforeComplete, afterComplete, beforeRetry func()
}

func (s *outboxFaultStore) Claim(c context.Context, o database.OutboxClaimOptions) ([]database.OutboxLease, error) {
	r, e := s.OutboxRepository.Claim(c, o)
	if e == nil && s.afterClaim != nil {
		s.afterClaim(r)
	}
	return r, e
}
func (s *outboxFaultStore) Complete(c context.Context, l database.OutboxLease) (database.OutboxSettlement, error) {
	if s.beforeComplete != nil {
		s.beforeComplete()
	}
	r, e := s.OutboxRepository.Complete(c, l)
	if e == nil && s.afterComplete != nil {
		s.afterComplete()
	}
	return r, e
}
func (s *outboxFaultStore) Retry(c context.Context, l database.OutboxLease, o database.OutboxRetryOptions, r database.OutboxRetryReason) (database.OutboxSettlement, error) {
	if s.beforeRetry != nil {
		s.beforeRetry()
	}
	return s.OutboxRepository.Retry(c, l, o, r)
}

type outboxFaultPublisher func(context.Context, messaging.Notification) error

func (p outboxFaultPublisher) Publish(c context.Context, n messaging.Notification) error {
	return p(c, n)
}
func outboxAcceptanceOptions() workers.OutboxOptions {
	return workers.OutboxOptions{Claim: database.OutboxClaimOptions{Limit: 10, Lease: 2 * time.Second}, Retry: database.OutboxRetryOptions{Base: 20 * time.Millisecond, Max: time.Second}, ClaimTimeout: time.Second, PublishTimeout: 400 * time.Millisecond, SettlementTimeout: 400 * time.Millisecond, PollInterval: 10 * time.Millisecond}
}

func TestOutboxAcceptanceIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	f := newPersistenceLifecycleFixture(t, ctx)
	t.Logf("outbox acceptance run=%s", f.env.RunID())
	conf, err := f.env.Config()
	if err != nil {
		t.Fatal(err)
	}
	newRedis := func(t *testing.T, address string) *redis.Client {
		t.Helper()
		if address == "" {
			address = conf.REDIS.Addr
		}
		r := redis.NewClient(&redis.Options{Addr: address, Password: conf.REDIS.Password, DB: conf.REDIS.DB, MaxRetries: -1, MinIdleConns: 0, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, PoolTimeout: time.Second})
		t.Cleanup(func() { r.Close() })
		return r
	}
	rds := newRedis(t, "")
	publisher, _ := workers.NewRedisNotificationPublisher(rds)
	repo, _ := database.NewOutboxRepository(f.pool)
	commands, _ := database.NewMessageRepository(f.pool)
	var mu sync.Mutex
	seen := map[string]int{}
	routes := map[string]string{}
	var notificationErr bool
	ps := rds.Subscribe(ctx, "ichat:notify:v1:user:"+strconv.FormatUint(uint64(f.users[0].ID), 10), "ichat:notify:v1:user:"+strconv.FormatUint(uint64(f.users[1].ID), 10))
	for range 2 {
		if _, err := ps.Receive(ctx); err != nil {
			t.Fatal(err)
		}
	}
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		for m := range ps.Channel() {
			var h struct {
				EventID   string                    `json:"eventId"`
				EventType messaging.OutboxEventType `json:"eventType"`
			}
			e := json.Unmarshal([]byte(m.Payload), &h)
			mu.Lock()
			if e != nil || h.EventID == "" || strings.Contains(m.Payload, "content") || strings.Contains(m.Payload, "media") {
				notificationErr = true
			}
			if previous, ok := routes[h.EventID]; ok && previous != m.Channel {
				notificationErr = true
			}
			seen[h.EventID]++
			routes[h.EventID] = m.Channel
			mu.Unlock()
		}
	}()
	t.Cleanup(func() { ps.Close(); lifecycleAwait(t, monitorDone) })
	notified := func(id string) int { mu.Lock(); defer mu.Unlock(); return seen[id] }
	known := map[string]messaging.Command{}
	snapshots := map[string][]string{}
	immutable := func(t *testing.T, c messaging.Command) []string {
		t.Helper()
		var message string
		if err := f.pool.QueryRowContext(ctx, "SELECT CAST(JSON_OBJECT('body',HEX(content),'seq',CAST(seq AS CHAR),'stamp',DATE_FORMAT(server_created_at,'%Y-%m-%d %H:%i:%s.%f'),'client',client_message_id,'conversation',conversation_id,'sender',CAST(sender_id AS CHAR),'target',CAST(receiver_id AS CHAR),'media',media,'type',type) AS CHAR) FROM message WHERE id=?", c.CommandID).Scan(&message); err != nil {
			t.Fatal(err)
		}
		rows, err := f.pool.QueryContext(ctx, "SELECT CONCAT(event_id,'|',event_type,'|',command_id,'|',COALESCE(conversation_id,''),'|',COALESCE(message_id,''),'|',CAST(payload AS CHAR),'|',DATE_FORMAT(created_at,'%Y-%m-%d %H:%i:%s.%f')) FROM message_outbox WHERE command_id=? ORDER BY event_id", c.CommandID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var terminal string
		if err := f.pool.QueryRowContext(ctx, "SELECT CAST(JSON_OBJECT('command',command_id,'sender',CAST(sender_id AS CHAR),'client',client_message_id,'hash',payload_hash,'state',state,'reason',reason_code,'message',message_id,'conversation',conversation_id,'created',DATE_FORMAT(created_at,'%Y-%m-%d %H:%i:%s.%f'),'completed',DATE_FORMAT(completed_at,'%Y-%m-%d %H:%i:%s.%f')) AS CHAR) FROM message_command WHERE command_id=?", c.CommandID).Scan(&terminal); err != nil {
			t.Fatal(err)
		}
		r := []string{message, terminal}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			r = append(r, s)
		}
		if rows.Err() != nil || len(r) != 4 {
			t.Fatal("incomplete immutable set")
		}
		return r
	}
	remember := func(t *testing.T, c messaging.Command) {
		t.Helper()
		known[c.CommandID] = c
		f.checkDurableSet(t, c)
		snapshots[c.CommandID] = immutable(t, c)
	}
	seed := func(t *testing.T) messaging.Command {
		t.Helper()
		f.nextID++
		c, e := messaging.Normalize(uint64(f.users[0].ID), messaging.SendRequest{ClientMessageID: fmt.Sprintf("8fd7c2ef-ae26-4bb5-91a5-%012d", f.nextID), TargetType: messaging.Private, TargetID: strconv.FormatUint(uint64(f.users[1].ID), 10), Media: messaging.Text, Content: fmt.Sprintf("outbox:%s:%d:中🙂\x00", f.env.RunID(), f.nextID)})
		if e != nil {
			t.Fatal(e)
		}
		if _, e := commands.PersistCommand(ctx, c); e != nil {
			t.Fatal(e)
		}
		remember(t, c)
		return c
	}
	completeCount := func(c messaging.Command) int {
		var n int
		if err := f.pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM message_outbox WHERE command_id=? AND status='completed'", c.CommandID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	loop := func(t *testing.T, s workers.OutboxStore, p workers.NotificationPublisher, o workers.OutboxOptions) *workers.OutboxLoop {
		t.Helper()
		w, e := workers.NewOutboxLoop(s, p, o)
		if e != nil {
			t.Fatal(e)
		}
		return w
	}
	finish := func(t *testing.T, c messaging.Command) {
		t.Helper()
		w := loop(t, repo, publisher, outboxAcceptanceOptions())
		lifecycleEventually(t, func() bool {
			if _, e := w.RunBatch(ctx); e != nil {
				t.Fatal(e)
			}
			return completeCount(c) == 2
		})
		f.checkDurableSet(t, c)
		if !reflect.DeepEqual(snapshots[c.CommandID], immutable(t, c)) {
			t.Fatal("recovery changed canonical data")
		}
	}
	expire := func(t *testing.T, id string) {
		t.Helper()
		lifecycleEventually(t, func() bool {
			var expired bool
			if err := f.pool.QueryRowContext(ctx, "SELECT lease_until<=UTC_TIMESTAMP(6) FROM message_outbox WHERE event_id=? AND status='leased'", id).Scan(&expired); err != nil {
				t.Fatal(err)
			}
			return expired
		})
	}
	proxyRepo := func(t *testing.T) (*database.OutboxRepository, *quarantineSQLProxy) {
		t.Helper()
		p := startQuarantineSQLProxy(t, f.dsn.Addr)
		cfg := f.dsn.Clone()
		cfg.Addr = p.address
		cfg.InterpolateParams = true
		base, e := mysql.NewConnector(cfg)
		if e != nil {
			t.Fatal("fixture connector failed")
		}
		pool := sql.OpenDB(base)
		t.Cleanup(func() { pool.Close() })
		r, e := database.NewOutboxRepository(pool)
		if e != nil {
			t.Fatal(e)
		}
		return r, p
	}
	run := func(name string, fn func(*testing.T)) {
		if !t.Run(name, fn) {
			t.FailNow()
		}
	}
	run("claim_commit_ok_lost_never_publishes_uncertain_receipt", func(t *testing.T) {
		c := seed(t)
		r, p := proxyRepo(t)
		p.armed.Store(true)
		result, e := loop(t, r, publisher, outboxAcceptanceOptions()).RunBatch(ctx)
		if !errors.Is(e, database.ErrOutboxCommitUnknown) || result.Claimed != 0 || p.dropped.Load() != 1 {
			t.Fatal(result, e)
		}
		rows, e := f.pool.QueryContext(ctx, "SELECT event_id,status,attempts FROM message_outbox WHERE command_id=?", c.CommandID)
		if e != nil {
			t.Fatal(e)
		}
		var ids []string
		for rows.Next() {
			var id, status string
			var n int
			if e := rows.Scan(&id, &status, &n); e != nil {
				t.Fatal(e)
			}
			if status != "leased" || n != 1 || notified(id) != 0 {
				t.Fatal("uncertain claim was used")
			}
			ids = append(ids, id)
		}
		if e := rows.Err(); e != nil {
			t.Fatal(e)
		}
		rows.Close()
		if len(ids) != 2 {
			t.Fatal("claim did not commit entire batch")
		}
		expire(t, ids[0])
		finish(t, c)
	})
	run("completion_commit_ok_lost_does_not_republish_completed_event", func(t *testing.T) {
		c := seed(t)
		r, p := proxyRepo(t)
		store := &outboxFaultStore{OutboxRepository: r, beforeComplete: func() { p.armed.Store(true) }}
		o := outboxAcceptanceOptions()
		o.Claim.Limit = 1
		result, e := loop(t, store, publisher, o).RunBatch(ctx)
		if !errors.Is(e, database.ErrOutboxCommitUnknown) || result.Published != 1 || result.Completed != 0 || p.dropped.Load() != 1 || completeCount(c) != 1 {
			t.Fatal(result, e)
		}
		var id string
		if e := f.pool.QueryRowContext(ctx, "SELECT event_id FROM message_outbox WHERE command_id=? AND status='completed'", c.CommandID).Scan(&id); e != nil {
			t.Fatal(e)
		}
		finish(t, c)
		lifecycleEventually(t, func() bool { return notified(id) >= 1 })
		if notified(id) != 1 {
			t.Fatal("completed event was resent")
		}
	})
	for _, settlementLost := range []bool{true, false} {
		run(map[bool]string{true: "publish_and_retry_commit_replies_lost", false: "redis_publish_reply_lost_recovers_same_event"}[settlementLost], func(t *testing.T) {
			c := seed(t)
			rp := startOutboxRedisProxy(t, conf.REDIS.Addr)
			client := newRedis(t, rp.address)
			pub, _ := workers.NewRedisNotificationPublisher(client)
			rp.armed.Store(true)
			store := workers.OutboxStore(repo)
			var sp *quarantineSQLProxy
			if settlementLost {
				r, p := proxyRepo(t)
				sp = p
				store = &outboxFaultStore{OutboxRepository: r, beforeRetry: func() { p.armed.Store(true) }}
			}
			o := outboxAcceptanceOptions()
			o.Claim.Limit = 1
			result, e := loop(t, store, pub, o).RunBatch(ctx)
			if rp.dropped.Load() != 1 || result.Published != 0 {
				t.Fatal("real PUBLISH reply not dropped", result, e)
			}
			if settlementLost {
				if !errors.Is(e, database.ErrOutboxCommitUnknown) || sp.dropped.Load() != 1 || result.Retried != 0 {
					t.Fatal(result, e)
				}
			} else if e != nil || result.Retried != 1 {
				t.Fatal(result, e)
			}
			var id, reason string
			var attempts int
			if e := f.pool.QueryRowContext(ctx, "SELECT event_id,last_error,attempts FROM message_outbox WHERE command_id=? AND status='pending' AND last_error IS NOT NULL", c.CommandID).Scan(&id, &reason, &attempts); e != nil || reason != "NOTIFICATION_UNKNOWN" || attempts != 1 {
				t.Fatal(reason, attempts, e)
			}
			lifecycleEventually(t, func() bool { return notified(id) >= 1 })
			finish(t, c)
			lifecycleEventually(t, func() bool { return notified(id) >= 2 })
			if notified(id) != 2 {
				t.Fatal("unexpected retry count")
			}
		})
	}
	run("expired_owner_late_network_success_cannot_settle_new_owner", func(t *testing.T) {
		c := seed(t)
		captured := make(chan database.OutboxLease, 1)
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		defer unblock()
		store := &outboxFaultStore{OutboxRepository: repo, afterClaim: func(l []database.OutboxLease) {
			if len(l) != 1 {
				t.Error("expected single claim")
				return
			}
			captured <- l[0]
		}}
		pub := outboxFaultPublisher(func(c context.Context, n messaging.Notification) error {
			if e := publisher.Publish(c, n); e != nil {
				return e
			}
			close(entered)
			<-release
			return nil
		})
		o := outboxAcceptanceOptions()
		o.Claim.Limit, o.Claim.Lease = 1, time.Second
		done := make(chan struct{})
		var result workers.OutboxBatch
		var resultErr error
		go func() { result, resultErr = loop(t, store, pub, o).RunBatch(ctx); close(done) }()
		lifecycleAwait(t, entered)
		old := <-captured
		expire(t, old.EventID)
		leases, e := repo.Claim(ctx, database.OutboxClaimOptions{Limit: 10, Lease: 2 * time.Second})
		if e != nil {
			t.Fatal(e)
		}
		found := false
		for _, l := range leases {
			if l.EventID == old.EventID {
				found = true
				if l.Token == old.Token || l.Attempts != 2 {
					t.Fatal("expired claim not fenced")
				}
			}
			n, e := messaging.DecodeOutboxNotification(messaging.OutboxRecord{EventID: l.EventID, EventType: l.EventType, CommandID: l.CommandID, ConversationID: l.ConversationID, MessageID: l.MessageID, Payload: l.Payload})
			if e != nil {
				t.Fatal(e)
			}
			bounded, stop := context.WithTimeout(ctx, time.Second)
			e = publisher.Publish(bounded, n)
			stop()
			if e != nil {
				t.Fatal(e)
			}
			if _, e := repo.Complete(ctx, l); e != nil {
				t.Fatal(e)
			}
		}
		if !found {
			t.Fatal("expired owner was not reclaimed")
		}
		unblock()
		lifecycleAwait(t, done)
		if resultErr != nil || result.Completed != 0 || result.Deferred != 1 {
			t.Fatal(result, resultErr)
		}
		if _, e := repo.Complete(ctx, old); !errors.Is(e, database.ErrOutboxLeaseLost) {
			t.Fatal(e)
		}
		if _, e := repo.Retry(ctx, old, o.Retry, database.OutboxRetryPublishUnknown); !errors.Is(e, database.ErrOutboxLeaseLost) {
			t.Fatal(e)
		}
		finish(t, c)
		lifecycleEventually(t, func() bool { return notified(old.EventID) >= 2 })
	})
	for _, stage := range []string{"before_notify", "after_notify"} {
		run("kill_process_"+stage, func(t *testing.T) {
			c := seed(t)
			kill, lease, connection := outboxCrashWorker(t, f, conf.REDIS.Addr, conf.REDIS.Password, conf.REDIS.DB, stage)
			kill()
			lifecycleEventually(t, func() bool {
				var n int
				return f.pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.processlist WHERE ID=?", connection).Scan(&n) == nil && n == 0
			})
			if stage == "before_notify" && notified(lease) != 0 {
				t.Fatal("pre-notify child published")
			}
			if stage == "after_notify" {
				lifecycleEventually(t, func() bool { return notified(lease) >= 1 })
			}
			expire(t, lease)
			finish(t, c)
			if stage == "after_notify" {
				lifecycleEventually(t, func() bool { return notified(lease) >= 2 })
			}
		})
	}
	// Full-role cases below use actual persistence, audit and Outbox factories.
	mainACKs := 0
	queueEmpty := func() bool {
		for _, q := range []string{rabbitmq.PersistQueue, rabbitmq.QuarantineQueue} {
			var v struct {
				Ready   int               `json:"messages_ready"`
				Unacked int               `json:"messages_unacknowledged"`
				Stats   struct{ Ack int } `json:"message_stats"`
			}
			_, e := f.env.rabbitAPI(ctx, http.MethodGet, "/api/queues/"+url.PathEscape(f.broker.VHost)+"/"+q, nil, &v)
			if e != nil || v.Ready != 0 || v.Unacked != 0 || (q == rabbitmq.PersistQueue && v.Stats.Ack < mainACKs) || (q == rabbitmq.QuarantineQueue && v.Stats.Ack < 1) {
				return false
			}
		}
		return true
	}
	run("three_real_roles_pending_shutdown_and_fresh_generation", func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		defer unblock()
		var barrier atomic.Bool
		barrier.Store(true)
		store := &outboxFaultStore{OutboxRepository: repo, afterComplete: func() {
			if barrier.CompareAndSwap(true, false) {
				close(entered)
				<-release
			}
		}}
		o := application.DefaultRuntimeOptions()
		o.DrainTimeout, o.CloseTimeout = 100*time.Millisecond, 100*time.Millisecond
		r := startOutboxAcceptanceRuntime(t, f, f.pool, store, publisher, o)
		c, _ := f.publish(t)
		mainACKs++
		if e := f.control.PublishWithContext(ctx, rabbitmq.CommandsExchange, rabbitmq.SubmitRoutingKey, true, false, amqp.Publishing{ContentType: "application/json", DeliveryMode: 2, MessageId: "outbox-cross-invalid", CorrelationId: "outbox-cross-invalid-attempt", Body: []byte("{")}); e != nil {
			t.Fatal(e)
		}
		mainACKs++
		lifecycleAwait(t, entered)
		lifecycleEventually(t, queueEmpty)
		remember(t, c)
		r.stop()
		lifecycleAwait(t, r.done)
		if !errors.Is(r.err, workers.ErrOutboxServicePending) {
			t.Fatal("runtime did not report blocked owner", r.err)
		}
		if r.inputs.Health.RuntimeReadiness().Outbox != health.Stopping {
			t.Fatal("stopped runtime stayed ready")
		}
		select {
		case <-r.outbox.Done():
			t.Fatal("pending role lost ownership")
		default:
		}
		if e := f.pool.PingContext(ctx); e != nil {
			t.Fatal("shared SQL closed while callback active", e)
		}
		unblock()
		lifecycleAwait(t, r.outbox.Done())
		lifecycleAwait(t, r.persist.Done())
		lifecycleAwait(t, r.audit.Done())
		c2, _ := f.publish(t)
		mainACKs++
		fresh := startOutboxAcceptanceRuntime(t, f, f.pool, repo, publisher, application.DefaultRuntimeOptions())
		lifecycleEventually(t, func() bool { return f.count(t, c2) == 1 && completeCount(c) == 2 && completeCount(c2) == 2 })
		remember(t, c2)
		lifecycleEventually(t, queueEmpty)
		fresh.close(t)
	})
	run("persistence_commit_reply_lost_crosses_live_outbox_and_redelivery", func(t *testing.T) {
		p := startQuarantineSQLProxy(t, f.dsn.Addr)
		cfg := f.dsn.Clone()
		cfg.Addr = p.address
		cfg.InterpolateParams = true
		connector, e := mysql.NewConnector(cfg)
		if e != nil {
			t.Fatal("configure SQL proxy failed")
		}
		pool := sql.OpenDB(connector)
		defer pool.Close()
		r := startOutboxAcceptanceRuntime(t, f, pool, repo, publisher, application.DefaultRuntimeOptions())
		p.armed.Store(true)
		c, _ := f.publish(t)
		mainACKs++
		lifecycleEventually(t, func() bool { return p.dropped.Load() == 1 && f.count(t, c) == 1 && completeCount(c) == 2 })
		lifecycleEventually(t, queueEmpty)
		remember(t, c)
		r.close(t)
		if e := pool.PingContext(ctx); e != nil {
			t.Fatal("runtime closed borrowed persistence pool", e)
		}
	})
	for _, c := range known {
		f.checkDurableSet(t, c)
		if completeCount(c) != 2 || !reflect.DeepEqual(snapshots[c.CommandID], immutable(t, c)) {
			t.Fatal("canonical collection changed")
		}
		for _, kind := range []messaging.OutboxEventType{messaging.EventMessagePersisted, messaging.EventCommandResult} {
			id, _ := messaging.OutboxEventID(c.CommandID, kind)
			lifecycleEventually(t, func() bool { return notified(id) >= 1 })
			want := "ichat:notify:v1:user:" + c.TargetID
			if kind == messaging.EventCommandResult {
				want = "ichat:notify:v1:user:" + c.SenderID
			}
			mu.Lock()
			got := routes[id]
			mu.Unlock()
			if got != want {
				t.Fatal("notification route mismatch")
			}
		}
	}
	mu.Lock()
	bad, unique := notificationErr, len(seen)
	observations := 0
	for _, n := range seen {
		observations += n
	}
	mu.Unlock()
	if bad || unique != 2*len(known) || observations < 24 {
		t.Fatal("notification set differs", unique, len(known))
	}
	for _, tc := range []struct {
		table string
		want  int
	}{{"message", 10}, {"message_command", 10}, {"conversation", 1}, {"message_outbox", 20}, {"message_quarantine", 1}, {"message_delivery_cursor", 0}} {
		var n int
		if e := f.pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tc.table).Scan(&n); e != nil || n != tc.want {
			t.Fatal("final set mismatch", tc.table, n, e)
		}
	}
	var gaps int
	if e := f.pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM conversation c WHERE c.last_seq<>(SELECT COUNT(*) FROM message m WHERE m.conversation_id=c.id) OR c.last_seq<>(SELECT MAX(m.seq) FROM message m WHERE m.conversation_id=c.id)").Scan(&gaps); e != nil || gaps != 0 {
		t.Fatal("sequence gaps", e)
	}
	if e := migrations.ApplyReliable(ctx, f.pool); e != nil {
		t.Fatal("schema drift", e)
	}
	if e := rabbitmq.VerifyLocal(ctx, f.management, f.broker.VHost); e != nil {
		t.Fatal("topology drift", e)
	}
	var rawBody, document []byte
	var transport, state string
	if e := f.pool.QueryRowContext(ctx, "SELECT transport_id,raw_body,raw_headers,state FROM message_quarantine").Scan(&transport, &rawBody, &document, &state); e != nil {
		t.Fatal(e)
	}
	audit, e := rabbitmq.DecodeQuarantineAudit(document)
	if e != nil {
		t.Fatal("invalid quarantine audit", e)
	}
	source, e := audit.Envelope().Source()
	if e != nil || string(rawBody) != "{" || string(source.Body) != "{" || source.MessageID != "outbox-cross-invalid" || source.CorrelationID != "outbox-cross-invalid-attempt" || audit.Envelope().TransportID() != transport || state != "open" {
		t.Fatal("cross-role audit changed original delivery")
	}
	t.Logf("final: commands=%d messages=10 outbox=20 completed=20 audit=1 cursor=0 unique_notifications=%d notification_observations=%d main_acks=%d", len(known), unique, observations, mainACKs)
}

type outboxAcceptanceRuntime struct {
	stop    context.CancelFunc
	done    chan struct{}
	err     error
	outbox  *workers.OutboxService
	persist *workers.PersistenceSupervisor
	audit   *workers.QuarantineSupervisor
	inputs  application.RoleDependencies
}

func startOutboxAcceptanceRuntime(t *testing.T, f *persistenceLifecycleFixture, pool *sql.DB, store workers.OutboxStore, publisher workers.NotificationPublisher, o application.RuntimeOptions) *outboxAcceptanceRuntime {
	t.Helper()
	conf, e := f.env.Config()
	if e != nil {
		t.Fatal(e)
	}
	notificationClient := redis.NewClient(&redis.Options{Addr: conf.REDIS.Addr, Password: conf.REDIS.Password, DB: conf.REDIS.DB, MaxRetries: -1, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, PoolTimeout: time.Second})
	target, e := rabbitmq.DialQuarantinePublisher(f.ctx, f.broker.AMQPURL(), rabbitmq.DefaultSessionOptions())
	if e != nil {
		t.Fatal(e)
	}
	po := workers.DefaultPersistenceSupervisorOptions()
	po.Worker.QuarantinePublisher = target
	po.Worker.PollTimeout, po.Worker.ProbeInterval = 100*time.Millisecond, 100*time.Millisecond
	po.Worker.ProbeTimeout = 300 * time.Millisecond
	po.BaseDelay, po.MaxDelay = 50*time.Millisecond, 200*time.Millisecond
	qo := workers.DefaultQuarantineSupervisorOptions()
	qo.Worker.PollTimeout, qo.Worker.ProbeInterval = 100*time.Millisecond, 100*time.Millisecond
	qo.Worker.ProbeTimeout = 300 * time.Millisecond
	qo.BaseDelay, qo.MaxDelay = 50*time.Millisecond, 200*time.Millisecond
	pf, e := application.NewSupervisedPersistenceFactory(pool, po)
	if e != nil {
		t.Fatal(e)
	}
	qf, e := application.NewSupervisedQuarantineFactory(f.pool, qo)
	if e != nil {
		t.Fatal(e)
	}
	oo := workers.DefaultOutboxServiceOptions()
	oo.Loop = outboxAcceptanceOptions()
	oo.ProbeInterval, oo.ProbeTimeout = 100*time.Millisecond, 300*time.Millisecond
	of, e := application.NewOutboxSourceFactory(func(context.Context) (workers.OutboxResources, error) {
		return workers.OutboxResources{Store: store, Publisher: publisher, ProbeStorage: f.pool.PingContext, ProbeNotification: func(c context.Context) error { return notificationClient.Ping(c).Err() }}, nil
	}, oo)
	if e != nil {
		t.Fatal(e)
	}
	r := &outboxAcceptanceRuntime{done: make(chan struct{})}
	captured := make(chan struct{})
	factories := application.RuntimeFactories{Persistence: func(c context.Context, d application.RoleDependencies) (application.RuntimeService, error) {
		s, e := pf(c, d)
		if e == nil {
			r.persist = s.(*workers.PersistenceSupervisor)
		}
		return s, e
	}, Quarantine: func(c context.Context, d application.RoleDependencies) (application.RuntimeService, error) {
		s, e := qf(c, d)
		if e == nil {
			r.audit = s.(*workers.QuarantineSupervisor)
		}
		return s, e
	}, Outbox: func(c context.Context, d application.RoleDependencies) (application.RuntimeService, error) {
		s, e := of(c, d)
		if e == nil {
			r.outbox, r.inputs = s.(*workers.OutboxService), d
			close(captured)
		}
		return s, e
	}}
	c, stop := context.WithCancel(f.ctx)
	r.stop = stop
	go func() { r.err = application.RunRuntime(c, persistenceRuntimeConfig(f), factories, o); close(r.done) }()
	t.Cleanup(func() {
		stop()
		lifecycleAwait(t, r.done)
		if r.outbox != nil {
			lifecycleAwait(t, r.outbox.Done())
		}
		if r.persist != nil {
			lifecycleAwait(t, r.persist.Done())
		}
		if r.audit != nil {
			lifecycleAwait(t, r.audit.Done())
		}
		target.Close(context.Background())
		notificationClient.Close()
	})
	select {
	case <-captured:
	case <-r.done:
		t.Fatal("real roles construction failed", r.err)
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	lifecycleEventually(t, func() bool {
		status := r.inputs.Health.RuntimeReadiness()
		return (status.Consumers == health.Ready && status.Outbox == health.Ready) || persistenceSupervisorDone(r.done)
	})
	if persistenceSupervisorDone(r.done) {
		t.Fatal("real roles activation failed", r.err)
	}
	return r
}
func (r *outboxAcceptanceRuntime) close(t *testing.T) {
	t.Helper()
	r.stop()
	lifecycleAwait(t, r.done)
	if r.err != nil {
		t.Fatal(r.err)
	}
	lifecycleAwait(t, r.outbox.Done())
	lifecycleAwait(t, r.persist.Done())
	lifecycleAwait(t, r.audit.Done())
}
