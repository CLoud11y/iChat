//go:build integration

package scheduledtasks

import (
	"context"
	"encoding/json"
	"errors"
	"iChat/database"
	"iChat/models"
	"iChat/tests/testenv"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
)

// Leave Redis commands real, then replace the first successful EVAL result
// observed by the caller. This tests an unknown outcome, not network transport.
type lostAckReply struct {
	syncCommandFailure
	applied bool
}

func (h *lostAckReply) AfterProcess(_ context.Context, cmd redis.Cmder) error {
	if !h.applied && cmd.Name() == "eval" && cmd.Err() == nil {
		result, ok := cmd.(*redis.Cmd)
		if !ok {
			return errors.New("unexpected EVAL command type")
		}
		if n, err := result.Int(); err != nil || n != 1 {
			return errors.New("EVAL did not acknowledge the prefix")
		}
		h.applied = true
		return io.EOF
	}
	return nil
}

func TestLegacyAcknowledgementUnknownIntegration(t *testing.T) {
	f := testenv.NewLegacyFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	rds := redis.NewClient(f.Redis.Options())
	t.Cleanup(func() { _ = rds.Close() })
	hook := &lostAckReply{}
	rds.AddHook(hook)
	owner, err := database.AcquireLegacyWorker(ctx, rds)
	if err != nil {
		t.Fatal(err)
	}
	worker := OwnedMessages{MessageStore: database.NewMessageManager(f.DB, rds), Owner: owner}
	var messages []models.Message
	var records []interface{}
	// One invalid record plus 127 valid ones fill the read batch. The last
	// valid message must survive ACK uncertainty without being persisted yet.
	records = append(records, "{bad-ack-outcome:"+f.Env.RunID())
	for i := 0; i < database.DirtyMessageBatchSize; i++ {
		msg := f.Message(f.Users[0].ID, f.Users[1].ID, models.PrivateType, models.TextMedia, "ack outcome")
		messages = append(messages, msg)
		raw, err := json.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, string(raw))
	}
	if err := f.Redis.RPush(ctx, "dirty:msgs", records...).Err(); err != nil {
		t.Fatal(err)
	}
	err = SyncMsg(ctx, worker)
	if !hook.applied || !errors.Is(err, io.EOF) || !errors.Is(err, database.ErrLegacyWorkerOwnership) {
		t.Fatalf("applied=%v unknown ACK error=%v", hook.applied, err)
	}
	assertTail := func() {
		t.Helper()
		assertSyncState(t, f, messages[:len(messages)-1], 1)
		got, err := f.Redis.LRange(ctx, "dirty:msgs", 0, -1).Result()
		if err != nil || !reflect.DeepEqual(got, []string{records[len(records)-1].(string)}) {
			t.Fatalf("unknown ACK changed unprocessed tail: %q err=%v", got, err)
		}
	}
	assertTail()
	var before []models.LegacyMessageQuarantine
	if err := f.DB.WithContext(ctx).Find(&before).Error; err != nil || len(before) != 1 || string(before[0].RawBody) != records[0].(string) || before[0].ReasonCode != "INVALID_JSON" {
		t.Fatalf("committed quarantine=%+v err=%v", before, err)
	}
	// An uncertain owner is permanently stopped, even though Redis is healthy.
	if err := SyncMsg(ctx, worker); !errors.Is(err, database.ErrLegacyWorkerOwnership) {
		t.Fatalf("uncertain owner resumed: %v", err)
	}
	assertTail()
	if other, err := database.AcquireLegacyWorker(ctx, f.Redis); !errors.Is(err, database.ErrLegacyWorkerBusy) {
		if other != nil {
			_ = other.Release(ctx)
		}
		t.Fatalf("uncertain registration allowed takeover: %v", err)
	}
	// No call is in flight: this is the normal stop-and-release path, unlike
	// the process-kill cases which require documented manual token recovery.
	if err := owner.Release(ctx); err != nil {
		t.Fatal(err)
	}
	replacement, err := database.AcquireLegacyWorker(ctx, f.Redis)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := replacement.Release(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	fresh := OwnedMessages{MessageStore: database.NewMessageManager(f.DB, f.Redis), Owner: replacement}
	if err := SyncMsg(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	assertSyncState(t, f, messages, 0)
	// Re-deliver the already-ACKed batch: both business and audit rows remain
	// immutable and a new, bounded read is required to consume every record.
	if err := f.Redis.RPush(ctx, "dirty:msgs", records...).Err(); err != nil {
		t.Fatal(err)
	}
	if err := SyncMsg(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	assertSyncState(t, f, messages, 1)
	if err := SyncMsg(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	assertSyncState(t, f, messages, 0)
	var after []models.LegacyMessageQuarantine
	if err := f.DB.WithContext(ctx).Find(&after).Error; err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("replay changed quarantine: %+v err=%v", after, err)
	}
}
