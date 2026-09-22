//go:build integration

package database

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"iChat/tests/testenv"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
)

// The child owns a separate Redis connection and an actual process lifetime.
func TestLegacyWorkerProcess(t *testing.T) {
	addr := os.Getenv("ICHAT_TEST_WORKER_REDIS")
	if addr == "" {
		return
	}
	rds := redis.NewClient(&redis.Options{Addr: addr, Password: os.Getenv("ICHAT_TEST_WORKER_PASSWORD")})
	defer rds.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	owner, err := AcquireLegacyWorker(ctx, rds)
	if errors.Is(err, ErrLegacyWorkerBusy) {
		fmt.Println("BUSY")
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("OWNED")
	bufio.NewScanner(os.Stdin).Scan()
	if err := owner.Release(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyWorkerOwnershipIntegration(t *testing.T) {
	f := testenv.NewLegacyFixture(t)
	ctx := context.Background()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := func(want string, kill bool) {
		t.Helper()
		childCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(childCtx, exe, "-test.run=^TestLegacyWorkerProcess$")
		cmd.Env = append(os.Environ(), "ICHAT_TEST_WORKER_REDIS="+f.Redis.Options().Addr, "ICHAT_TEST_WORKER_PASSWORD="+f.Redis.Options().Password)
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		in, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		defer in.Close()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		}()
		scanner := bufio.NewScanner(out)
		found := false
		for scanner.Scan() {
			if strings.TrimSpace(scanner.Text()) == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("child did not report %s", want)
		}
		if kill {
			if _, err := AcquireLegacyWorker(ctx, f.Redis); !errors.Is(err, ErrLegacyWorkerBusy) {
				t.Fatalf("concurrent parent: %v", err)
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err == nil {
				t.Fatal("killed process succeeded")
			}
		} else {
			_ = in.Close()
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
		}
	}
	owner, err := AcquireLegacyWorker(ctx, f.Redis)
	if err != nil {
		t.Fatal(err)
	}
	child("BUSY", false)
	if ttl, err := f.Redis.TTL(ctx, LegacyWorkerLockKey).Result(); err != nil || ttl != -1 {
		t.Fatalf("TTL=%v err=%v", ttl, err)
	}
	if err := owner.Release(ctx); err != nil {
		t.Fatal(err)
	}
	child("OWNED", false)
	child("OWNED", true)
	// A crashed process never yields its registration automatically.
	child("BUSY", false)
	// Simulate documented operator recovery only AFTER the child has exited.
	token, err := f.Redis.Get(ctx, LegacyWorkerLockKey).Result()
	if err != nil {
		t.Fatal(err)
	}
	dead := &LegacyWorkerLock{rds: f.Redis, token: token}
	if err := dead.Release(ctx); err != nil {
		t.Fatal(err)
	}
	owner, err = AcquireLegacyWorker(ctx, f.Redis)
	if err != nil {
		t.Fatal(err)
	}
	if err := dead.Release(ctx); !errors.Is(err, ErrLegacyWorkerOwnership) {
		t.Fatalf("stale release: %v", err)
	}
	if err := owner.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.Redis.RPush(ctx, "dirty:msgs", "first", "second").Err(); err != nil {
		t.Fatal(err)
	}
	if err := owner.Acknowledge(ctx, []string{"wrong"}); !errors.Is(err, ErrDirtyMessagePrefixMismatch) {
		t.Fatal(err)
	}
	if err := owner.Acknowledge(ctx, []string{"first"}); err != nil {
		t.Fatal(err)
	}
	// Lock removal/replacement is an operator fault, never automatic takeover.
	if err := f.Redis.Set(ctx, LegacyWorkerLockKey, "replacement", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := owner.Acknowledge(ctx, []string{"second"}); !errors.Is(err, ErrLegacyWorkerOwnership) {
		t.Fatal(err)
	}
	if n := f.Redis.LLen(ctx, "dirty:msgs").Val(); n != 1 {
		t.Fatalf("stale owner trimmed queue: %d", n)
	}
	if err := f.Redis.Set(ctx, LegacyWorkerLockKey, owner.token, 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := owner.Check(ctx); !errors.Is(err, ErrLegacyWorkerOwnership) {
		t.Fatal("lost ownership resumed")
	}
	if err := owner.Release(ctx); err != nil {
		t.Fatal(err)
	}
	owner, err = AcquireLegacyWorker(ctx, f.Redis)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := owner.Check(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := owner.Check(ctx); !errors.Is(err, ErrLegacyWorkerOwnership) {
		t.Fatal("uncertain ownership resumed")
	}
	if err := owner.Release(ctx); err != nil {
		t.Fatal(err)
	}
}
