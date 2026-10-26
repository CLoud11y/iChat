//go:build integration

package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"iChat/messaging"
	"iChat/migrations"
	"iChat/tests/testenv"
	"iChat/utils"
	"iChat/workers"
)

type outboxLifecyclePublisher func(context.Context, messaging.Notification) error

func (p outboxLifecyclePublisher) Publish(c context.Context, n messaging.Notification) error {
	return p(c, n)
}

func TestOutboxLifecycleIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	env, err := testenv.Start(ctx, filepath.Join("..", "docker-compose.test.yml"))
	if env != nil {
		t.Cleanup(func() {
			c, stop := context.WithTimeout(context.Background(), time.Minute)
			defer stop()
			if err := env.Close(c); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("outbox lifecycle fixture run=%s", env.RunID())
	conf, err := env.Config()
	if err != nil {
		t.Fatal(err)
	}
	db, err := utils.OpenMySQL(conf)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var schema string
	if err := pool.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&schema); err != nil || schema != "ichat_test_"+env.RunID() {
		t.Fatal("refusing writes outside owned schema")
	}
	if err := migrations.ApplyReliable(ctx, pool); err != nil {
		t.Fatal(err)
	}
	client, err := utils.OpenRedis(ctx, conf)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	o := workers.DefaultOutboxServiceOptions()
	o.Loop.PollInterval = 5 * time.Millisecond
	o.Loop.PublishTimeout = 5 * time.Second
	o.ProbeInterval = 20 * time.Millisecond
	start := func(t *testing.T, s RuntimeService) {
		t.Helper()
		c, stop := context.WithTimeout(ctx, 3*time.Second)
		defer stop()
		if err := s.Start(c, ctx); err != nil {
			t.Fatal(err)
		}
	}
	join := func(t *testing.T, s RuntimeService) {
		t.Helper()
		select {
		case <-s.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("service did not join")
		}
	}
	assertClosed := func(t *testing.T, r workers.OutboxResources) {
		t.Helper()
		if err := r.ProbeStorage(ctx); err == nil || err.Error() != "sql: database is closed" {
			t.Fatal("owned SQL not closed", err)
		}
		if err := r.ProbeNotification(ctx); !errors.Is(err, redis.ErrClosed) {
			t.Fatal("owned Redis not closed", err)
		}
	}
	t.Run("configured_factory_and_borrowed_isolation", func(t *testing.T) {
		f, err := NewConfiguredOutboxFactory(conf, o)
		if err != nil {
			t.Fatal(err)
		}
		s, err := f(ctx, RoleDependencies{})
		if err != nil || s.(RuntimeOutboxReadiness).OutboxReady() {
			t.Fatal(err)
		}
		start(t, s)
		if !s.(RuntimeOutboxReadiness).OutboxReady() {
			t.Fatal("production role not ready")
		}
		s.StopAdmission()
		if err := s.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		join(t, s)
		borrowed, _ := NewOutboxFactory(pool, client, o)
		b, err := borrowed(ctx, RoleDependencies{})
		if err != nil {
			t.Fatal(err)
		}
		start(t, b)
		if err := b.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		if err := pool.PingContext(ctx); err != nil {
			t.Fatal("borrowed SQL closed", err)
		}
		if err := client.Ping(ctx).Err(); err != nil {
			t.Fatal("borrowed Redis closed", err)
		}
	})
	t.Run("owned_resources_close_after_idle_drain", func(t *testing.T) {
		real := configuredOutboxSource(conf, o)
		captured := make(chan workers.OutboxResources, 1)
		f, _ := NewOutboxSourceFactory(func(c context.Context) (workers.OutboxResources, error) {
			r, err := real(c)
			captured <- r
			return r, err
		}, o)
		s, _ := f(ctx, RoleDependencies{})
		start(t, s)
		r := <-captured
		if err := s.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		assertClosed(t, r)
	})
	for _, mode := range []string{"sql_start_failure", "redis_start_failure"} {
		t.Run(mode, func(t *testing.T) {
			bad := conf
			if mode == "sql_start_failure" {
				bad.MYSQL.DbName += "_missing"
			} else {
				bad.REDIS.Password = "incorrect-fixture-password"
			}
			real := configuredOutboxSource(bad, o)
			captured := make(chan workers.OutboxResources, 1)
			f, _ := NewOutboxSourceFactory(func(c context.Context) (workers.OutboxResources, error) {
				r, err := real(c)
				captured <- r
				return r, err
			}, o)
			s, _ := f(ctx, RoleDependencies{})
			c, stop := context.WithTimeout(ctx, 2*time.Second)
			err := s.Start(c, ctx)
			stop()
			if !errors.Is(err, workers.ErrOutboxServiceStart) {
				t.Fatal("dependency startup unexpectedly succeeded", err)
			}
			join(t, s)
			assertClosed(t, <-captured)
		})
	}
	serial := 0
	for _, force := range []bool{false, true} {
		t.Run(map[bool]string{false: "graceful_admitted_publication", true: "forced_pending_keeps_resources"}[force], func(t *testing.T) {
			serial++
			c, _ := messaging.Normalize(2, messaging.SendRequest{ClientMessageID: fmt.Sprintf("12345678-1234-4234-8234-%012d", serial), TargetType: messaging.Private, TargetID: "10", Media: messaging.Text, Content: "fixture-content"})
			id, _ := messaging.OutboxEventID(c.CommandID, messaging.EventMessagePersisted)
			body, _ := json.Marshal(messaging.MessagePersistedEvent{SchemaVersion: 1, EventID: id, CommandID: c.CommandID, ConversationID: c.ConversationID, MessageID: c.CommandID, Seq: 1, SenderID: c.SenderID, TargetID: c.TargetID, TargetType: c.TargetType})
			if _, err := pool.ExecContext(ctx, "INSERT INTO message_outbox(event_id,event_type,command_id,conversation_id,message_id,payload,status,available_at,created_at) VALUES (?,'message.persisted',?,?,?,?,'pending',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6))", id, c.CommandID, c.ConversationID, c.CommandID, string(body)); err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			var closeCount atomic.Int32
			var resource workers.OutboxResources
			real := configuredOutboxSource(conf, o)
			f, _ := NewOutboxSourceFactory(func(c context.Context) (workers.OutboxResources, error) {
				r, err := real(c)
				resource = r
				originalPublisher := r.Publisher
				r.Publisher = outboxLifecyclePublisher(func(c context.Context, n messaging.Notification) error {
					if err := originalPublisher.Publish(c, n); err != nil {
						return err
					}
					close(entered)
					<-release
					return c.Err()
				})
				closeResources := r.Close
				r.Close = func() error { closeCount.Add(1); return closeResources() }
				return r, err
			}, o)
			s, _ := f(ctx, RoleDependencies{})
			start(t, s)
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("publication was not admitted")
			}
			s.StopAdmission()
			if s.(RuntimeOutboxReadiness).OutboxReady() {
				t.Fatal("stopping role still ready")
			}
			observer, stop := context.WithTimeout(ctx, 10*time.Millisecond)
			var err error
			if force {
				err = s.Close(observer)
			} else {
				err = s.Drain(observer)
			}
			stop()
			if !errors.Is(err, workers.ErrOutboxServicePending) || closeCount.Load() != 0 {
				t.Fatal("pending observer closed active resources", err)
			}
			if err := resource.ProbeStorage(ctx); err != nil {
				t.Fatal("SQL released before callback joined", err)
			}
			if err := resource.ProbeNotification(ctx); err != nil {
				t.Fatal("Redis released before callback joined", err)
			}
			var status string
			if err := pool.QueryRowContext(ctx, "SELECT status FROM message_outbox WHERE event_id=?", id).Scan(&status); err != nil || status != "leased" {
				t.Fatal(status, err)
			}
			close(release)
			join(t, s)
			if err := pool.QueryRowContext(ctx, "SELECT status FROM message_outbox WHERE event_id=?", id).Scan(&status); err != nil {
				t.Fatal(err)
			}
			want := "completed"
			if force {
				want = "leased"
			}
			if status != want || closeCount.Load() != 1 {
				t.Fatal("wrong settlement/ownership", status, closeCount.Load())
			}
			assertClosed(t, resource)
		})
	}
	for _, table := range []string{"message", "message_command", "conversation", "message_delivery_cursor", "message_quarantine"} {
		var n int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil || n != 0 {
			t.Fatal("lifecycle changed business tables", table, n, err)
		}
	}
	var total, completed, leased int
	if err := pool.QueryRowContext(ctx, "SELECT COUNT(*),SUM(status='completed'),SUM(status='leased') FROM message_outbox").Scan(&total, &completed, &leased); err != nil || total != 2 || completed != 1 || leased != 1 {
		t.Fatal(total, completed, leased, err)
	}
	if err := migrations.ApplyReliable(ctx, pool); err != nil {
		t.Fatal("schema drift", err)
	}
}
