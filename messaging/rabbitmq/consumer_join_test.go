package rabbitmq

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestConsumerWaitClosedDoesNotConfuseRetirementWithJoin(t *testing.T) {
	idle := make(chan struct{})
	close(idle)
	owner := &connectionOwner{options: DefaultConnectionOptions(), done: make(chan struct{}),
		idle: idle, closeDone: make(chan struct{})}
	s := &ConsumerSession{owner: owner}
	joined := make(chan error, 1)
	go func() { joined <- s.WaitClosed(context.Background()) }()
	// A call can enter after a join observer starts but before Close stops
	// admission. The final idle snapshot must be taken after closeDone.
	if !owner.beginCall() {
		t.Fatal("could not admit controlled transport work")
	}
	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err := s.Close(short)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("close observer concealed outstanding transport work", err)
	}
	select {
	case <-s.Done():
	default:
		t.Fatal("close did not retire admission")
	}
	select {
	case err := <-joined:
		t.Fatal("retirement/close attempt was treated as quiescence", err)
	default:
	}
	short, cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
	if err := s.WaitClosed(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("transport join ignored its observer deadline", err)
	}
	cancel()
	owner.endCall()
	select {
	case err := <-joined:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("quiescent consumer did not join")
	}
	if err := s.WaitClosed(context.Background()); err != nil || s.WaitClosed(nil) != ErrConsumerConfig {
		t.Fatal("completed join was not stable", err)
	}
}
