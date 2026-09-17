package database

import (
	"context"
	"errors"
	"testing"

	"gorm.io/gorm"
)

func TestUserRepositoryMissingDatabase(t *testing.T) {
	for _, users := range []*userManager{nil, NewUserManager(nil), NewUserManager(&gorm.DB{})} {
		if user, err := users.GetUserByPhone(context.Background(), "phone"); user != nil || !errors.Is(err, ErrStorageUnavailable) {
			t.Fatalf("missing DB lookup: user=%v err=%v", user, err)
		}
		if user, err := users.Signup(context.Background(), "phone", "name", "hash"); user != nil || !errors.Is(err, ErrStorageUnavailable) {
			t.Fatalf("missing DB signup: user=%v err=%v", user, err)
		}
		users.Offline(1)
	}
}
