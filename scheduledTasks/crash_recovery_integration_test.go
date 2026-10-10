//go:build integration

package scheduledtasks

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"iChat/config"
	"iChat/database"
	"iChat/models"
	"iChat/tests/testenv"
	"iChat/utils"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

type crashWorkerInput struct {
	Config    config.Config
	RunID     string
	StopAfter int
}

// This barrier wraps real per-record transactions only in the integration test
// binary. No production failpoint, alternate persistence or ACK path is added.
type committedMessageBarrier struct {
	MessageStore
	stopAfter    int
	completed    int
	connectionID uint64
}

func (s *committedMessageBarrier) ProcessDirtyMsgs(ctx context.Context, raw []string) (int, error) {
	n, err := s.MessageStore.ProcessDirtyMsgs(ctx, raw)
	s.completed += n
	if err == nil && s.completed == s.stopAfter {
		fmt.Printf("COMMITTED %d %d\n", s.completed, s.connectionID)
		// The parent kills this process; a deadline is a test failure, not an ACK.
		<-ctx.Done()
		return n, ctx.Err()
	}
	return n, err
}

func TestLegacyCrashWorkerProcess(t *testing.T) {
	if os.Getenv("ICHAT_TEST_CRASH_WORKER") != "1" {
		return
	}
	var input crashWorkerInput
	if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
		t.Fatal(err)
	}
	if len(input.RunID) != 32 || input.Config.MYSQL.DbName != "ichat_test_"+input.RunID ||
		input.Config.MYSQL.Host != "127.0.0.1" || !strings.HasPrefix(input.Config.REDIS.Addr, "127.0.0.1:") ||
		input.StopAfter < 0 || input.StopAfter > 3 {
		t.Fatal("refusing non-fixture worker configuration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := utils.NewMySQL(input.Config)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	var schema string
	if err := db.WithContext(ctx).Raw("SELECT DATABASE()").Scan(&schema).Error; err != nil || schema != input.Config.MYSQL.DbName {
		t.Fatalf("fixture schema validation: %q, %v", schema, err)
	}
	var connectionID uint64
	if err := db.WithContext(ctx).Raw("SELECT CONNECTION_ID()").Scan(&connectionID).Error; err != nil {
		t.Fatal(err)
	}
	rds, err := utils.OpenRedis(ctx, input.Config)
	if err != nil {
		t.Fatal(err)
	}
	defer rds.Close()
	owner, err := database.AcquireLegacyWorker(ctx, rds)
	if err != nil {
		t.Fatal(err)
	}
	store := &committedMessageBarrier{
		MessageStore: database.NewMessageManager(db, rds),
		stopAfter:    input.StopAfter, connectionID: connectionID,
	}
	if err := SyncMsg(ctx, OwnedMessages{MessageStore: store, Owner: owner}); err != nil {
		t.Fatal(err)
	}
	if err := owner.Release(ctx); err != nil {
		t.Fatal(err)
	}
	fmt.Println("DRAINED")
}

type crashWorkerProcess struct {
	cmd    *exec.Cmd
	out    *bufio.Scanner
	stderr bytes.Buffer
}

func startCrashWorker(t *testing.T, f *testenv.LegacyFixture, stopAfter int) *crashWorkerProcess {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	conf, err := f.Env.Config()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	p := &crashWorkerProcess{cmd: exec.CommandContext(ctx, exe, "-test.run=^TestLegacyCrashWorkerProcess$", "-test.timeout=40s")}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(entry), "ICHAT_TEST_CRASH_WORKER=") {
			p.cmd.Env = append(p.cmd.Env, entry)
		}
	}
	p.cmd.Env = append(p.cmd.Env, "ICHAT_TEST_CRASH_WORKER=1")
	// Only this run's fixture configuration travels over stdin, never arguments
	// or inherited application configuration. Credentials are not logged.
	var input bytes.Buffer
	if err := json.NewEncoder(&input).Encode(crashWorkerInput{Config: conf, RunID: f.Env.RunID(), StopAfter: stopAfter}); err != nil {
		cancel()
		t.Fatal(err)
	}
	p.cmd.Stdin = &input
	p.cmd.Stderr = &p.stderr
	out, err := p.cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	p.out = bufio.NewScanner(out)
	if err := p.cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if p.cmd.ProcessState == nil {
			_ = p.cmd.Process.Kill()
			_ = p.cmd.Wait()
		}
		cancel()
	})
	return p
}

func (p *crashWorkerProcess) await(t *testing.T, prefix string) string {
	t.Helper()
	var output strings.Builder
	for p.out.Scan() {
		line := p.out.Text()
		if strings.HasPrefix(line, prefix) {
			return line
		}
		output.WriteString(line + "\n")
	}
	err := p.cmd.Wait()
	t.Fatalf("worker never reported %q: wait=%v scan=%v\n%s%s", prefix, err, p.out.Err(), output.String(), p.stderr.String())
	return ""
}

func TestLegacyWorkerCrashRecoveryIntegration(t *testing.T) {
	for _, tc := range []struct {
		name      string
		committed int
	}{
		{"message_committed", 1},
		{"quarantine_committed", 2},
		{"batch_committed_before_ack", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testenv.NewLegacyFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			first := f.Message(f.Users[0].ID, f.Users[1].ID, models.PrivateType, models.TextMedia, "before crash")
			last := f.Message(f.Users[1].ID, f.Users[0].ID, models.PrivateType, models.PictureMedia, "after quarantine")
			late := f.Message(f.Users[0].ID, f.Groups[0].ID, models.GroupType, models.AudioMedia, "appended while paused")
			raw := func(msg models.Message) string {
				b, err := json.Marshal(msg)
				if err != nil {
					t.Fatal(err)
				}
				return string(b)
			}
			bad := "\xff\x00{broken:" + f.Env.RunID()
			queue := []string{raw(first), bad, raw(last)}
			push := func(records ...string) {
				t.Helper()
				for _, record := range records {
					if err := f.Redis.RPush(ctx, "dirty:msgs", record).Err(); err != nil {
						t.Fatal(err)
					}
				}
			}
			assertQueue := func(want []string) {
				t.Helper()
				got, err := f.Redis.LRange(ctx, "dirty:msgs", 0, -1).Result()
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("queue=%q want=%q error=%v", got, want, err)
				}
			}
			audit := func(want int) []models.LegacyMessageQuarantine {
				t.Helper()
				var rows []models.LegacyMessageQuarantine
				if err := f.DB.WithContext(ctx).Find(&rows).Error; err != nil {
					t.Fatal(err)
				}
				if len(rows) != want {
					t.Fatalf("quarantine rows=%d want=%d", len(rows), want)
				}
				if want == 1 {
					r := rows[0]
					if string(r.RawBody) != bad || r.PayloadHash != fmt.Sprintf("%x", sha256.Sum256([]byte(bad))) ||
						r.ReasonCode != "INVALID_JSON" || r.ReasonDetail == "" || r.CreatedAt.IsZero() {
						t.Fatalf("invalid quarantine row: %+v", r)
					}
				}
				return rows
			}
			push(queue...)
			child := startCrashWorker(t, f, tc.committed)
			line := child.await(t, "COMMITTED ")
			var completed int
			var connectionID uint64
			if _, err := fmt.Sscanf(line, "COMMITTED %d %d", &completed, &connectionID); err != nil || completed != tc.committed || connectionID == 0 {
				t.Fatalf("invalid barrier: %q", line)
			}
			persisted := []models.Message{first}
			if tc.committed == 3 {
				persisted = append(persisted, last)
			}
			assertSyncState(t, f, persisted, 3)
			audits := 0
			if tc.committed >= 2 {
				audits = 1
			}
			before := audit(audits)
			// This record was never in the crashed worker's read batch.
			queue = append(queue, raw(late))
			push(raw(late))
			assertQueue(queue)
			token, err := f.Redis.Get(ctx, database.LegacyWorkerLockKey).Result()
			if err != nil || token == "" {
				t.Fatalf("owner token missing: %v", err)
			}
			if err := child.cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := child.cmd.Wait(); err == nil {
				t.Fatal("killed worker exited successfully")
			}
			// Follow the documented recovery precondition: wait for the old
			// database session to disappear before releasing its registration.
			waitCtx, stopWait := context.WithTimeout(ctx, 10*time.Second)
			defer stopWait()
			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()
			for {
				var sessions int64
				if err := f.DB.WithContext(waitCtx).Raw("SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE ID = ?", connectionID).Scan(&sessions).Error; err != nil {
					t.Fatal(err)
				}
				if sessions == 0 {
					break
				}
				select {
				case <-waitCtx.Done():
					t.Fatal("crashed worker's MySQL session remains")
				case <-ticker.C:
				}
			}
			assertSyncState(t, f, persisted, 4)
			assertQueue(queue)
			if after := audit(audits); !reflect.DeepEqual(before, after) {
				t.Fatal("crash changed committed audit")
			}
			if owner, err := database.AcquireLegacyWorker(ctx, f.Redis); !errors.Is(err, database.ErrLegacyWorkerBusy) {
				if owner != nil {
					_ = owner.Release(ctx)
				}
				t.Fatalf("crash allowed automatic takeover: %v", err)
			}
			if ttl, err := f.Redis.TTL(ctx, database.LegacyWorkerLockKey).Result(); err != nil || ttl != -1 {
				t.Fatalf("crashed registration TTL=%v err=%v", ttl, err)
			}
			// Exact compare-delete from docs/legacy-worker-recovery.md; no DEL
			// fallback and no live consumer remains in this isolated fixture.
			n, err := f.Redis.Eval(ctx, "if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end return 0", []string{database.LegacyWorkerLockKey}, token).Int()
			if err != nil || n != 1 {
				t.Fatalf("manual recovery=%d err=%v", n, err)
			}
			drain := func() {
				t.Helper()
				p := startCrashWorker(t, f, 0)
				p.await(t, "DRAINED")
				if err := p.cmd.Wait(); err != nil {
					t.Fatalf("recovery worker: %v %s", err, p.stderr.String())
				}
				assertSyncState(t, f, []models.Message{first, last, late}, 0)
				if n, err := f.Redis.Exists(ctx, database.LegacyWorkerLockKey).Result(); err != nil || n != 0 {
					t.Fatalf("clean worker retained registration: %d %v", n, err)
				}
			}
			drain()
			after := audit(1)
			if audits == 1 && !reflect.DeepEqual(before, after) {
				t.Fatal("recovery changed committed audit")
			}
			// Re-delivery after recovery must not insert rows, change original
			// content or audit timestamps, or block subsequent queue progress.
			push(queue...)
			drain()
			if again := audit(1); !reflect.DeepEqual(after, again) {
				t.Fatal("replay changed audit")
			}
		})
	}
}
