//go:build integration

package scheduledtasks

import (
	"context"
	"errors"
	"iChat/database"
	"iChat/models"
	"iChat/tests/testenv"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
)

func TestSyncMsg(t *testing.T) {
	f := testenv.NewLegacyFixture(t)
	store := database.NewMessageManager(f.DB, f.Redis)
	messages := []models.Message{
		f.Message(f.Users[0].ID, f.Users[1].ID, models.PrivateType, models.TextMedia, "hello"),
		f.Message(f.Users[1].ID, f.Users[0].ID, models.PrivateType, models.PictureMedia, "/fixture/image.png"),
		f.Message(f.Users[0].ID, f.Groups[0].ID, models.GroupType, models.AudioMedia, "/fixture/audio.ogg"),
	}
	for i := range messages {
		if err := store.SaveMsg(context.Background(), &messages[i]); err != nil {
			t.Fatal(err)
		}
	}
	assertSyncState(t, f, nil, int64(len(messages)))
	if err := SyncMsg(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	assertSyncState(t, f, messages, 0)
	// A second invocation with an empty queue must not change committed data.
	if err := SyncMsg(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	assertSyncState(t, f, messages, 0)

	// Each invocation persists and acknowledges only the bounded head batch.
	for i := 0; i < database.DirtyMessageBatchSize+2; i++ {
		msg := f.Message(f.Users[0].ID, f.Users[1].ID, models.PrivateType, models.TextMedia, "bounded")
		if err := store.SaveMsg(context.Background(), &msg); err != nil {
			t.Fatal(err)
		}
		messages = append(messages, msg)
	}
	if err := SyncMsg(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	assertSyncState(t, f, messages[:len(messages)-2], 2)
	if err := SyncMsg(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	assertSyncState(t, f, messages, 0)

	// Redis read failures must not be reported as an empty queue.
	closed := redis.NewClient(f.Redis.Options())
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := SyncMsg(context.Background(), database.NewMessageManager(f.DB, closed)); !errors.Is(err, database.ErrStorageUnavailable) || !errors.Is(err, redis.ErrClosed) {
		t.Fatalf("closed Redis: %v", err)
	}

	pending := f.Message(f.Users[0].ID, f.Users[1].ID, models.PrivateType, models.TextMedia, "pending")
	if err := store.SaveMsg(context.Background(), &pending); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := SyncMsg(canceled, store); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled sync: %v", err)
	}
	assertSyncState(t, f, messages, 1)
	for _, err := range []error{store.SaveMsgs2DB(canceled, []string{`{}`}), store.RemDirtyMsgs(canceled, []string{`{}`})} {
		if !errors.Is(err, context.Canceled) || !errors.Is(err, database.ErrStorageUnavailable) {
			t.Fatalf("canceled repository: %v", err)
		}
	}
	if _, _, err := store.GetAllDirtyMsgs(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read: %v", err)
	}
	assertSyncState(t, f, messages, 1)

	readFailure := errors.New("injected Redis range failure")
	readRedis := redis.NewClient(f.Redis.Options())
	t.Cleanup(func() { _ = readRedis.Close() })
	readRedis.AddHook(&syncCommandFailure{command: "lrange", cause: readFailure})
	if err := SyncMsg(context.Background(), database.NewMessageManager(f.DB, readRedis)); !errors.Is(err, readFailure) || !errors.Is(err, database.ErrStorageUnavailable) {
		t.Fatalf("failed range read: %v", err)
	}
	assertSyncState(t, f, messages, 1)

	// Mutate only this fixture's schema to make persistence fail after reading.
	if err := f.DB.Exec("RENAME TABLE message TO sync_message_unavailable").Error; err != nil {
		t.Fatal(err)
	}
	if err := SyncMsg(context.Background(), store); !errors.Is(err, database.ErrStorageUnavailable) {
		t.Fatalf("failed persistence: %v", err)
	}
	if err := f.DB.Exec("RENAME TABLE sync_message_unavailable TO message").Error; err != nil {
		t.Fatal(err)
	}
	assertSyncState(t, f, messages, 1)

	// Keep real reads and MySQL writes, but inject an ACK command failure.
	ackFailure := errors.New("injected Redis acknowledgement failure")
	ackRedis := redis.NewClient(f.Redis.Options())
	t.Cleanup(func() { _ = ackRedis.Close() })
	ackRedis.AddHook(&syncCommandFailure{command: "eval", cause: ackFailure})
	if err := SyncMsg(context.Background(), database.NewMessageManager(f.DB, ackRedis)); !errors.Is(err, ackFailure) {
		t.Fatalf("failed acknowledgement: %v", err)
	}
	messages = append(messages, pending)
	assertSyncState(t, f, messages, 1)
	// Retry after a committed batch lost its Redis acknowledgement.
	if err := SyncMsg(context.Background(), store); err != nil {
		t.Fatalf("replay committed batch: %v", err)
	}
	assertSyncState(t, f, messages, 0)
}

type syncCommandFailure struct {
	command string
	cause   error
}

func (h *syncCommandFailure) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	if cmd.Name() == h.command {
		return ctx, h.cause
	}
	return ctx, nil
}
func (*syncCommandFailure) AfterProcess(context.Context, redis.Cmder) error { return nil }
func (*syncCommandFailure) BeforeProcessPipeline(ctx context.Context, _ []redis.Cmder) (context.Context, error) {
	return ctx, nil
}
func (*syncCommandFailure) AfterProcessPipeline(context.Context, []redis.Cmder) error { return nil }

func assertSyncState(t *testing.T, f *testenv.LegacyFixture, want []models.Message, queued int64) {
	t.Helper()
	// Fixture IDs have unpadded numeric suffixes, so SQL's ID order differs
	// from generation order once the fixture contains at least ten messages.
	want = append([]models.Message(nil), want...)
	sort.Slice(want, func(i, j int) bool { return want[i].Id < want[j].Id })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got []models.Message
	if err := f.DB.WithContext(ctx).Order("id").Find(&got).Error; err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("persisted count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Fatalf("persisted message = %#v, want %#v", got[i], want[i])
		}
	}
	if size, err := f.Redis.LLen(ctx, "dirty:msgs").Result(); err != nil || size != queued {
		t.Fatalf("dirty queue length = %d, want %d; err=%v", size, queued, err)
	}
}
