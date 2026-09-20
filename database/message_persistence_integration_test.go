//go:build integration

package database

import (
	"context"
	"encoding/json"
	"errors"
	"iChat/models"
	"iChat/tests/testenv"
	"testing"

	mysqlDriver "github.com/go-sql-driver/mysql"
)

func TestLegacyMessagePersistenceIntegration(t *testing.T) {
	f := testenv.NewLegacyFixture(t)
	store := NewMessageManager(f.DB, nil)
	ctx := context.Background()
	raw := func(msg models.Message) string {
		b, err := json.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	first := f.Message(f.Users[0].ID, f.Users[1].ID, models.PrivateType, models.TextMedia, "original")
	second := f.Message(f.Users[0].ID, f.Groups[0].ID, models.GroupType, models.AudioMedia, "/audio")
	if err := store.SaveMsgs2DB(ctx, []string{raw(first), raw(first), raw(second)}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMsgs2DB(ctx, []string{raw(second), raw(first)}); err != nil {
		t.Fatal(err)
	}
	assertCount := func(t *testing.T, want int64) {
		t.Helper()
		var count int64
		if err := f.DB.Model(&models.Message{}).Count(&count).Error; err != nil || count != want {
			t.Fatalf("count=%d want=%d err=%v", count, want, err)
		}
	}
	assertCount(t, 2)
	for _, mutate := range []struct {
		name   string
		change func(*models.Message)
	}{
		{"content", func(m *models.Message) { m.Content += " changed" }},
		{"sender", func(m *models.Message) { m.SenderId++ }},
		{"receiver", func(m *models.Message) { m.ReceiverId++ }},
		{"type", func(m *models.Message) { m.Type = models.GroupType }},
		{"media", func(m *models.Message) { m.Media = models.PictureMedia }},
		{"time", func(m *models.Message) { m.TimeStamp++ }},
		{"identifier", func(m *models.Message) { m.Identifier++ }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			conflict := first
			mutate.change(&conflict)
			newMsg := f.Message(first.SenderId, first.ReceiverId, first.Type, first.Media, "must roll back")
			if err := store.SaveMsgs2DB(ctx, []string{raw(newMsg), raw(conflict)}); !errors.Is(err, ErrMessageConflict) {
				t.Fatalf("conflict=%v", err)
			}
			assertCount(t, 2)
			var existing models.Message
			if err := f.DB.Where("id = ?", first.Id).Take(&existing).Error; err != nil || existing != first {
				t.Fatalf("canonical row changed: %+v %v", existing, err)
			}
		})
	}
	// A deterministic SQL constraint error must not be treated as a duplicate.
	if err := f.DB.Exec("ALTER TABLE message ADD CONSTRAINT legacy_test_content CHECK (content <> 'blocked')").Error; err != nil {
		t.Fatal(err)
	}
	newMsg := f.Message(first.SenderId, first.ReceiverId, first.Type, first.Media, "new")
	blocked := f.Message(first.SenderId, first.ReceiverId, first.Type, first.Media, "blocked")
	err := store.SaveMsgs2DB(ctx, []string{raw(newMsg), raw(blocked)})
	var sqlErr *mysqlDriver.MySQLError
	if !errors.Is(err, ErrStorageUnavailable) || !errors.As(err, &sqlErr) || sqlErr.Number != 3819 {
		t.Fatalf("constraint error=%v", err)
	}
	assertCount(t, 2)
	if err := f.DB.Exec("ALTER TABLE message DROP CHECK legacy_test_content").Error; err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"{", `{"id":""}`} {
		if err := store.SaveMsgs2DB(ctx, []string{raw(newMsg), invalid}); err == nil {
			t.Fatal("invalid batch accepted")
		}
		assertCount(t, 2)
	}
	if err := store.SaveMsgs2DB(ctx, []string{raw(first), raw(newMsg)}); err != nil {
		t.Fatal(err)
	}
	assertCount(t, 3)
	// A duplicate on another unique index is not proof this message ID exists.
	if err := f.DB.Exec("CREATE UNIQUE INDEX legacy_test_unique_content ON message(content(100))").Error; err != nil {
		t.Fatal(err)
	}
	otherID := f.Message(first.SenderId, first.ReceiverId, first.Type, first.Media, first.Content)
	if err := store.SaveMsgs2DB(ctx, []string{raw(otherID)}); !errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("unrelated unique violation accepted: %v", err)
	}
	assertCount(t, 3)
	if err := f.DB.Exec("DROP INDEX legacy_test_unique_content ON message").Error; err != nil {
		t.Fatal(err)
	}
}
