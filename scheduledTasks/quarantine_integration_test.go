//go:build integration

package scheduledtasks

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"iChat/database"
	"iChat/models"
	"iChat/tests/testenv"
	"reflect"
	"testing"

	"github.com/go-redis/redis/v8"
)

func TestDirtyMessageQuarantineIntegration(t *testing.T) {
	f := testenv.NewLegacyFixture(t)
	ctx := context.Background()
	store := database.NewMessageManager(f.DB, f.Redis)
	first := f.Message(f.Users[0].ID, f.Users[1].ID, models.PrivateType, models.TextMedia, "before")
	last := f.Message(f.Users[0].ID, f.Users[1].ID, models.PrivateType, models.TextMedia, "after")
	raw := func(msg models.Message) string {
		b, err := json.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if err := f.Redis.RPush(ctx, "dirty:msgs", raw(first), "{broken", raw(last)).Err(); err != nil {
		t.Fatal(err)
	}
	if err := SyncMsg(ctx, store); err != nil {
		t.Fatalf("bad record blocked valid tail: %v", err)
	}
	assertSyncState(t, f, []models.Message{first, last}, 0)
	var count int64
	if err := f.DB.Table("legacy_message_quarantine").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("quarantine count=%d error=%v", count, err)
	}
	assertQueue := func(want []string) {
		t.Helper()
		got, err := f.Redis.LRange(ctx, "dirty:msgs", 0, -1).Result()
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("queue=%q want=%q err=%v", got, want, err)
		}
	}
	quarantine := func() []models.LegacyMessageQuarantine {
		t.Helper()
		var records []models.LegacyMessageQuarantine
		if err := f.DB.Order("payload_hash").Find(&records).Error; err != nil {
			t.Fatal(err)
		}
		return records
	}
	conflict := first
	conflict.Content = "must not overwrite"
	blocked := f.Message(first.SenderId, first.ReceiverId, first.Type, first.Media, "blocked")
	if err := f.DB.Exec("ALTER TABLE message ADD CONSTRAINT quarantine_test_content CHECK (content <> 'blocked')").Error; err != nil {
		t.Fatal(err)
	}
	cases := []struct{ raw, reason string }{
		{"{broken", "INVALID_JSON"},
		{"\xff\x00\xfe", "INVALID_JSON"},
		{"null", "INVALID_MESSAGE"},
		{raw(conflict), "ID_CONFLICT"},
		{raw(blocked), "SQL_DATA_3819"},
	}
	for _, tc := range cases {
		if err := f.Redis.RPush(ctx, "dirty:msgs", tc.raw, tc.raw).Err(); err != nil {
			t.Fatal(err)
		}
	}
	// Isolation commits, then queue acknowledgement fails. Retrying must neither
	// duplicate audit rows nor re-execute previously isolated SQL failures.
	failure := errors.New("injected acknowledgement failure")
	rds := redis.NewClient(f.Redis.Options())
	t.Cleanup(func() { _ = rds.Close() })
	rds.AddHook(&syncCommandFailure{command: "eval", cause: failure})
	if err := SyncMsg(ctx, database.NewMessageManager(f.DB, rds)); !errors.Is(err, failure) {
		t.Fatalf("ack failure=%v", err)
	}
	before := quarantine()
	if len(before) != len(cases) {
		t.Fatalf("audit count=%d want=%d", len(before), len(cases))
	}
	for _, tc := range cases {
		found := false
		for _, row := range before {
			if string(row.RawBody) == tc.raw {
				found = true
				if row.PayloadHash != fmt.Sprintf("%x", sha256.Sum256([]byte(tc.raw))) || row.ReasonCode != tc.reason || row.ReasonDetail == "" || row.CreatedAt.IsZero() {
					t.Fatalf("invalid audit: %+v", row)
				}
			}
		}
		if !found {
			t.Fatalf("missing original bytes %q", tc.raw)
		}
	}
	if err := f.DB.Exec("ALTER TABLE message DROP CHECK quarantine_test_content").Error; err != nil {
		t.Fatal(err)
	}
	if err := SyncMsg(ctx, store); err != nil {
		t.Fatal(err)
	}
	if after := quarantine(); !reflect.DeepEqual(before, after) {
		t.Fatalf("replay changed audit: before=%+v after=%+v", before, after)
	}
	assertSyncState(t, f, []models.Message{first, last}, 0)

	// Reject only this new audit record in the fixture: its preceding valid
	// message may be acknowledged, but the failed record and tail must stay.
	bad := "{isolation must fail"
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(bad)))
	if err := f.DB.Exec("ALTER TABLE legacy_message_quarantine ADD CONSTRAINT quarantine_test_reject CHECK (payload_hash <> '" + hash + "')").Error; err != nil {
		t.Fatal(err)
	}
	head := f.Message(first.SenderId, first.ReceiverId, first.Type, first.Media, "committed prefix")
	tail := f.Message(first.SenderId, first.ReceiverId, first.Type, first.Media, "untouched tail")
	if err := f.Redis.RPush(ctx, "dirty:msgs", raw(head), bad, raw(tail)).Err(); err != nil {
		t.Fatal(err)
	}
	if err := SyncMsg(ctx, store); !errors.Is(err, database.ErrStorageUnavailable) {
		t.Fatalf("isolation write failure=%v", err)
	}
	assertQueue([]string{bad, raw(tail)})
	assertSyncState(t, f, []models.Message{first, last, head}, 2)
	if got := quarantine(); !reflect.DeepEqual(got, before) {
		t.Fatal("failed isolation changed audit")
	}
	if err := f.DB.Exec("ALTER TABLE legacy_message_quarantine DROP CHECK quarantine_test_reject").Error; err != nil {
		t.Fatal(err)
	}
	if err := SyncMsg(ctx, store); err != nil {
		t.Fatal(err)
	}
	assertSyncState(t, f, []models.Message{first, last, head, tail}, 0)
	if len(quarantine()) != len(before)+1 {
		t.Fatal("recovered isolation missing")
	}

	// Missing isolation schema is an infrastructure error, never grounds for ACK.
	if err := f.DB.Exec("RENAME TABLE legacy_message_quarantine TO unavailable_quarantine").Error; err != nil {
		t.Fatal(err)
	}
	if err := f.Redis.RPush(ctx, "dirty:msgs", "{unavailable").Err(); err != nil {
		t.Fatal(err)
	}
	if err := SyncMsg(ctx, store); !errors.Is(err, database.ErrStorageUnavailable) {
		t.Fatalf("missing isolation table=%v", err)
	}
	assertQueue([]string{"{unavailable"})
	if err := f.DB.Exec("RENAME TABLE unavailable_quarantine TO legacy_message_quarantine").Error; err != nil {
		t.Fatal(err)
	}
	if err := SyncMsg(ctx, store); err != nil {
		t.Fatal(err)
	}
	assertSyncState(t, f, []models.Message{first, last, head, tail}, 0)
}
