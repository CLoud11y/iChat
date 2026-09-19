package database

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
)

func TestSubscriptionUnavailable(t *testing.T) {
	ctx := context.Background()
	for _, store := range []*msgManager{nil, NewMessageManager(nil, nil)} {
		for _, subscribe := range []func(context.Context, uint) (Subscription, error){store.Subscribe, store.SubscribeGroups} {
			if sub, err := subscribe(ctx, 1); sub != nil || !errors.Is(err, ErrStorageUnavailable) {
				t.Fatalf("missing dependency sub=%v err=%v", sub, err)
			}
		}
	}
	cause := errors.New("dial refused")
	rds := redis.NewClient(&redis.Options{MaxRetries: -1, Dialer: func(context.Context, string, string) (net.Conn, error) { return nil, cause }})
	defer rds.Close()
	store := NewMessageManager(nil, rds)
	if sub, err := store.Subscribe(ctx, 1); sub != nil || !errors.Is(err, cause) || !errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("handshake failure sub=%v err=%v", sub, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if sub, err := store.Subscribe(canceled, 1); sub != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled sub=%v err=%v", sub, err)
	}
	if sub, err := store.SubscribeGroups(ctx, 1); sub != nil || !errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("nil DB sub=%v err=%v", sub, err)
	}
	if err := rds.Close(); err != nil {
		t.Fatal(err)
	}
	if sub, err := store.Subscribe(ctx, 1); sub != nil || !errors.Is(err, redis.ErrClosed) {
		t.Fatalf("closed Redis sub=%v err=%v", sub, err)
	}
}

func TestEmptySubscriptionClose(t *testing.T) {
	store := NewMessageManager(nil, nil)
	sub, err := store.subscribeChannels(context.Background(), nil)
	if err != nil || sub.Channel() != nil {
		t.Fatalf("empty subscription: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := sub.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

func TestSubscriptionCancelInterruptsHandshake(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	read := make(chan struct{})
	started := make(chan struct{})
	go func() {
		defer close(read)
		buffer := make([]byte, 4096)
		if _, err := server.Read(buffer); err != nil {
			return
		}
		close(started)
		_, _ = io.Copy(io.Discard, server)
	}()
	rds := redis.NewClient(&redis.Options{MaxRetries: -1, Dialer: func(context.Context, string, string) (net.Conn, error) { return client, nil }})
	defer rds.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		sub, err := NewMessageManager(nil, rds).Subscribe(ctx, 1)
		if sub != nil {
			_ = sub.Close()
		}
		done <- err
	}()
	// The peer accepts bytes but never sends SUBSCRIBE confirmation.
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("subscription command not sent")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel error=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handshake ignored cancellation")
	}
	_ = client.Close()
	select {
	case <-read:
	case <-time.After(time.Second):
		t.Fatal("pipe reader leaked")
	}
}
