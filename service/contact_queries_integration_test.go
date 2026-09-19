//go:build integration

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iChat/database"
	"iChat/models"
	"iChat/tests/testenv"
	"reflect"
	"sort"
	"testing"
)

func TestContactQueriesMySQLIntegration(t *testing.T) {
	f := testenv.NewLegacyFixture(t)
	friends, groups := database.NewRelationManager(f.DB), database.NewGroupManager(f.DB)
	queries := ContactQueries{Friends: friends, Groups: groups}
	ctx := context.Background()
	uid := f.Users[0].ID
	body := fmt.Sprintf(`{"group_id":%d}`, f.Groups[0].ID)
	oldFriends, oldGroups := database.Rmanager, database.Gmanager
	database.Rmanager, database.Gmanager = nil, nil
	t.Cleanup(func() { database.Rmanager, database.Gmanager = oldFriends, oldGroups })

	assertIDs := func(got, want []uint) {
		t.Helper()
		sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
		sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("IDs=%v want=%v", got, want)
		}
	}
	var contacts []ContactInfo
	contactData(t, contactRequest(GetContacts(queries), body, ctx, uid), &contacts)
	var friendIDs, groupIDs []uint
	for _, contact := range contacts {
		if contact.IsGroup == 1 {
			groupIDs = append(groupIDs, contact.Id)
		} else {
			friendIDs = append(friendIDs, contact.Id)
		}
	}
	assertIDs(friendIDs, []uint{f.Users[1].ID})
	assertIDs(groupIDs, []uint{f.Groups[0].ID, f.Groups[1].ID})
	var users []models.User
	if total := contactData(t, contactRequest(SearchFriends(friends), body, ctx, uid), &users); total != 1 || len(users) != 1 || users[0].ID != f.Users[1].ID {
		t.Fatalf("unexpected friends: total=%d rows=%v", total, users)
	}
	var items []models.Group
	if total := contactData(t, contactRequest(LoadGroups(groups), body, ctx, uid), &items); total != 2 || len(items) != 2 {
		t.Fatalf("unexpected groups: total=%d rows=%v", total, items)
	}
	assertIDs([]uint{items[0].ID, items[1].ID}, []uint{f.Groups[0].ID, f.Groups[1].ID})
	var members []struct {
		UserID uint `json:"user_id"`
	}
	if total := contactData(t, contactRequest(LoadGroupUsers(groups), body, ctx, uid), &members); total != 2 || len(members) != 2 {
		t.Fatalf("unexpected members: total=%d rows=%v", total, members)
	}
	assertIDs([]uint{members[0].UserID, members[1].UserID}, []uint{f.Users[0].ID, f.Users[1].ID})
	for _, action := range []string{"contacts", "friends", "groups", "members"} {
		var rows []json.RawMessage
		contactData(t, contactRequest(contactHandler(action, queries), `{"group_id":0}`, ctx, f.Users[4].ID), &rows)
		if rows == nil || len(rows) != 0 {
			t.Fatalf("%s: expected empty array, got %v", action, rows)
		}
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	for name, query := range map[string]func(context.Context) error{
		"friends":      func(ctx context.Context) error { _, err := friends.SearchFriends(ctx, uid); return err },
		"friends_join": func(ctx context.Context) error { _, err := friends.SearchFriends2(ctx, uid); return err },
		"groups":       func(ctx context.Context) error { _, err := groups.GetGroupsByUid(ctx, uid); return err },
		"groups_join":  func(ctx context.Context) error { _, err := groups.GetGroupsByUid2(ctx, uid); return err },
		"group_ids":    func(ctx context.Context) error { _, err := groups.GetGroupIds(ctx, uid); return err },
		"groups_by_id": func(ctx context.Context) error {
			_, err := groups.GetGroupsById(ctx, []uint{f.Groups[0].ID})
			return err
		},
		"members": func(ctx context.Context) error { _, err := groups.GetGroupUsers(ctx, f.Groups[0].ID); return err },
	} {
		if err := query(canceled); !errors.Is(err, context.Canceled) || !errors.Is(err, database.ErrStorageUnavailable) {
			t.Fatalf("%s: canceled query error=%v", name, err)
		}
	}
	for _, action := range []string{"contacts", "friends", "groups", "members"} {
		assertAccountUnavailable(t, contactRequest(contactHandler(action, queries), body, canceled, uid))
	}

	// Only this run's isolated schema is changed. Friend lookup still succeeds,
	// while the second query fails; the endpoint must not return partial contacts.
	if err := f.DB.Exec("RENAME TABLE `group` TO contact_query_unavailable").Error; err != nil {
		t.Fatal(err)
	}
	if rows, err := friends.SearchFriends2(ctx, uid); err != nil || len(rows) != 1 {
		t.Fatalf("friend query must still succeed: rows=%d err=%v", len(rows), err)
	}
	assertAccountUnavailable(t, contactRequest(GetContacts(queries), body, ctx, uid))
	if err := f.DB.Exec("RENAME TABLE contact_query_unavailable TO `group`").Error; err != nil {
		t.Fatal(err)
	}
	contactData(t, contactRequest(GetContacts(queries), body, ctx, uid), &contacts)
	if len(contacts) != 3 {
		t.Fatalf("restored query returned %d contacts", len(contacts))
	}
	pool, err := f.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"contacts", "friends", "groups", "members"} {
		assertAccountUnavailable(t, contactRequest(contactHandler(action, queries), body, ctx, uid))
	}
}
