package database

import (
	"context"
	"errors"
	"iChat/models"
	"testing"

	"github.com/go-redis/redis/v8"
)

func TestMessageStoreMissingDependencies(t *testing.T) {
	ctx := context.Background()
	msg := &models.Message{SenderId: 1, ReceiverId: 2, Type: models.PrivateType}
	for _, store := range []*msgManager{nil, NewMessageManager(nil, nil)} {
		for _, err := range []error{store.SaveMsg(ctx, msg), store.PublishMsg(ctx, msg), store.PublishAndSave(ctx, msg)} {
			if !errors.Is(err, ErrStorageUnavailable) {
				t.Fatalf("missing Redis: %v", err)
			}
		}
		if _, err := store.LoadMsgs(ctx, 1, 2, models.PrivateType, models.Message{Type: models.InvalidType}, 10); !errors.Is(err, ErrStorageUnavailable) {
			t.Fatalf("history: %v", err)
		}
		if err := store.SaveMsg(ctx, nil); !errors.Is(err, ErrInvalidMessage) {
			t.Fatalf("nil message: %v", err)
		}
		if _, err := store.LoadMsgs(ctx, 1, 2, 999, models.Message{}, 10); !errors.Is(err, ErrInvalidMessage) {
			t.Fatalf("invalid type: %v", err)
		}
	}
	rds := redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"})
	_ = rds.Close()
	store := NewMessageManager(nil, rds)
	for _, err := range []error{store.SaveMsg(ctx, msg), store.PublishMsg(ctx, msg), store.PublishAndSave(ctx, msg)} {
		if !errors.Is(err, ErrStorageUnavailable) || !errors.Is(err, redis.ErrClosed) {
			t.Fatalf("closed Redis: %v", err)
		}
	}
	if _, err := store.LoadMsgs(ctx, 1, 2, models.PrivateType, models.Message{Type: models.InvalidType}, 10); !errors.Is(err, redis.ErrClosed) || !errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("closed history: %v", err)
	}
}
