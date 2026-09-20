package service

import (
	"context"
	"errors"
	"github.com/sirupsen/logrus"
	"iChat/database"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
)

type testSubscription struct {
	messages chan *redis.Message
	closed   int
}

func (s *testSubscription) Channel() <-chan *redis.Message { return s.messages }
func (s *testSubscription) Close() error                   { s.closed++; return nil }

type subscriptionStub struct {
	private, groups      *testSubscription
	privateErr, groupErr error
	ctx                  context.Context
	uid                  uint
	groupCalls           int
	ready                chan struct{}
}

func (s *subscriptionStub) Subscribe(ctx context.Context, uid uint) (database.Subscription, error) {
	s.ctx, s.uid = ctx, uid
	if s.private == nil {
		return nil, s.privateErr
	}
	return s.private, s.privateErr
}
func (s *subscriptionStub) SubscribeGroups(ctx context.Context, uid uint) (database.Subscription, error) {
	s.groupCalls++
	if s.ctx != ctx || s.uid != uid {
		return nil, errors.New("identity/context mismatch")
	}
	if s.ready != nil {
		close(s.ready)
	}
	if s.groups == nil {
		return nil, s.groupErr
	}
	return s.groups, s.groupErr
}

func TestReceiveSubscriptionCleanup(t *testing.T) {
	for _, scenario := range []string{"private_failure", "private_partial", "private_nil", "group_failure", "group_partial", "group_nil", "private_closed", "group_closed", "nil_message", "canceled", "no_groups", "private_channel_nil"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			private := &testSubscription{messages: make(chan *redis.Message, 1)}
			groups := &testSubscription{messages: make(chan *redis.Message, 1)}
			cause := errors.New("subscription failure")
			stub := &subscriptionStub{private: private, groups: groups, ready: make(chan struct{})}
			wantPrivate, wantGroup := 1, 1
			switch scenario {
			case "private_partial":
				stub.privateErr = cause
				wantGroup = 0
			case "group_partial":
				stub.groupErr = cause
			case "private_failure":
				stub.private = nil
				stub.privateErr = cause
				wantPrivate, wantGroup = 0, 0
			case "private_nil":
				stub.private = nil
				wantPrivate, wantGroup = 0, 0
			case "group_failure":
				stub.groups = nil
				stub.groupErr = cause
				wantGroup = 0
			case "group_nil":
				stub.groups = nil
				wantGroup = 0
			case "private_closed":
				close(private.messages)
			case "group_closed":
				close(groups.messages)
			case "nil_message":
				private.messages <- nil
			case "no_groups":
				groups.messages = nil
			case "private_channel_nil":
				private.messages = nil
				wantGroup = 0
			}
			done := make(chan error, 1)
			go func() { done <- testReceiveSession(ctx, cancel, stub, 42, make(chan websocketOutbound, 1)) }()
			if scenario == "canceled" || scenario == "no_groups" {
				select {
				case <-stub.ready:
				case <-time.After(time.Second):
					t.Fatal("subscription setup stuck")
				}
				cancel()
			}
			select {
			case err := <-done:
				if (scenario == "private_failure" || scenario == "group_failure") && !errors.Is(err, cause) {
					t.Fatalf("cause lost: %v", err)
				}
				if (scenario == "private_nil" || scenario == "group_nil" || scenario == "private_channel_nil") && !errors.Is(err, database.ErrStorageUnavailable) {
					t.Fatalf("missing subscription: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("receive did not exit")
			}
			if private.closed != wantPrivate || groups.closed != wantGroup {
				t.Fatalf("close counts private=%d group=%d", private.closed, groups.closed)
			}
			if ctx.Err() == nil || stub.ctx != ctx || stub.uid != 42 {
				t.Fatal("session not canceled or incorrect arguments")
			}
			if wantPrivate == 0 && stub.groupCalls != 0 {
				t.Fatal("group subscription attempted after private failure")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := testReceiveSession(ctx, cancel, nil, 1, nil); !errors.Is(err, database.ErrStorageUnavailable) {
		t.Fatalf("nil store: %v", err)
	}
}

func testReceiveSession(ctx context.Context, cancel context.CancelFunc, provider MessageSubscriptions, uid uint, outbound chan<- websocketOutbound) error {
	defer cancel()
	owned, err := openSubscriptions(ctx, provider, uid)
	if err != nil {
		return err
	}
	defer owned.Close()
	receiveMessages(ctx, cancel, uid, owned.private.Channel(), owned.groups.Channel(), outbound, logrus.New())
	return nil
}
