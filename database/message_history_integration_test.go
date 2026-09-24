//go:build integration

package database

import (
	"context"
	"errors"
	"fmt"
	"iChat/models"
	"testing"

	"gorm.io/gorm"
)

func TestMessageHistoryIntegration(t *testing.T) {
	f := newLegacyFixture(t)
	ctx := context.Background()
	a, b, outsider := f.Users[0].ID, f.Users[1].ID, f.Users[4].ID
	group := f.Groups[0].ID
	first := models.Message{Type: models.InvalidType}
	var private, grouped []models.Message
	for i := 0; i < 9; i++ {
		msg := f.Message(a, b, models.PrivateType, uint(i%3), fmt.Sprintf("history-%d", i))
		if i%2 == 1 {
			msg.SenderId, msg.ReceiverId = b, a
		}
		// Exercise all three ordering keys, including identical timestamp/identifier.
		msg.Id = fmt.Sprintf("history-%02d", i)
		msg.TimeStamp = 100 + int64(i/6)
		msg.Identifier = uint(i / 3)
		private = append(private, msg)
		gm := msg
		gm.Id = fmt.Sprintf("group-%02d", i)
		gm.Type, gm.ReceiverId = models.GroupType, group
		grouped = append(grouped, gm)
	}
	for _, rows := range [][]models.Message{private, grouped} {
		if err := f.DB.Create(&rows).Error; err != nil {
			t.Fatal(err)
		}
	}
	unrelated := f.Message(a, outsider, models.PrivateType, models.TextMedia, "not this conversation")
	if err := f.DB.Create(&unrelated).Error; err != nil {
		t.Fatal(err)
	}
	// Only the newest committed message is cached. Older history must remain visible.
	for _, rows := range [][]models.Message{private, grouped} {
		if err := f.messages.SaveMsg(ctx, &rows[len(rows)-1]); err != nil {
			t.Fatal(err)
		}
	}
	checkPages := func(t *testing.T, uid, target, kind uint, want []models.Message) {
		t.Helper()
		cursor := first
		var got []models.Message
		for page := 0; page < 10; page++ {
			raw, err := f.messages.LoadMsgs(ctx, uid, target, kind, cursor, 2)
			if err != nil {
				t.Fatal(err)
			}
			rows := decodeMessages(t, raw)
			if len(rows) == 0 {
				break
			}
			if len(rows) > 2 {
				t.Fatal("unbounded page")
			}
			got = append(got, rows...)
			cursor = rows[len(rows)-1]
			// Only the stored cursor identity may determine the boundary.
			cursor.TimeStamp, cursor.Identifier, cursor.Content = 999, 999, "forged"
		}
		reversed := make([]models.Message, len(want))
		for i := range want {
			reversed[len(want)-1-i] = want[i]
		}
		assertMessages(t, got, reversed)
	}
	t.Run("partial_cache_private", func(t *testing.T) { checkPages(t, a, b, models.PrivateType, private) })
	t.Run("reverse_private", func(t *testing.T) { checkPages(t, b, a, models.PrivateType, private) })
	t.Run("partial_cache_group", func(t *testing.T) { checkPages(t, b, group, models.GroupType, grouped) })
	t.Run("cursor_scope", func(t *testing.T) {
		for _, cursor := range []models.Message{unrelated, grouped[0], {Id: "missing", Type: models.PrivateType}} {
			rows, err := f.messages.LoadMsgs(ctx, a, b, models.PrivateType, cursor, 2)
			if rows != nil || !errors.Is(err, ErrMessageCursorNotFound) {
				t.Fatalf("foreign/missing cursor rows=%v err=%v", rows, err)
			}
		}
	})
	t.Run("group_permissions", func(t *testing.T) {
		deny := func(uid, target uint) {
			t.Helper()
			rows, err := f.messages.LoadMsgs(ctx, uid, target, models.GroupType, first, 2)
			if err == nil || rows != nil || errors.Is(err, ErrStorageUnavailable) {
				t.Fatalf("unauthorized history rows=%v err=%v", rows, err)
			}
		}
		deny(outsider, group)
		deny(a, 999999)
		// A newly joined current member can read messages sent before joining.
		relation := models.Relation{OwnerId: outsider, TargetId: group, Type: models.GroupRelation}
		if err := f.DB.Create(&relation).Error; err != nil {
			t.Fatal(err)
		}
		checkPages(t, outsider, group, models.GroupType, grouped)
		if err := f.DB.Delete(&relation).Error; err != nil {
			t.Fatal(err)
		}
		deny(outsider, group)
		// Owner status alone does not bypass a removed membership.
		if err := f.DB.Where("owner_id = ? AND target_id = ? AND type = ?", a, group, models.GroupRelation).Delete(&models.Relation{}).Error; err != nil {
			t.Fatal(err)
		}
		deny(a, group)
		// Keep b's relation and cache; deletion of the group still revokes access.
		if err := f.DB.Delete(&f.Groups[0]).Error; err != nil {
			t.Fatal(err)
		}
		deny(b, group)
	})
	t.Run("dependencies", func(t *testing.T) {
		for _, db := range []*gorm.DB{nil, {}} {
			if rows, err := NewMessageManager(db, f.Redis).LoadMsgs(ctx, a, b, models.PrivateType, first, 2); rows != nil || !errors.Is(err, ErrStorageUnavailable) {
				t.Fatalf("missing DB with warm cache: %v %v", rows, err)
			}
		}
		// History has no Redis dependency.
		if rows, err := NewMessageManager(f.DB, nil).LoadMsgs(ctx, a, b, models.PrivateType, first, 2); err != nil || len(rows) != 2 {
			t.Fatalf("DB-only history: %v %v", rows, err)
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if rows, err := f.messages.LoadMsgs(canceled, a, b, models.PrivateType, first, 2); rows != nil || !errors.Is(err, context.Canceled) || !errors.Is(err, ErrStorageUnavailable) {
			t.Fatalf("canceled: %v %v", rows, err)
		}
		if err := f.DB.Exec("RENAME TABLE message TO history_unavailable").Error; err != nil {
			t.Fatal(err)
		}
		defer f.DB.Exec("RENAME TABLE history_unavailable TO message")
		if rows, err := f.messages.LoadMsgs(ctx, a, b, models.PrivateType, first, 2); rows != nil || !errors.Is(err, ErrStorageUnavailable) {
			t.Fatalf("failed DB with warm cache: %v %v", rows, err)
		}
	})
}
