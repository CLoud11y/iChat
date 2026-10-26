//go:build integration

package testenv

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	mysql "github.com/go-sql-driver/mysql"
	"iChat/database"
	"iChat/messaging"
	"iChat/workers"
)

type outboxChildConfig struct {
	DSN, RunID, RedisAddress, RedisPassword, Stage string
	RedisDB                                        int
}
type outboxChildBarrier struct {
	Connection uint64
	EventID    string
}

func outboxCrashWorker(t *testing.T, f *persistenceLifecycleFixture, redisAddress, redisPassword string, redisDB int, stage string) (func(), string, uint64) {
	t.Helper()
	raw, e := json.Marshal(outboxChildConfig{DSN: f.dsn.FormatDSN(), RunID: f.env.RunID(), RedisAddress: redisAddress, RedisPassword: redisPassword, RedisDB: redisDB, Stage: stage})
	if e != nil {
		t.Fatal("encode child settings failed")
	}
	program, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command(program, "-test.run=^TestOutboxCrashHelper$", "-test.timeout=45s")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(entry), "ICHAT_OUTBOX_CHILD=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "ICHAT_OUTBOX_CHILD="+string(raw))
	stdout, e := cmd.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	done, barriers := make(chan struct{}), make(chan outboxChildBarrier, 1)
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(done) }()
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if strings.HasPrefix(scanner.Text(), "outbox-crash:") {
				var b outboxChildBarrier
				if json.Unmarshal([]byte(strings.TrimPrefix(scanner.Text(), "outbox-crash:")), &b) == nil {
					barriers <- b
					return
				}
			}
		}
	}()
	var once sync.Once
	kill := func() {
		once.Do(func() {
			cmd.Process.Kill()
			select {
			case <-done:
				if waitErr == nil {
					t.Error("child exited instead of being killed")
				}
			case <-time.After(5 * time.Second):
				t.Error("child process did not join")
			}
		})
	}
	t.Cleanup(kill)
	select {
	case b := <-barriers:
		if b.Connection == 0 || len(b.EventID) != 64 {
			t.Fatal("invalid child barrier")
		}
		return kill, b.EventID, b.Connection
	case <-done:
		t.Fatal("Outbox child exited before barrier", waitErr)
	case <-time.After(20 * time.Second):
		t.Fatal("Outbox child did not reach barrier")
	}
	return kill, "", 0
}

func TestOutboxCrashHelper(t *testing.T) {
	raw := os.Getenv("ICHAT_OUTBOX_CHILD")
	if raw == "" {
		t.Skip("private subprocess helper")
	}
	var conf outboxChildConfig
	if json.Unmarshal([]byte(raw), &conf) != nil || (conf.Stage != "before_notify" && conf.Stage != "after_notify") || len(conf.RunID) != 32 {
		t.Fatal("invalid child settings")
	}
	settings, e := mysql.ParseDSN(conf.DSN)
	if e != nil || settings.DBName != "ichat_test_"+conf.RunID {
		t.Fatal("refusing child SQL outside owned schema")
	}
	connector, e := mysql.NewConnector(settings)
	if e != nil {
		t.Fatal("child connector failed")
	}
	pool := sql.OpenDB(connector)
	pool.SetMaxOpenConns(1)
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var connection uint64
	if e := pool.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&connection); e != nil {
		t.Fatal("child connection failed")
	}
	client := redis.NewClient(&redis.Options{Addr: conf.RedisAddress, Password: conf.RedisPassword, DB: conf.RedisDB, MaxRetries: -1, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, PoolTimeout: time.Second})
	defer client.Close()
	repo, _ := database.NewOutboxRepository(pool)
	pub, _ := workers.NewRedisNotificationPublisher(client)
	barrier := func(id string) {
		body, _ := json.Marshal(outboxChildBarrier{Connection: connection, EventID: id})
		fmt.Println("outbox-crash:" + string(body))
		<-ctx.Done()
	}
	store := &outboxFaultStore{OutboxRepository: repo, afterClaim: func(l []database.OutboxLease) {
		if len(l) > 0 && conf.Stage == "before_notify" {
			barrier(l[0].EventID)
		}
	}}
	publisher := outboxFaultPublisher(func(c context.Context, n messaging.Notification) error {
		if e := pub.Publish(c, n); e != nil {
			return e
		}
		var h struct {
			EventID string `json:"eventId"`
		}
		if json.Unmarshal(n.Body(), &h) != nil {
			return workers.ErrOutboxWorkerInput
		}
		if conf.Stage == "after_notify" {
			barrier(h.EventID)
		}
		return c.Err()
	})
	o := workers.DefaultOutboxServiceOptions()
	o.Loop = outboxAcceptanceOptions()
	o.Loop.Claim.Limit = 1
	s, e := workers.NewOutboxService(func(context.Context) (workers.OutboxResources, error) {
		return workers.OutboxResources{Store: store, Publisher: publisher, ProbeStorage: pool.PingContext, ProbeNotification: func(c context.Context) error { return client.Ping(c).Err() }}, nil
	}, o)
	if e != nil {
		t.Fatal("child service failed")
	}
	if e := s.Start(ctx, ctx); e != nil {
		t.Fatal("child activation failed")
	}
	select {
	case <-s.Done():
		t.Fatal("child stopped before being killed")
	case <-ctx.Done():
		t.Fatal("child must be killed at barrier")
	}
}
