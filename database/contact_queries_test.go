package database

import (
	"context"
	"errors"
	"testing"

	"gorm.io/gorm"
)

func TestContactQueriesMissingDatabase(t *testing.T) {
	ctx := context.Background()
	for _, relations := range []*relationManager{nil, NewRelationManager(nil), NewRelationManager(&gorm.DB{})} {
		if rows, err := relations.SearchFriends(ctx, 1); rows != nil || !errors.Is(err, ErrStorageUnavailable) {
			t.Fatalf("friends: rows=%v err=%v", rows, err)
		}
		if rows, err := relations.SearchFriends2(ctx, 1); rows != nil || !errors.Is(err, ErrStorageUnavailable) {
			t.Fatalf("joined friends: rows=%v err=%v", rows, err)
		}
	}
	for _, groups := range []*groupManager{nil, NewGroupManager(nil), NewGroupManager(&gorm.DB{})} {
		if rows, err := groups.GetGroupsByUid(ctx, 1); rows != nil || !errors.Is(err, ErrStorageUnavailable) {
			t.Fatalf("groups: rows=%v err=%v", rows, err)
		}
		if rows, err := groups.GetGroupsByUid2(ctx, 1); rows != nil || !errors.Is(err, ErrStorageUnavailable) {
			t.Fatalf("joined groups: rows=%v err=%v", rows, err)
		}
		if rows, err := groups.GetGroupIds(ctx, 1); rows != nil || !errors.Is(err, ErrStorageUnavailable) {
			t.Fatalf("group IDs: rows=%v err=%v", rows, err)
		}
		if rows, err := groups.GetGroupsById(ctx, []uint{1}); rows != nil || !errors.Is(err, ErrStorageUnavailable) {
			t.Fatalf("groups by ID: rows=%v err=%v", rows, err)
		}
		if rows, err := groups.GetGroupUsers(ctx, 1); rows != nil || !errors.Is(err, ErrStorageUnavailable) {
			t.Fatalf("group users: rows=%v err=%v", rows, err)
		}
	}
}
