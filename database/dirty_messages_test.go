package database

import (
	"context"
	"errors"
	"net"
	"reflect"
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
		if err := store.RemDirtyMsgs(ctx, []string{"raw"}); !errors.Is(err, ErrStorageUnavailable) {
			t.Fatalf("ack: %v", err)
		}
	}
}

type dirtyQueueHook struct {
	ctx   context.Context
	cause error
	args  []interface{}
}

func (h *dirtyQueueHook) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	h.args = cmd.Args()
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
	for _, command := range []string{"lrange", "eval"} {
		cause := errors.New("redis unavailable")
		rds := redis.NewClient(&redis.Options{Dialer: func(context.Context, string, string) (net.Conn, error) {
			t.Error("unexpected network dial")
			return nil, cause
		}})
		hook := &dirtyQueueHook{ctx: ctx, cause: cause}
		rds.AddHook(hook)
		store := NewMessageManager(nil, rds)
		var err error
		if command == "lrange" {
			_, _, err = store.GetAllDirtyMsgs(ctx)
		} else {
			err = store.RemDirtyMsgs(ctx, []string{"raw"})
		}
		if !errors.Is(err, cause) || !errors.Is(err, ErrStorageUnavailable) {
			t.Fatalf("%s: %v", command, err)
		}
		want := []interface{}{"lrange", getDirtyMsgsKey(), int64(0), int64(127)}
		if command == "eval" {
			want = []interface{}{"eval", acknowledgeDirtyPrefix, 1, getDirtyMsgsKey(), "raw"}
		}
		if !reflect.DeepEqual(hook.args, want) {
			t.Fatalf("command=%#v want=%#v", hook.args, want)
		}
		if err := rds.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDirtyAcknowledgementRejectsInvalidSize(t *testing.T) {
	rds := redis.NewClient(&redis.Options{Dialer: func(context.Context, string, string) (net.Conn, error) {
		t.Error("invalid acknowledgement reached Redis")
		return nil, errors.New("unexpected dial")
	}})
	defer rds.Close()
	store := NewMessageManager(nil, rds)
	for _, prefix := range [][]string{nil, {}, make([]string, DirtyMessageBatchSize+1)} {
		if err := store.RemDirtyMsgs(context.Background(), prefix); err == nil {
			t.Fatal("invalid prefix accepted")
		}
	}
}
