//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"iChat/database"
	"iChat/models"
	scheduledtasks "iChat/scheduledTasks"
	"iChat/tests/testenv"
)

type legacyWorkerCommitBarrier struct {
	scheduledtasks.MessageStore
	committed chan struct{}
	proceed   <-chan struct{}
	once      sync.Once
}

func (s *legacyWorkerCommitBarrier) ProcessDirtyMsgs(ctx context.Context, raw []string) (int, error) {
	n, err := s.MessageStore.ProcessDirtyMsgs(ctx, raw)
	if n > 0 && err == nil {
		s.once.Do(func() { close(s.committed) })
		<-s.proceed // DB has really committed; the worker callback is still owned.
	}
	return n, err
}

type observedLegacyWorkerOwner struct {
	legacyWorkerOwnership
	releases atomic.Int32
}

func (o *observedLegacyWorkerOwner) Release(ctx context.Context) error {
	o.releases.Add(1)
	return o.legacyWorkerOwnership.Release(ctx)
}

func TestLegacyWorkerLifecycleIntegration(t *testing.T) {
	f := testenv.NewLegacyFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	manager := database.NewMessageManager(f.DB, f.Redis)
	assertBusy := func() {
		t.Helper()
		owner, err := database.AcquireLegacyWorker(ctx, f.Redis)
		if owner != nil {
			_ = owner.Release(ctx)
		}
		if !errors.Is(err, database.ErrLegacyWorkerBusy) {
			t.Fatal("worker released its registration before its job joined", err)
		}
	}
	for _, mode := range []string{"graceful", "forced_after_commit"} {
		t.Run(mode, func(t *testing.T) {
			msg := f.Message(f.Users[0].ID, f.Users[1].ID, models.PrivateType, models.TextMedia, mode)
			raw, err := json.Marshal(msg)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Redis.RPush(ctx, "dirty:msgs", string(raw)).Err(); err != nil {
				t.Fatal(err)
			}
			proceed := make(chan struct{})
			release := sync.OnceFunc(func() { close(proceed) })
			defer release()
			store := &legacyWorkerCommitBarrier{MessageStore: manager, committed: make(chan struct{}), proceed: proceed}
			options := defaultLegacyWorkerOptions()
			options.Schedule = "@every 1s"
			w, err := startLegacyWorker(ctx, store, f.Redis, options)
			if w != nil {
				t.Cleanup(func() {
					release()
					cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
					defer stop()
					if err := w.Close(cleanup); err != nil {
						t.Error("worker did not clean up", err)
					}
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			legacyHTTPAwait(t, store.committed)
			assertBusy()
			if ttl, err := f.Redis.TTL(ctx, database.LegacyWorkerLockKey).Result(); err != nil || ttl != -1 {
				t.Fatal("worker registration acquired a TTL", err)
			}
			var count int64
			if err := f.DB.WithContext(ctx).Model(&models.Message{}).Where("id = ?", msg.Id).Count(&count).Error; err != nil || count != 1 {
				t.Fatal("commit barrier did not follow a durable message", err)
			}
			w.StopAdmission()
			if mode == "forced_after_commit" {
				expired, stop := context.WithCancel(context.Background())
				stop()
				if err := w.Close(expired); !errors.Is(err, context.Canceled) {
					t.Fatal("forced observer did not return cancellation", err)
				}
				assertBusy() // cancellation is not permission to release the token
			}
			release()
			if err := w.Drain(ctx); err != nil {
				t.Fatal(err)
			}
			if mode == "graceful" {
				assertBusy() // Drain alone never releases ownership
			}
			if err := w.Close(ctx); err != nil {
				t.Fatal(err)
			}
			pending, err := f.Redis.LRange(ctx, "dirty:msgs", 0, -1).Result()
			if err != nil {
				t.Fatal(err)
			}
			if mode == "graceful" && len(pending) != 0 || mode == "forced_after_commit" && (len(pending) != 1 || pending[0] != string(raw)) {
				t.Fatal("graceful/forced acknowledgement boundary lost the raw record")
			}
			// A fresh owner replays the real committed-but-not-acknowledged row.
			next, err := database.AcquireLegacyWorker(ctx, f.Redis)
			if err != nil {
				t.Fatal(err)
			}
			defer next.Release(ctx)
			if err := scheduledtasks.SyncMsg(ctx, scheduledtasks.OwnedMessages{MessageStore: manager, Owner: next}); err != nil {
				t.Fatal(err)
			}
			if err := f.DB.WithContext(ctx).Model(&models.Message{}).Where("id = ?", msg.Id).Count(&count).Error; err != nil || count != 1 || f.Redis.LLen(ctx, "dirty:msgs").Val() != 0 {
				t.Fatal("fresh owner replay duplicated a committed row or failed to trim", err)
			}
		})
	}
	t.Run("stale_release_preserves_successor", func(t *testing.T) {
		var owner *observedLegacyWorkerOwner
		w, err := startOwnedLegacyWorker(ctx, manager, defaultLegacyWorkerOptions(), func(setup context.Context) (legacyWorkerOwnership, error) {
			acquired, err := database.AcquireLegacyWorker(setup, f.Redis)
			if err != nil {
				return nil, err
			}
			owner = &observedLegacyWorkerOwner{legacyWorkerOwnership: acquired}
			return owner, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close(ctx)
		if err := w.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		successor := "fixture-successor-" + f.Env.RunID()
		if err := f.Redis.Set(ctx, database.LegacyWorkerLockKey, successor, 0).Err(); err != nil {
			t.Fatal(err)
		}
		first := w.Close(ctx)
		if !errors.Is(first, database.ErrLegacyWorkerOwnership) || w.Close(ctx) != first || owner.releases.Load() != 1 {
			t.Fatal("release failure lost or retried", first)
		}
		if got, err := f.Redis.Get(ctx, database.LegacyWorkerLockKey).Result(); err != nil || got != successor {
			t.Fatal("old worker deleted the successor's registration", err)
		}
		// Only this owned fixture fault is recovered, by comparing its token.
		if n, err := f.Redis.Eval(ctx, "if redis.call('GET',KEYS[1]) == ARGV[1] then return redis.call('DEL',KEYS[1]) end return 0", []string{database.LegacyWorkerLockKey}, successor).Int(); err != nil || n != 1 {
			t.Fatal("fixture token recovery failed", err)
		}
	})
	t.Run("default_factory_stop", func(t *testing.T) {
		stop, err := defaultStartupDependencies().startWorker(manager, f.Redis)
		if stop != nil {
			defer stop()
		}
		if err != nil {
			t.Fatal(err)
		}
		assertBusy()
		stop()
		stop()
		owner, err := database.AcquireLegacyWorker(ctx, f.Redis)
		if err != nil {
			t.Fatal("default stop did not release its token", err)
		}
		defer owner.Release(ctx)
		if err := f.Redis.Ping(ctx).Err(); err != nil {
			t.Fatal("worker closed shared Redis", err)
		}
		if err := f.DB.WithContext(ctx).Exec("SELECT 1").Error; err != nil {
			t.Fatal("worker closed shared MySQL", err)
		}
	})
}
