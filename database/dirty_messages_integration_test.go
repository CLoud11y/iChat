//go:build integration

package database

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"iChat/tests/testenv"
)

func TestDirtyMessageBatchIntegration(t *testing.T) {
	f := testenv.NewLegacyFixture(t)
	store := NewMessageManager(nil, f.Redis)
	ctx := context.Background()
	key := getDirtyMsgsKey()
	reset := func(t *testing.T, values []string) {
		t.Helper()
		if err := f.Redis.Del(ctx, key).Err(); err != nil {
			t.Fatal(err)
		}
		for _, value := range values {
			if err := f.Redis.RPush(ctx, key, value).Err(); err != nil {
				t.Fatal(err)
			}
		}
	}
	assertQueue := func(t *testing.T, want []string) {
		t.Helper()
		got, err := f.Redis.LRange(ctx, key, 0, -1).Result()
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("queue=%q want=%q err=%v", got, want, err)
		}
	}
	ack := func(batch []string) error { return store.RemDirtyMsgs(ctx, batch) }
	t.Run("bounded_read", func(t *testing.T) {
		values := make([]string, 130)
		for i := range values {
			values[i] = fmt.Sprintf("raw-%03d", i)
		}
		reset(t, values)
		batch, count, err := store.GetAllDirtyMsgs(ctx)
		if err != nil || count != 128 || !reflect.DeepEqual(batch, values[:128]) {
			t.Fatalf("batch size=%d count=%d err=%v", len(batch), count, err)
		}
		if err := ack(batch); err != nil {
			t.Fatal(err)
		}
		assertQueue(t, values[128:])
	})
	t.Run("append_after_read", func(t *testing.T) {
		reset(t, []string{"first", "second"})
		batch, _, err := store.GetAllDirtyMsgs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		appended := make(chan error, 1)
		go func() { appended <- f.Redis.RPush(ctx, key, "new-tail").Err() }()
		if err := <-appended; err != nil {
			t.Fatal(err)
		}
		if err := ack(batch); err != nil {
			t.Fatal(err)
		}
		assertQueue(t, []string{"new-tail"})
	})
	t.Run("stale_batch", func(t *testing.T) {
		reset(t, []string{"first", "second", "uncommitted"})
		batch, _, err := store.GetAllDirtyMsgs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := ack(batch[:2]); err != nil {
			t.Fatal(err)
		}
		if err := ack(batch[:2]); !errors.Is(err, ErrDirtyMessagePrefixMismatch) {
			t.Errorf("stale acknowledgement: %v", err)
		}
		assertQueue(t, []string{"uncommitted"})
	})
	for _, tc := range []struct {
		name       string
		queue, ack []string
	}{
		{"later_mismatch", []string{"first", "changed", "tail"}, []string{"first", "second"}},
		{"shorter_queue", []string{"first"}, []string{"first", "second"}},
		{"missing_queue", []string{}, []string{"first"}},
		{"exact_raw_bytes", []string{`{ "id":"one" }`, "tail"}, []string{`{"id":"one"}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reset(t, tc.queue)
			if err := ack(tc.ack); !errors.Is(err, ErrDirtyMessagePrefixMismatch) {
				t.Errorf("mismatched acknowledgement: %v", err)
			}
			assertQueue(t, tc.queue)
		})
	}
	t.Run("binary_and_empty_records", func(t *testing.T) {
		values := []string{"", "raw\x00bytes\r\n", "tail"}
		reset(t, values)
		if err := ack(values[:2]); err != nil {
			t.Fatal(err)
		}
		assertQueue(t, values[2:])
	})
	t.Run("concurrent_ack_and_append", func(t *testing.T) {
		for i := 0; i < 8; i++ {
			reset(t, []string{"first", "second", "uncommitted"})
			start := make(chan struct{})
			results := make(chan error, 2)
			appended := make(chan error, 1)
			var wg sync.WaitGroup
			for n := 0; n < 2; n++ {
				wg.Add(1)
				go func() { defer wg.Done(); <-start; results <- ack([]string{"first", "second"}) }()
			}
			wg.Add(1)
			go func() { defer wg.Done(); <-start; appended <- f.Redis.RPush(ctx, key, "new-tail").Err() }()
			close(start)
			wg.Wait()
			first, second := <-results, <-results
			if !((first == nil && errors.Is(second, ErrDirtyMessagePrefixMismatch)) || (second == nil && errors.Is(first, ErrDirtyMessagePrefixMismatch))) {
				t.Fatalf("concurrent acknowledgements: %v / %v", first, second)
			}
			if err := <-appended; err != nil {
				t.Fatal(err)
			}
			assertQueue(t, []string{"uncommitted", "new-tail"})
		}
	})
	t.Run("wrong_type", func(t *testing.T) {
		reset(t, nil)
		if err := f.Redis.Set(ctx, key, "wrong-type", 0).Err(); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.GetAllDirtyMsgs(ctx); !errors.Is(err, ErrStorageUnavailable) {
			t.Fatalf("wrong-type read: %v", err)
		}
		if err := ack([]string{"raw"}); !errors.Is(err, ErrStorageUnavailable) {
			t.Fatalf("wrong-type acknowledgement: %v", err)
		}
		if value, err := f.Redis.Get(ctx, key).Result(); err != nil || value != "wrong-type" {
			t.Fatalf("wrong-type queue changed: %q %v", value, err)
		}
	})
}
