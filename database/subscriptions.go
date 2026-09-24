package database

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
)

// Subscription owns its Redis connection. Call Close on every successful
// acquisition, including when a later subscription or session setup fails.
// Close discards pending notifications and waits for owned forwarding tasks.
// An empty group subscription has a nil channel and a no-op Close.
type Subscription interface {
	Channel() <-chan *redis.Message
	Close() error
}

type messageSubscription struct {
	sub      *redis.PubSub
	messages <-chan *redis.Message
	done     chan struct{}
	once     sync.Once
	err      error
	workers  sync.WaitGroup
}

func (s *messageSubscription) Channel() <-chan *redis.Message { return s.messages }
func (s *messageSubscription) Close() error {
	s.stop()
	s.workers.Wait()
	return s.err
}

// stop does not wait: subscription workers may call it themselves.
func (s *messageSubscription) stop() {
	s.once.Do(func() {
		close(s.done)
		if s.sub != nil {
			s.err = s.sub.Close()
		}
	})
}

// forward keeps go-redis's reconnect/health checks, but owns cancellation of
// delivery to our caller. In v8.11.5 the dependency's buffered-channel send
// does not select on PubSub.Close and can otherwise linger for 60 seconds.
func (s *messageSubscription) forward(source <-chan *redis.Message, output chan<- *redis.Message) {
	defer s.workers.Done()
	defer close(output)
	defer func() {
		s.stop()
		// Closing the transport stops new reads. Drain queued sends until the
		// dependency closes source, proving its message reader has exited.
		for range source {
		}
	}()
	for {
		select {
		case <-s.done:
			return
		case msg, ok := <-source:
			if !ok {
				return
			}
			select {
			case <-s.done:
				return
			case output <- msg:
			}
		}
	}
}

func (mm *msgManager) Subscribe(ctx context.Context, uid uint) (Subscription, error) {
	if uid == 0 {
		return nil, ErrInvalidMessage
	}
	if mm == nil || mm.rds == nil {
		return nil, ErrStorageUnavailable
	}
	return mm.subscribeChannels(ctx, []string{getPrivateChannel(uid)})
}

func (mm *msgManager) SubscribeGroups(ctx context.Context, uid uint) (Subscription, error) {
	if uid == 0 {
		return nil, ErrInvalidMessage
	}
	if mm == nil || mm.rds == nil {
		return nil, ErrStorageUnavailable
	}
	groupIDs, err := NewGroupManager(mm.db).GetGroupIds(ctx, uid)
	if err != nil {
		return nil, err
	}
	channels := make([]string, 0, len(groupIDs))
	seen := make(map[uint]bool)
	for _, id := range groupIDs {
		if !seen[id] {
			channels = append(channels, getGroupChannel(id))
			seen[id] = true
		}
	}
	return mm.subscribeChannels(ctx, channels)
}

func (mm *msgManager) subscribeChannels(ctx context.Context, channels []string) (Subscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, storageError("subscribe messages", err)
	}
	s := &messageSubscription{done: make(chan struct{})}
	if len(channels) == 0 {
		return s, nil
	}
	s.sub = mm.rds.Subscribe(ctx, channels...)
	// Closing the connection also interrupts a blocked handshake/read. Manual
	// Close releases this watcher even when the caller uses Background().
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		select {
		case <-ctx.Done():
			s.stop()
		case <-s.done:
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	pending := make(map[string]bool, len(channels))
	for _, channel := range channels {
		pending[channel] = true
	}
	for range channels {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			_ = s.Close()
			return nil, storageError("confirm subscriptions", context.DeadlineExceeded)
		}
		ack, err := s.sub.ReceiveTimeout(ctx, remaining)
		if err != nil {
			_ = s.Close()
			return nil, storageError("confirm subscriptions", errors.Join(err, ctx.Err()))
		}
		confirmation, ok := ack.(*redis.Subscription)
		if !ok || confirmation.Kind != "subscribe" || !pending[confirmation.Channel] {
			_ = s.Close()
			return nil, storageError("confirm subscriptions", fmt.Errorf("unexpected subscription confirmation"))
		}
		delete(pending, confirmation.Channel)
	}
	if err := ctx.Err(); err != nil {
		_ = s.Close()
		return nil, storageError("subscribe messages", err)
	}
	output := make(chan *redis.Message)
	s.messages = output
	s.workers.Add(1)
	go s.forward(s.sub.Channel(), output)
	return s, nil
}
