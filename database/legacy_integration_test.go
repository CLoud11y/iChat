//go:build integration

package database

import (
	"context"
	"encoding/json"
	"fmt"
	"iChat/models"
	"iChat/tests/testenv"
	"reflect"
	"sort"
	"testing"
	"time"
)

type legacyFixture struct {
	*testenv.LegacyFixture
	messages  *msgManager
	groups    *groupManager
	relations *relationManager
}

func newLegacyFixture(tb testing.TB) *legacyFixture {
	tb.Helper()
	f := testenv.NewLegacyFixture(tb)
	fixture := &legacyFixture{f, NewMessageManager(f.DB, f.Redis), NewGroupManager(f.DB), NewRelationManager(f.DB)}
	return fixture
}

func TestSaveAndLoadMsg(t *testing.T) {
	f := newLegacyFixture(t)
	var sent []models.Message
	for i := 0; i < 20; i++ {
		sender, receiver := f.Users[i%2].ID, f.Users[(i+1)%2].ID
		msg := f.Message(sender, receiver, models.PrivateType, uint(i%3), fmt.Sprintf("消息-%02d", i))
		if err := f.messages.SaveMsg(context.Background(), &msg); err != nil {
			t.Fatal(err)
		}
		sent = append(sent, msg)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := f.Redis.LRange(ctx, getDirtyMsgsKey(), 0, -1).Result()
	if err != nil {
		t.Fatal(err)
	}
	assertMessages(t, decodeMessages(t, raw), sent)
	// Fetch every page, including the final empty page, in both directions.
	for _, reverse := range []bool{false, true} {
		a, b := f.Users[0].ID, f.Users[1].ID
		if reverse {
			a, b = b, a
		}
		cursor := models.Message{Type: models.InvalidType}
		for offset := 0; ; {
			raw, err := f.messages.LoadMsgs(context.Background(), a, b, models.PrivateType, cursor, 7)
			if err != nil {
				t.Fatal(err)
			}
			var want []models.Message
			for i := len(sent) - 1 - offset; i >= 0 && len(want) < 7; i-- {
				want = append(want, sent[i])
			}
			page := decodeMessages(t, raw)
			assertMessages(t, page, want)
			if len(page) == 0 {
				break
			}
			offset += len(page)
			cursor = page[len(page)-1]
		}
	}
}

func TestDeleteMsg(t *testing.T) {
	f := newLegacyFixture(t)
	removed := f.Message(f.Users[0].ID, f.Users[1].ID, models.PrivateType, models.TextMedia, "evicted")
	retained := f.Message(f.Users[0].ID, f.Users[2].ID, models.PrivateType, models.PictureMedia, "/fixture/image.png")
	for _, msg := range []models.Message{removed, retained} {
		if err := f.messages.SaveMsg(context.Background(), &msg); err != nil {
			t.Fatal(err)
		}
	}
	raw, _, err := f.messages.GetAllDirtyMsgs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.messages.SaveMsgs2DB(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	count, err := f.Redis.Del(ctx, getKey(&removed)).Result()
	if err != nil || count != 1 {
		t.Fatalf("must delete exactly one existing cache key: count=%d err=%v", count, err)
	}
	if exists, err := f.Redis.Exists(ctx, getKey(&removed)).Result(); err != nil || exists != 0 {
		t.Fatalf("evicted cache still exists: exists=%d err=%v", exists, err)
	}
	if exists, err := f.Redis.Exists(ctx, getKey(&retained)).Result(); err != nil || exists != 1 {
		t.Fatalf("another conversation's cache was changed: exists=%d err=%v", exists, err)
	}
	for _, want := range []models.Message{removed, retained} {
		raw, err := f.messages.LoadMsgs(context.Background(), want.SenderId, want.ReceiverId, want.Type, models.Message{Type: models.InvalidType}, 10)
		if err != nil {
			t.Fatal(err)
		}
		assertMessages(t, decodeMessages(t, raw), []models.Message{want})
	}
	var persisted int64
	if err := f.DB.Model(&models.Message{}).Count(&persisted).Error; err != nil || persisted != 2 {
		t.Fatalf("cache eviction changed stored messages: count=%d err=%v", persisted, err)
	}
}

func TestGetGroups2(t *testing.T) {
	f := newLegacyFixture(t)
	for _, query := range []func(context.Context, uint) ([]models.Group, error){f.groups.GetGroupsByUid, f.groups.GetGroupsByUid2} {
		groups, err := query(context.Background(), f.Users[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		assertIDs(t, groupIDs(groups), f.Groups[0].ID, f.Groups[1].ID)
		groups, err = query(context.Background(), f.Users[4].ID)
		if err != nil {
			t.Fatal(err)
		}
		assertIDs(t, groupIDs(groups))
	}
}

func TestSearchFriends(t *testing.T) {
	f := newLegacyFixture(t)
	for _, query := range []func(context.Context, uint) ([]models.User, error){f.relations.SearchFriends, f.relations.SearchFriends2} {
		friends, err := query(context.Background(), f.Users[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		assertIDs(t, userIDs(friends), f.Users[1].ID)
		friends, err = query(context.Background(), f.Users[1].ID)
		if err != nil {
			t.Fatal(err)
		}
		assertIDs(t, userIDs(friends), f.Users[0].ID)
		friends, err = query(context.Background(), f.Users[4].ID)
		if err != nil {
			t.Fatal(err)
		}
		assertIDs(t, userIDs(friends))
	}
}

func TestGetGroupUsers(t *testing.T) {
	f := newLegacyFixture(t)
	for _, test := range []struct {
		group uint
		users []uint
	}{
		{f.Groups[0].ID, []uint{f.Users[0].ID, f.Users[1].ID}},
		{f.Groups[1].ID, []uint{f.Users[0].ID, f.Users[2].ID}},
		{f.Groups[2].ID, []uint{f.Users[3].ID}},
	} {
		users, err := f.groups.GetGroupUsers(context.Background(), test.group)
		if err != nil {
			t.Fatal(err)
		}
		assertIDs(t, userIDs(users), test.users...)
	}
}

func BenchmarkGetGroups(b *testing.B)  { benchmarkGroups(b, false) }
func BenchmarkGetGroups2(b *testing.B) { benchmarkGroups(b, true) }
func benchmarkGroups(b *testing.B, joined bool) {
	b.StopTimer()
	f := newLegacyFixture(b)
	query := f.groups.GetGroupsByUid
	if joined {
		query = f.groups.GetGroupsByUid2
	}
	groups, err := query(context.Background(), f.Users[0].ID)
	if err != nil {
		b.Fatal(err)
	}
	assertIDs(b, groupIDs(groups), f.Groups[0].ID, f.Groups[1].ID)
	b.ResetTimer()
	b.StartTimer()
	for i := 0; i < b.N; i++ {
		groups, err = query(context.Background(), f.Users[0].ID)
		if err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	assertIDs(b, groupIDs(groups), f.Groups[0].ID, f.Groups[1].ID)
}

func BenchmarkSearchFriends(b *testing.B)  { benchmarkFriends(b, false) }
func BenchmarkSearchFriends2(b *testing.B) { benchmarkFriends(b, true) }
func benchmarkFriends(b *testing.B, joined bool) {
	b.StopTimer()
	f := newLegacyFixture(b)
	query := f.relations.SearchFriends
	if joined {
		query = f.relations.SearchFriends2
	}
	friends, err := query(context.Background(), f.Users[0].ID)
	if err != nil {
		b.Fatal(err)
	}
	assertIDs(b, userIDs(friends), f.Users[1].ID)
	b.ResetTimer()
	b.StartTimer()
	for i := 0; i < b.N; i++ {
		friends, err = query(context.Background(), f.Users[0].ID)
		if err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	assertIDs(b, userIDs(friends), f.Users[1].ID)
}

func decodeMessages(tb testing.TB, raw []string) []models.Message {
	tb.Helper()
	var messages []models.Message
	for _, item := range raw {
		var msg models.Message
		if err := json.Unmarshal([]byte(item), &msg); err != nil {
			tb.Fatal(err)
		}
		messages = append(messages, msg)
	}
	return messages
}

func assertMessages(tb testing.TB, got, want []models.Message) {
	tb.Helper()
	if !reflect.DeepEqual(got, want) {
		tb.Fatalf("messages = %#v, want %#v", got, want)
	}
}

func userIDs(users []models.User) []uint {
	var ids []uint
	for _, user := range users {
		ids = append(ids, user.ID)
	}
	return ids
}

func groupIDs(groups []models.Group) []uint {
	var ids []uint
	for _, group := range groups {
		ids = append(ids, group.ID)
	}
	return ids
}

func assertIDs(tb testing.TB, got []uint, want ...uint) {
	tb.Helper()
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	if !reflect.DeepEqual(got, want) {
		tb.Fatalf("IDs = %v, want %v", got, want)
	}
}
