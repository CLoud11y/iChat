package scheduledtasks

import (
	"context"
	"errors"
	"iChat/database"
	"reflect"
	"testing"
)

type syncStore struct {
	ctx   context.Context
	msgs  []string
	size  int
	stage string
	cause error
	calls []string
	saved []string
	acked []string
}

func (s *syncStore) call(ctx context.Context, stage string) error {
	if ctx != s.ctx {
		return errors.New("context was not forwarded")
	}
	s.calls = append(s.calls, stage)
	if stage == s.stage {
		return s.cause
	}
	return ctx.Err()
}
func (s *syncStore) GetAllDirtyMsgs(ctx context.Context) ([]string, int, error) {
	return s.msgs, s.size, s.call(ctx, "read")
}
func (s *syncStore) SaveMsgs2DB(ctx context.Context, msgs []string) error {
	s.saved = msgs
	return s.call(ctx, "save")
}
func (s *syncStore) RemDirtyMsgs(ctx context.Context, prefix []string) error {
	s.acked = prefix
	return s.call(ctx, "ack")
}

func TestSyncMsgStages(t *testing.T) {
	old := database.Mmanager
	database.Mmanager = nil
	t.Cleanup(func() { database.Mmanager = old })
	for _, tc := range []struct {
		name  string
		calls []string
	}{
		{"success", []string{"read", "save", "ack"}},
		{"empty", []string{"read"}},
		{"read", []string{"read"}},
		{"save", []string{"read", "save"}},
		{"ack", []string{"read", "save", "ack"}},
		{"mismatch", []string{"read"}},
		{"negative", []string{"read"}},
		{"oversized", []string{"read"}},
		{"stale", []string{"read", "save", "ack"}},
		{"canceled", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cause := errors.New("injected failure")
			store := &syncStore{ctx: ctx, msgs: []string{"first", "second"}, size: 2, stage: tc.name, cause: cause}
			switch tc.name {
			case "empty":
				store.msgs, store.size = nil, 0
			case "mismatch":
				store.size = 1
			case "negative":
				store.size = -1
			case "oversized":
				store.msgs = make([]string, database.DirtyMessageBatchSize+1)
				store.size = len(store.msgs)
			case "stale":
				store.stage, store.cause = "ack", database.ErrDirtyMessagePrefixMismatch
			case "canceled":
				cancel()
			}
			err := SyncMsg(ctx, store)
			switch tc.name {
			case "success", "empty":
				if err != nil {
					t.Fatal(err)
				}
			case "read", "save", "ack":
				if !errors.Is(err, cause) {
					t.Fatalf("error=%v", err)
				}
			case "canceled":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("error=%v", err)
				}
			case "stale":
				if !errors.Is(err, database.ErrDirtyMessagePrefixMismatch) {
					t.Fatalf("error=%v", err)
				}
			default:
				if err == nil {
					t.Fatal("invalid batch accepted")
				}
			}
			if !reflect.DeepEqual(store.calls, tc.calls) {
				t.Fatalf("calls=%v want=%v", store.calls, tc.calls)
			}
			if len(store.calls) > 1 && !reflect.DeepEqual(store.saved, store.msgs) {
				t.Fatal("batch changed")
			}
			if len(store.calls) == 3 && !reflect.DeepEqual(store.acked, store.msgs) {
				t.Fatal("acknowledgement did not contain the persisted raw prefix")
			}
		})
	}
	for _, store := range []MessageStore{nil, database.NewMessageManager(nil, nil)} {
		if err := SyncMsg(context.Background(), store); !errors.Is(err, database.ErrStorageUnavailable) {
			t.Fatalf("missing store: %v", err)
		}
	}
}
