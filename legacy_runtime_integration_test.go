//go:build integration

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"iChat/config"
	"iChat/database"
	"iChat/models"
	scheduledtasks "iChat/scheduledTasks"
	"iChat/tests/testenv"
	"iChat/utils"

	"github.com/go-redis/redis/v8"
	"github.com/gorilla/websocket"
	"gorm.io/gorm"
)

type runtimeCommitBarrier struct {
	scheduledtasks.MessageStore
	committed, proceed chan struct{}
	once               sync.Once
	ctx                context.Context // published by committed
}

func (s *runtimeCommitBarrier) ProcessDirtyMsgs(ctx context.Context, raw []string) (int, error) {
	n, err := s.MessageStore.ProcessDirtyMsgs(ctx, raw)
	if n > 0 && err == nil {
		s.ctx = ctx
		s.once.Do(func() { close(s.committed) })
		<-s.proceed
	}
	return n, err
}

type realLegacyRuntime struct {
	d       startupDependencies
	c       config.Config
	o       legacyRuntimeOptions
	pool    *sql.DB
	rds     *redis.Client
	http    *legacyHTTPServer
	monitor legacyHealthMonitor
	address chan string
}

func realLegacyRuntimeDependencies(t *testing.T, f *testenv.LegacyFixture) *realLegacyRuntime {
	t.Helper()
	c, err := f.Env.Config()
	if err != nil {
		t.Fatal(err)
	}
	c.JWT.Key = "fixture-jwt-" + f.Env.RunID()
	c.JWT.TokenHourLifeSpan = 1
	c.LOG.AccessEnabled = false
	c.LEGACY_WORKER.Enabled = true
	r := &realLegacyRuntime{d: defaultStartupDependencies(), c: c, o: defaultLegacyRuntimeOptions(), address: make(chan string, 1)}
	r.o.Address = "127.0.0.1:0"
	r.d.loadConfig = func() (config.Config, error) { return r.c, nil }
	open := r.d.openMySQL
	r.d.openMySQL = func(c config.Config) (*gorm.DB, error) {
		db, err := open(c)
		if db != nil {
			r.pool, _ = db.DB()
		}
		return db, err
	}
	redisOpen := r.d.openRedis
	r.d.openRedis = func(ctx context.Context, c config.Config) (*redis.Client, error) {
		var err error
		r.rds, err = redisOpen(ctx, c)
		return r.rds, err
	}
	health := r.d.managed.Health
	r.d.managed.Health = func(p *sql.DB) legacyHealthMonitor { r.monitor = health(p); return r.monitor }
	build := r.d.managed.HTTP
	r.d.managed.HTTP = func(i legacyHTTPInputs) (legacyHTTPService, error) {
		h, err := build(i)
		if h != nil {
			r.http, _ = h.(*legacyHTTPServer)
		}
		return h, err
	}
	listen := r.d.managed.Listen
	r.d.managed.Listen = func(ctx context.Context, n, a string) (net.Listener, error) {
		l, err := listen(ctx, n, a)
		if err == nil {
			r.address <- l.Addr().String()
		}
		return l, err
	}
	return r
}

func TestLegacyRuntimeIntegration(t *testing.T) {
	f := testenv.NewLegacyFixture(t)
	for _, forced := range []bool{false, true} {
		t.Run(map[bool]string{false: "signal_graceful", true: "forced_pending_after_commit"}[forced], func(t *testing.T) {
			r := realLegacyRuntimeDependencies(t, f)
			r.o.Worker.Schedule = "@every 1s"
			if forced {
				r.o.DrainTimeout = 40 * time.Millisecond
				r.o.CloseTimeout = 40 * time.Millisecond
			}
			msg := f.Message(f.Users[0].ID, f.Users[1].ID, models.PrivateType, models.TextMedia, "runtime lifecycle")
			raw, err := json.Marshal(msg)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Redis.RPush(context.Background(), "dirty:msgs", string(raw)).Err(); err != nil {
				t.Fatal(err)
			}
			proceed := make(chan struct{})
			release := sync.OnceFunc(func() { close(proceed) })
			defer release()
			var barrier *runtimeCommitBarrier
			startWorker := r.d.managed.Worker
			r.d.managed.Worker = func(ctx context.Context, s scheduledtasks.MessageStore, rds *redis.Client, o legacyWorkerOptions) (legacyWorkerService, error) {
				barrier = &runtimeCommitBarrier{MessageStore: s, committed: make(chan struct{}), proceed: proceed}
				return startWorker(ctx, barrier, rds, o)
			}
			before := legacyState()
			process, cancel := context.WithCancel(context.Background())
			defer cancel()
			var l *legacyLifecycle
			setupComplete := make(chan struct{})
			entry := defaultProcessDependencies()
			entry.legacy = r.d
			entry.legacyOptions = r.o
			// Exercise the actual dispatcher and runner while retaining the handle
			// so this embedding test can join eventual cleanup after a pending result.
			entry.runLegacy = func(ctx context.Context, c config.Config, d startupDependencies, o legacyRuntimeOptions) error {
				var err error
				l, err = startLegacyLifecycle(ctx, c, d, o)
				close(setupComplete)
				if l == nil {
					return err
				}
				return l.Result(context.Background())
			}
			unregistered := make(chan struct{})
			entry.notifyContext = func(context.Context, ...os.Signal) (context.Context, context.CancelFunc) {
				return process, func() { close(unregistered); cancel() }
			}
			t.Cleanup(func() {
				release()
				cancel()
				select {
				case <-setupComplete:
				case <-time.After(5 * time.Second):
					t.Error("startup did not finish")
					return
				}
				if l != nil {
					ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
					defer stop()
					_ = l.Wait(ctx)
					select {
					case <-l.Done():
					default:
						t.Error("runtime did not complete cleanup")
					}
				}
			})
			result := make(chan error, 1)
			go func() { result <- runProcess(entry) }()
			var address string
			select {
			case address = <-r.address:
			case <-time.After(5 * time.Second):
				t.Fatal("listener not constructed")
			}
			legacyHTTPAwait(t, setupComplete)
			client := &http.Client{Timeout: 3 * time.Second}
			resp, err := client.Get("http://" + address + "/health/live")
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatal("liveness failed", resp.StatusCode)
			}
			tokens, err := utils.NewTokenService(r.c, utils.NewRedisTokenBlacklist(f.Redis))
			if err != nil {
				t.Fatal(err)
			}
			token, err := tokens.GenerateToken(f.Users[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			conn, _, err := websocket.DefaultDialer.Dial("ws://"+address+"/auth/getws", http.Header{"Authorization": []string{"Bearer " + token}})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			legacyHTTPAwait(t, barrier.committed)
			cancel() // simulates the NotifyContext cancellation, not an OS signal
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, _, err := conn.ReadMessage(); err == nil {
				t.Fatal("signal did not terminate WebSocket")
			}
			if r.pool.Ping() != nil || r.rds.Ping(context.Background()).Err() != nil {
				t.Fatal("storage closed beneath in-flight commit callback")
			}
			if candidate, err := database.AcquireLegacyWorker(context.Background(), f.Redis); !errors.Is(err, database.ErrLegacyWorkerBusy) {
				if candidate != nil {
					_ = candidate.Release(context.Background())
				}
				t.Fatal("registration released before job joined", err)
			}
			if !forced && barrier.ctx.Err() != nil {
				t.Fatal("process cancellation cancelled the drain job")
			}
			if forced {
				select {
				case err := <-result:
					if !errors.Is(err, errLegacyRuntimePending) || !errors.Is(err, context.DeadlineExceeded) {
						t.Fatal("pending shutdown not reported", err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("bounded result did not return")
				}
				if reflect.DeepEqual(legacyState(), before) || r.pool.Ping() != nil || r.rds.Ping(context.Background()).Err() != nil {
					t.Fatal("pending owner released globals or clients")
				}
				select {
				case <-l.Done():
					t.Fatal("pending job reported complete")
				default:
				}
			}
			release()
			if !forced {
				select {
				case err := <-result:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("graceful shutdown did not return")
				}
			}
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			final := l.Wait(cleanup)
			if forced && !errors.Is(final, errLegacyRuntimePending) || !forced && final != nil {
				t.Fatal("final lifecycle result", final)
			}
			legacyHTTPAwait(t, unregistered)
			if !reflect.DeepEqual(legacyState(), before) || !errors.Is(r.rds.Ping(context.Background()).Err(), redis.ErrClosed) || r.pool.Ping() == nil {
				t.Fatal("final cleanup did not restore/close dependencies")
			}
			legacyHTTPAwait(t, r.http.RequestsDone())
			legacyHTTPAwait(t, r.monitor.Done())
			pending, err := f.Redis.LRange(context.Background(), "dirty:msgs", 0, -1).Result()
			if err != nil {
				t.Fatal(err)
			}
			if forced && (len(pending) != 1 || pending[0] != string(raw)) || !forced && len(pending) != 0 {
				t.Fatal("shutdown violated the acknowledgement boundary")
			}
			next, err := database.AcquireLegacyWorker(context.Background(), f.Redis)
			if err != nil {
				t.Fatal(err)
			}
			defer next.Release(context.Background())
			if err := scheduledtasks.SyncMsg(context.Background(), scheduledtasks.OwnedMessages{MessageStore: database.NewMessageManager(f.DB, f.Redis), Owner: next}); err != nil {
				t.Fatal(err)
			}
			var count int64
			if err := f.DB.Model(&models.Message{}).Where("id = ?", msg.Id).Count(&count).Error; err != nil || count != 1 || f.Redis.LLen(context.Background(), "dirty:msgs").Val() != 0 {
				t.Fatal("real committed row did not replay idempotently", err)
			}
		})
	}
	t.Run("busy_startup_rolls_back", func(t *testing.T) {
		r := realLegacyRuntimeDependencies(t, f)
		owner, err := database.AcquireLegacyWorker(context.Background(), f.Redis)
		if err != nil {
			t.Fatal(err)
		}
		defer owner.Release(context.Background())
		before := legacyState()
		l, err := startLegacyLifecycle(context.Background(), r.c, r.d, r.o)
		if l == nil || !errors.Is(err, database.ErrLegacyWorkerBusy) {
			t.Fatal("busy owner did not reject startup", err)
		}
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := l.Wait(cleanup); !errors.Is(err, database.ErrLegacyWorkerBusy) {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(legacyState(), before) || owner.Check(context.Background()) != nil || r.pool.Ping() == nil || !errors.Is(r.rds.Ping(context.Background()).Err(), redis.ErrClosed) {
			t.Fatal("partial startup released another owner or leaked dependencies")
		}
	})
	t.Run("mysql_offline_still_serves_liveness", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := f.Env.SetMySQLRunning(ctx, false); err != nil {
			t.Fatal(err)
		}
		r := realLegacyRuntimeDependencies(t, f)
		process, stop := context.WithCancel(context.Background())
		defer stop()
		l, err := startLegacyLifecycle(process, r.c, r.d, r.o)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			stop()
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := l.Wait(cleanup); err != nil {
				t.Error(err)
			}
		}()
		address := <-r.address
		client := &http.Client{Timeout: 3 * time.Second}
		for path, want := range map[string]int{"/health/live": http.StatusOK, "/health/ready/storage": http.StatusServiceUnavailable} {
			resp, err := client.Get("http://" + address + path)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != want {
				t.Fatalf("%s status=%d body=%s", path, resp.StatusCode, strings.TrimSpace(string(body)))
			}
		}
	})
}
