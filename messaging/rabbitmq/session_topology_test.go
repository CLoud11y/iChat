package rabbitmq

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

func TestSessionTopologyChecksKeepHealthyPublisherAndSetupLifetime(t *testing.T) {
	p := newSessionPeer(t, "healthy", 1)
	setup, cancelSetup := context.WithCancel(context.Background())
	s, err := dialPreparedPublisher(setup, testSessionURI, testSessionOptions(), p.dial, consumerPeerVerify)
	cancelSetup()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(context.Background()) })
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.verifyTopology(canceled); !errors.Is(err, context.Canceled) {
		t.Fatal("pre-cancelled check ran", err)
	}
	if err := s.verifyTopology(nil); !errors.Is(err, ErrSupervisorTopology) {
		t.Fatal("nil observer accepted", err)
	}
	for i := 0; i < 3; i++ {
		if err := s.verifyTopology(context.Background()); err != nil {
			t.Fatal("healthy connection recheck failed", err)
		}
	}
	if result, err := s.Publish(context.Background(), publisherCommand(t), time.Now()); err != nil || result.Outcome != PublishAccepted || result.DeliveryTag != 1 {
		t.Fatal("verification watchdog or channel changed publishing", result, err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.verifyTopology(context.Background()); !errors.Is(err, ErrPublisherUnavailable) {
		t.Fatal("closed connection verified", err)
	}
}

func TestSessionTopologyPassiveRPCBlackholeAbortsSocket(t *testing.T) {
	p := newSessionPeer(t, "healthy", 1)
	o := testSessionOptions()
	o.OperationTimeout = 60 * time.Millisecond
	s, err := dialPreparedPublisher(context.Background(), testSessionURI, o, p.dial, consumerPeerVerify)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(context.Background()) })
	p.pauseChannelOpen.Store(true)
	started := time.Now()
	if err := s.verifyTopology(context.Background()); !errors.Is(err, ErrSupervisorTopology) || time.Since(started) > time.Second {
		t.Fatal("passive RPC blackhole was not bounded", err)
	}
	awaitTopologyEvent(t, s.Done())
	awaitTopologyEvent(t, s.quiescent())
	if p.socket.count.Load() != 1 {
		t.Fatal("failed recheck did not abort its owned socket exactly once")
	}
	if result, err := s.Publish(context.Background(), publisherCommand(t), time.Now()); !errors.Is(err, ErrPublisherUnavailable) || result.Outcome != PublishNotStarted {
		t.Fatal("failed topology check kept admission", result, err)
	}
}

func TestSessionTopologyFailureMakesUnconfirmedPublishUnknown(t *testing.T) {
	p := newSessionPeer(t, "lost_confirm", 1)
	var checks atomic.Int32
	s, err := dialPreparedPublisher(context.Background(), testSessionURI, testSessionOptions(), p.dial, func(context.Context, *amqp.Connection) error {
		if checks.Add(1) == 1 {
			return nil
		}
		return errors.New("PRIVATE_VERIFY_CAUSE")
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(context.Background()) })
	command := publisherCommand(t)
	result := make(chan PublishResult, 1)
	go func() { r, _ := s.Publish(context.Background(), command, time.Now()); result <- r }()
	var publication wirePublish
	select {
	case publication = <-p.received:
	case <-time.After(time.Second):
		t.Fatal("publish did not reach AMQP peer")
	}
	if err := s.verifyTopology(context.Background()); !errors.Is(err, ErrSupervisorTopology) || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("topology error was not sanitized", err)
	}
	select {
	case r := <-result:
		if r.Outcome != PublishUnknown || r.DeliveryTag != publication.tag || r.PublishAttemptID == "" {
			t.Fatal("unconfirmed publish became accepted or definitely absent", r)
		}
	case <-time.After(time.Second):
		t.Fatal("failed check did not interrupt pending publication")
	}
	awaitTopologyEvent(t, s.quiescent())
	select {
	case <-p.received:
		t.Fatal("topology retirement replayed the command")
	default:
	}
}
