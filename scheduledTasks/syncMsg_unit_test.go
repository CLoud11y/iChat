package scheduledtasks

import (
	"context"
	"errors"
	"iChat/database"
	"reflect"
	"testing"
)

type syncStore struct {
	ctx            context.Context
	msgs           []string
	size           int
	stage          string
	cause          error
	calls          []string
	saved          []string
	acked          []string
	completed      int
	ackErr         error
	resultOverride *int
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
func (s *syncStore) ProcessDirtyMsgs(ctx context.Context, msgs []string) (int, error) {
	s.saved = msgs
	if s.resultOverride != nil {
		return *s.resultOverride, s.call(ctx, "save")
	}
	if err := s.call(ctx, "save"); err != nil {
		return s.completed, err
	}
	return len(msgs), nil
}
func (s *syncStore) RemDirtyMsgs(ctx context.Context, prefix []string) error {
	s.acked = prefix
	if s.ackErr != nil {
		s.calls = append(s.calls, "ack")
		return s.ackErr
	}
	return s.call(ctx, "ack")
}

func TestSyncMsgInvalidProgressAndCombinedErrors(t *testing.T) {
	ctx := context.Background()
	for _, n := range []int{-1, 1, 3} {
		store := &syncStore{ctx: ctx, msgs: []string{"one", "two"}, size: 2, resultOverride: &n}
		if err := SyncMsg(ctx, store); err == nil || store.acked != nil {
			t.Fatalf("invalid processed count %d acknowledged: %v", n, err)
		}
	}
	processErr, ackErr := errors.New("isolation unavailable"), errors.New("ack unavailable")
	store := &syncStore{ctx: ctx, msgs: []string{"done", "failed", "tail"}, size: 3, stage: "save", cause: processErr, completed: 1, ackErr: ackErr}
	if err := SyncMsg(ctx, store); !errors.Is(err, processErr) || !errors.Is(err, ackErr) {
		t.Fatalf("lost failure cause: %v", err)
	}
	if !reflect.DeepEqual(store.acked, []string{"done"}) {
		t.Fatalf("incorrect prefix=%v", store.acked)
	}
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
		{"partial", []string{"read", "save", "ack"}},
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
			case "partial":
				store.stage, store.completed = "save", 1
			case "canceled":
				cancel()
			}
			err := SyncMsg(ctx, store)
			switch tc.name {
			case "success", "empty":
				if err != nil {
					t.Fatal(err)
				}
			case "read", "save", "ack", "partial":
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
			wantAck := store.msgs
			if tc.name == "partial" {
				wantAck = wantAck[:1]
			}
			if len(store.calls) == 3 && !reflect.DeepEqual(store.acked, wantAck) {
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
