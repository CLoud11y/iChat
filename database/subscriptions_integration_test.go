//go:build integration

package database

import (
	"context"
	"encoding/json"
	"errors"
	"iChat/models"
	"iChat/tests/testenv"
	"sync"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
)

func TestSubscriptionsIntegration(t *testing.T) {
	f := testenv.NewLegacyFixture(t)
	store := NewMessageManager(f.DB, f.Redis)
	old := Gmanager
	Gmanager = nil
	t.Cleanup(func() { Gmanager = old })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	uid := f.Users[0].ID
	private, err := store.Subscribe(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	defer private.Close()
	groups, err := store.SubscribeGroups(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	defer groups.Close()
	channels := []string{getPrivateChannel(uid), getGroupChannel(f.Groups[0].ID), getGroupChannel(f.Groups[1].ID)}
	assertCount := func(want int64) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			counts, err := f.Redis.PubSubNumSub(context.Background(), channels...).Result()
			if err != nil {
				t.Fatal(err)
			}
			match := true
			for _, channel := range channels {
				if counts[channel] != want {
					match = false
				}
			}
			if match {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("subscriber counts=%v want=%d", counts, want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	assertCount(1)
	for i, channel := range channels {
		kind, target := uint(models.PrivateType), uid
		sub := private
		if i > 0 {
			kind, target = uint(models.GroupType), f.Groups[i-1].ID
			sub = groups
		}
		msg := f.Message(f.Users[1].ID, target, kind, models.TextMedia, "subscription fixture")
		if err := store.PublishMsg(ctx, &msg); err != nil {
			t.Fatal(err)
		}
		select {
		case got, ok := <-sub.Channel():
			if !ok || got == nil || got.Channel != channel {
				t.Fatalf("invalid delivery: %v", got)
			}
			var decoded models.Message
			if err := json.Unmarshal([]byte(got.Payload), &decoded); err != nil || decoded != msg {
				t.Fatalf("payload mismatch: %+v %v", decoded, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("message not delivered")
		}
	}
	empty, err := store.SubscribeGroups(ctx, f.Users[4].ID)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Channel() != nil {
		t.Fatal("no-group user created active channel")
	}
	if err := empty.Close(); err != nil {
		t.Fatal(err)
	}
	if err := empty.Close(); err != nil {
		t.Fatal(err)
	}
	assertCount(1)
	cancel()
	for _, sub := range []Subscription{private, groups} {
		select {
		case _, ok := <-sub.Channel():
			if ok {
				t.Fatal("canceled subscription channel still open")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("cancel did not close channel")
		}
		if err := sub.Close(); err != nil {
			t.Fatal(err)
		}
		if err := sub.Close(); err != nil {
			t.Fatal(err)
		}
	}
	assertCount(0)
	// Manual close must also work when no cancellable context was supplied.
	manual, err := store.Subscribe(context.Background(), uid)
	if err != nil {
		t.Fatal(err)
	}
	if err := manual.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-manual.Channel():
		if ok {
			t.Fatal("manual close left channel open")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("manual close stuck")
	}
	assertCount(0)
	options := *f.Redis.Options()
	// Repeated lifecycles with undelivered notifications must not accumulate
	// Redis subscribers or leave local readers waiting for a consumer.
	for cycle := 0; cycle < 3; cycle++ {
		cycleCtx, stop := context.WithCancel(context.Background())
		sub, err := store.Subscribe(cycleCtx, uid)
		if err != nil {
			stop()
			t.Fatal(err)
		}
		for n := 0; n < 110; n++ {
			if err := f.Redis.Publish(cycleCtx, getPrivateChannel(uid), "unread notification").Err(); err != nil {
				stop()
				_ = sub.Close()
				t.Fatal(err)
			}
		}
		stop()
		var wg sync.WaitGroup
		for n := 0; n < 4; n++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := sub.Close(); err != nil {
					t.Error(err)
				}
			}()
		}
		closed := make(chan struct{})
		go func() { wg.Wait(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(3 * time.Second):
			t.Fatal("concurrent subscription close stuck")
		}
		select {
		case _, ok := <-sub.Channel():
			if ok {
				t.Fatal("closed subscription retained notifications")
			}
		default:
			t.Fatal("subscription channel not closed")
		}
		assertCount(0)
	}
	options.Password = "invalid-fixture-password"
	bad := redis.NewClient(&options)
	defer bad.Close()
	if sub, err := NewMessageManager(f.DB, bad).Subscribe(context.Background(), uid); sub != nil || !errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("authentication failure sub=%v err=%v", sub, err)
	}
	assertCount(0)
}
