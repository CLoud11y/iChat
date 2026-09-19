//go:build integration

package service

import (
	"context"
	"errors"
	"fmt"
	"iChat/database"
	"iChat/models"
	"iChat/tests/testenv"
	"net/http"
	"strings"
	"testing"

	"gorm.io/gorm"
)

func TestContactWritesMySQLIntegration(t *testing.T) {
	f := testenv.NewLegacyFixture(t)
	friends, groups := database.NewRelationManager(f.DB), database.NewGroupManager(f.DB)
	writes := ContactWrites{Friends: friends, Groups: groups}
	ctx := context.Background()
	uid, target := f.Users[4].ID, f.Users[0]
	oldFriends, oldGroups, oldUsers := database.Rmanager, database.Gmanager, database.Umanager
	database.Rmanager, database.Gmanager, database.Umanager = nil, nil, nil
	t.Cleanup(func() { database.Rmanager, database.Gmanager, database.Umanager = oldFriends, oldGroups, oldUsers })
	request := func(action, body string, user uint, wantCode int) {
		t.Helper()
		response := contactRequest(contactWriteHandler(action, writes), body, ctx, user)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), fmt.Sprintf(`"code":%d`, wantCode)) {
			t.Fatalf("%s: status=%d body=%s", action, response.Code, response.Body)
		}
	}
	count := func(model interface{}, where string, args ...interface{}) int64 {
		t.Helper()
		var n int64
		if err := f.DB.Unscoped().Model(model).Where(where, args...).Count(&n).Error; err != nil {
			t.Fatal(err)
		}
		return n
	}
	friendBody := fmt.Sprintf(`{"targetPhone":%q}`, target.Phone)
	request("friend", friendBody, uid, 0)
	if n := count(&models.Relation{}, "type = ? AND ((owner_id = ? AND target_id = ?) OR (owner_id = ? AND target_id = ?))", models.FriendRelation, uid, target.ID, target.ID, uid); n != 2 {
		t.Fatalf("friend rows=%d", n)
	}
	if err := friends.AddFriendByPhone(ctx, uid, target.Phone); !errors.Is(err, database.ErrRelationExists) {
		t.Fatalf("duplicate friend: %v", err)
	}
	request("friend", friendBody, uid, -1)
	if err := friends.AddFriendByPhone(ctx, uid, f.Users[4].Phone); !errors.Is(err, database.ErrSelfFriend) {
		t.Fatalf("self friend: %v", err)
	}
	if err := friends.AddFriendByPhone(ctx, uid, "missing-phone"); !errors.Is(err, database.ErrUserNotFound) {
		t.Fatalf("missing friend: %v", err)
	}

	request("create", `{"name":"created","desc":"fixture"}`, uid, 0)
	var created models.Group
	if err := f.DB.Where("name = ?", "created").First(&created).Error; err != nil {
		t.Fatal(err)
	}
	if created.OwnerId != uid || created.Desc != "fixture" || created.MaxUser != database.MAX_GROUP_USER {
		t.Fatalf("incorrect group: %+v", created)
	}
	if n := count(&models.Relation{}, "owner_id = ? AND target_id = ? AND type = ?", uid, created.ID, models.GroupRelation); n != 1 {
		t.Fatalf("owner membership=%d", n)
	}
	groupBody := fmt.Sprintf(`{"groupId":"%d"}`, created.ID)
	request("join", groupBody, target.ID, 0)
	if n := count(&models.Relation{}, "owner_id = ? AND target_id = ? AND type = ?", target.ID, created.ID, models.GroupRelation); n != 1 {
		t.Fatalf("joined membership=%d", n)
	}
	if err := groups.JoinGroup(ctx, target.ID, created.ID); !errors.Is(err, database.ErrRelationExists) {
		t.Fatalf("duplicate join: %v", err)
	}
	request("join", groupBody, target.ID, -1)
	request("delete", groupBody, target.ID, -1)
	if err := f.DB.First(&models.Group{}, created.ID).Error; err != nil {
		t.Fatalf("non-owner changed group: %v", err)
	}
	request("delete", groupBody, uid, 0)
	if err := f.DB.First(&models.Group{}, created.ID).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("deleted group still visible: %v", err)
	}
	request("delete", groupBody, uid, -1)

	// Reject only a new relation for this owner. Existing fixture rows satisfy
	// the constraint; all DDL is confined to this run's schema.
	if err := f.DB.Exec(fmt.Sprintf("ALTER TABLE relation ADD CONSTRAINT contact_reject_owner CHECK (owner_id <> %d OR target_id <= %d)", uid, created.ID)).Error; err != nil {
		t.Fatal(err)
	}
	assertAccountUnavailable(t, contactRequest(CreateGroup(groups), `{"name":"must-rollback"}`, ctx, uid))
	if n := count(&models.Group{}, "name = ?", "must-rollback"); n != 0 {
		t.Fatalf("failed group left %d rows", n)
	}
	if n := count(&models.Relation{}, "owner_id = ? AND target_id > ?", uid, created.ID); n != 0 {
		t.Fatalf("failed group left %d memberships", n)
	}
	if err := f.DB.Exec("ALTER TABLE relation DROP CHECK contact_reject_owner").Error; err != nil {
		t.Fatal(err)
	}

	// Reject the reverse side of a new friendship to check all-or-nothing writes.
	other := f.Users[2]
	if err := f.DB.Exec(fmt.Sprintf("ALTER TABLE relation ADD CONSTRAINT contact_reject_reverse CHECK (NOT (owner_id = %d AND target_id = %d AND type = %d))", other.ID, uid, models.FriendRelation)).Error; err != nil {
		t.Fatal(err)
	}
	assertAccountUnavailable(t, contactRequest(AddFriend(friends), fmt.Sprintf(`{"targetPhone":%q}`, other.Phone), ctx, uid))
	if n := count(&models.Relation{}, "type = ? AND ((owner_id = ? AND target_id = ?) OR (owner_id = ? AND target_id = ?))", models.FriendRelation, uid, other.ID, other.ID, uid); n != 0 {
		t.Fatalf("failed friendship left %d rows", n)
	}
	if err := f.DB.Exec("ALTER TABLE relation DROP CHECK contact_reject_reverse").Error; err != nil {
		t.Fatal(err)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	beforeGroups, beforeRelations := count(&models.Group{}, "1 = 1"), count(&models.Relation{}, "1 = 1")
	for name, err := range map[string]error{
		"friend":   friends.AddFriendByPhone(canceled, uid, other.Phone),
		"relation": friends.AddGroupRelation(canceled, uid, f.Groups[1].ID),
		"create":   groups.CreateGroup(canceled, "canceled", uid, ""),
		"join":     groups.JoinGroup(canceled, uid, f.Groups[1].ID),
		"delete":   groups.DeleteGroup(canceled, target.ID, f.Groups[0].ID),
	} {
		if !errors.Is(err, context.Canceled) || !errors.Is(err, database.ErrStorageUnavailable) {
			t.Fatalf("%s cancel: %v", name, err)
		}
	}
	if count(&models.Group{}, "1 = 1") != beforeGroups || count(&models.Relation{}, "1 = 1") != beforeRelations {
		t.Fatal("canceled requests inserted rows")
	}
	if err := f.DB.First(&models.Group{}, f.Groups[0].ID).Error; err != nil {
		t.Fatalf("canceled delete changed group: %v", err)
	}

	// A real SQL execution failure in delete must not be reported as success.
	if err := f.DB.Exec("RENAME TABLE `group` TO contact_write_unavailable").Error; err != nil {
		t.Fatal(err)
	}
	assertAccountUnavailable(t, contactRequest(DeleteGroup(groups), fmt.Sprintf(`{"groupId":"%d"}`, f.Groups[0].ID), ctx, target.ID))
	if err := f.DB.Exec("RENAME TABLE contact_write_unavailable TO `group`").Error; err != nil {
		t.Fatal(err)
	}
	pool, err := f.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"friend", "create", "join", "delete"} {
		assertAccountUnavailable(t, contactRequest(contactWriteHandler(action, writes), contactWriteBody, ctx, uid))
	}
}
