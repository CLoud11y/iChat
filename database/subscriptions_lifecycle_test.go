package database

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
)

// Use real go-redis parsing and buffering with a controlled RESP peer.
// Returning from Write proves the client consumed those bytes, without sleeps.
func subscriptionPeer(t *testing.T, frames string) (*redis.Client, <-chan struct{}) {
	t.Helper()
	client, server := net.Pipe()
	written := make(chan struct{})
	finished := make(chan struct{})
	var dialOnce sync.Once
	rds := redis.NewClient(&redis.Options{MaxRetries: -1, WriteTimeout: time.Second,
		Dialer: func(context.Context, string, string) (net.Conn, error) {
			var conn net.Conn
			dialOnce.Do(func() { conn = client })
			if conn == nil {
				return nil, errors.New("fixture already connected")
			}
			return conn, nil
		}})
	go func() {
		defer close(finished)
		defer server.Close()
		buffer := make([]byte, 4096)
		if _, err := server.Read(buffer); err != nil {
			return
		}
		if _, err := io.WriteString(server, frames); err != nil {
			return
		}
		close(written)
		_, _ = io.Copy(io.Discard, server)
	}()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
		_ = rds.Close()
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			t.Error("RESP peer did not exit")
		}
	})
	return rds, written
}

func subscriptionACK(channel string, count int) string {
	return fmt.Sprintf("*3\r\n$9\r\nsubscribe\r\n$%d\r\n%s\r\n:%d\r\n", len(channel), channel, count)
}

func TestSubscriptionConfirmsRequestedChannels(t *testing.T) {
	for _, scenario := range []string{"wrong_channel", "duplicate_channel"} {
		t.Run(scenario, func(t *testing.T) {
			frames := subscriptionACK("private_1", 1)
			if scenario == "wrong_channel" {
				frames += subscriptionACK("private_999", 2)
			} else {
				frames += subscriptionACK("private_1", 2)
			}
			rds, _ := subscriptionPeer(t, frames)
			sub, err := NewMessageManager(nil, rds).subscribeChannels(context.Background(), []string{"private_1", "private_2"})
			if sub != nil {
				_ = sub.Close()
			}
			if sub != nil || !errors.Is(err, ErrStorageUnavailable) {
				t.Fatalf("invalid readiness accepted: sub=%v err=%v", sub, err)
			}
		})
	}
}

func TestSubscriptionFullBufferClose(t *testing.T) {
	for _, scenario := range []string{"manual", "canceled", "concurrent"} {
		t.Run(scenario, func(t *testing.T) { testSubscriptionFullBufferClose(t, scenario) })
	}
}

func testSubscriptionFullBufferClose(t *testing.T, scenario string) {
	t.Helper()
	// More than the dependency's default 100-message buffer. Padding prevents
	// the complete stream fitting in its socket reader's read-ahead buffer.
	payload := strings.Repeat("x", 8192)
	frame := fmt.Sprintf("*3\r\n$7\r\nmessage\r\n$9\r\nprivate_1\r\n$%d\r\n%s\r\n", len(payload), payload)
	rds, written := subscriptionPeer(t, subscriptionACK("private_1", 1)+strings.Repeat(frame, 101))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub, err := NewMessageManager(nil, rds).Subscribe(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	select {
	case <-written:
	case <-time.After(3 * time.Second):
		t.Fatal("buffer fixture not delivered")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		switch scenario {
		case "canceled":
			cancel()
			// Do not call Close or consume messages: cancellation alone must
			// release both our workers and go-redis's blocked reader.
			sub.(*messageSubscription).workers.Wait()
		case "concurrent":
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := sub.Close(); err != nil {
						t.Error(err)
					}
				}()
			}
			cancel()
			wg.Wait()
		default:
			if err := sub.Close(); err != nil {
				t.Error(err)
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("subscription workers did not exit with a full buffer")
	}
	// Close must finish the forwarding reader itself; callers must not need to
	// drain the channel to release it. The old Channel() leaves it blocked here.
	select {
	case _, ok := <-sub.Channel():
		if ok {
			t.Fatal("Close returned with undrained messages and an active reader")
		}
	default:
		t.Fatal("Close returned before the message channel was closed")
	}
}
