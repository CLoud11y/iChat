package database

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/go-redis/redis/v8"
	"gorm.io/gorm"
)

func TestDirtyMessagesMissingDependencies(t *testing.T) {
	ctx := context.Background()
	for _, store := range []*msgManager{nil, NewMessageManager(nil, nil), NewMessageManager(&gorm.DB{}, nil)} {
		if _, _, err := store.GetAllDirtyMsgs(ctx); !errors.Is(err, ErrStorageUnavailable) {
			t.Fatalf("read: %v", err)
		}
		if err := store.SaveMsgs2DB(ctx, []string{`{}`}); !errors.Is(err, ErrStorageUnavailable) {
			t.Fatalf("save: %v", err)
		}
		if err := store.RemDirtyMsgs(ctx, 1); !errors.Is(err, ErrStorageUnavailable) {
			t.Fatalf("ack: %v", err)
		}
	}
}

type dirtyQueueHook struct {
	ctx   context.Context
	cause error
}

func (h *dirtyQueueHook) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	if ctx != h.ctx {
		return ctx, errors.New("context not forwarded")
	}
	return ctx, h.cause
}
func (*dirtyQueueHook) AfterProcess(context.Context, redis.Cmder) error { return nil }
func (*dirtyQueueHook) BeforeProcessPipeline(ctx context.Context, _ []redis.Cmder) (context.Context, error) {
	return ctx, errors.New("unexpected pipeline")
}
func (*dirtyQueueHook) AfterProcessPipeline(context.Context, []redis.Cmder) error { return nil }

// Failure hooks stop commands before a network dial; successful real reads are
// covered by the isolated integration fixture.
func TestDirtyQueueErrorsAreNotEmptySuccess(t *testing.T) {
	ctx := context.Background()
	for _, command := range []string{"llen", "ltrim"} {
		cause := errors.New("redis unavailable")
		rds := redis.NewClient(&redis.Options{Dialer: func(context.Context, string, string) (net.Conn, error) {
			t.Error("unexpected network dial")
			return nil, cause
		}})
		rds.AddHook(&dirtyQueueHook{ctx: ctx, cause: cause})
		store := NewMessageManager(nil, rds)
		var err error
		if command == "llen" {
			_, _, err = store.GetAllDirtyMsgs(ctx)
		} else {
			err = store.RemDirtyMsgs(ctx, 1)
		}
		if !errors.Is(err, cause) || !errors.Is(err, ErrStorageUnavailable) {
			t.Fatalf("%s: %v", command, err)
		}
		if err := rds.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
