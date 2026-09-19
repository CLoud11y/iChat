package database

import (
	"context"
	"errors"
	"testing"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

func TestContactWritesMissingDatabase(t *testing.T) {
	ctx := context.Background()
	for _, relations := range []*relationManager{nil, NewRelationManager(nil), NewRelationManager(&gorm.DB{})} {
		for _, err := range []error{relations.AddFriendByPhone(ctx, 1, "phone"), relations.AddGroupRelation(ctx, 1, 2)} {
			if !errors.Is(err, ErrStorageUnavailable) {
				t.Fatalf("missing relation DB: %v", err)
			}
		}
	}
	for _, groups := range []*groupManager{nil, NewGroupManager(nil), NewGroupManager(&gorm.DB{})} {
		for _, err := range []error{groups.CreateGroup(ctx, "name", 1, "desc"), groups.JoinGroup(ctx, 1, 2), groups.DeleteGroup(ctx, 1, 2)} {
			if !errors.Is(err, ErrStorageUnavailable) {
				t.Fatalf("missing group DB: %v", err)
			}
		}
	}
}

func TestRelationWriteError(t *testing.T) {
	for _, err := range []error{gorm.ErrDuplicatedKey, &mysqlDriver.MySQLError{Number: 1062}} {
		if got := relationWriteError("insert", err); !errors.Is(got, ErrRelationExists) {
			t.Fatalf("duplicate=%v", got)
		}
	}
	cause := errors.New("write failed")
	if got := relationWriteError("insert", cause); !errors.Is(got, cause) || !errors.Is(got, ErrStorageUnavailable) {
		t.Fatalf("write=%v", got)
	}
}
