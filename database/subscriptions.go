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
}

func (s *messageSubscription) Channel() <-chan *redis.Message { return s.messages }
func (s *messageSubscription) Close() error {
	s.once.Do(func() {
		if s.sub != nil {
			s.err = s.sub.Close()
		}
		close(s.done)
	})
	return s.err
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
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Close()
		case <-s.done:
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
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
		if !ok || confirmation.Kind != "subscribe" {
			_ = s.Close()
			return nil, storageError("confirm subscriptions", fmt.Errorf("unexpected subscription confirmation"))
		}
	}
	if err := ctx.Err(); err != nil {
		_ = s.Close()
		return nil, storageError("subscribe messages", err)
	}
	s.messages = s.sub.Channel()
	return s, nil
}
