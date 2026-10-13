package rabbitmq

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// The controlled peer supports channel.open/close. Exercise actual AMQP RPC
// interruption while keeping default tests independent of management/Docker.
func consumerPeerVerify(ctx context.Context, conn *amqp.Connection) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	return ch.Close()
}

func dialPeerConsumer(t *testing.T, peer *sessionPeer, options ConnectionOptions, verify func(context.Context, *amqp.Connection) error) *ConsumerSession {
	t.Helper()
	s, err := dialConsumer(context.Background(), testSessionURI, options, peer.dial, verify)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(context.Background()) })
	return s
}

func TestConsumerSessionHandshakeAndSetupBounds(t *testing.T) {
	for _, mode := range []string{"unread_handshake", "silent_handshake", "silent_channel_open"} {
		t.Run(mode, func(t *testing.T) {
			peer := newSessionPeer(t, mode, 0)
			o := testSessionOptions().connectionOptions()
			o.OperationTimeout = 100 * time.Millisecond
			started := time.Now()
			s, err := dialConsumer(context.Background(), testSessionURI, o, peer.dial, consumerPeerVerify)
			if err == nil || s != nil {
				t.Fatal("incomplete consumer handshake/setup was accepted")
			}
			if time.Since(started) > time.Second {
				t.Fatal("consumer initialization was not bounded")
			}
			if peer.socket.count.Load() != 1 {
				t.Fatal("failed initialization did not close its socket exactly once")
			}
		})
	}
	t.Run("topology_error_is_sanitized", func(t *testing.T) {
		peer := newSessionPeer(t, "healthy", 0)
		s, err := dialConsumer(context.Background(), testSessionURI, testSessionOptions().connectionOptions(), peer.dial, func(context.Context, *amqp.Connection) error {
			return errors.New("SECRET: original topology error")
		})
		if s != nil || !errors.Is(err, ErrConsumerTopology) || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("unexpected error: %v", err)
		}
		if peer.socket.count.Load() != 1 {
			t.Fatal("topology failure leaked socket")
		}
	})
}

func TestConsumerSessionRepeatedVerifyAndInitializationContext(t *testing.T) {
	peer := newSessionPeer(t, "healthy", 0)
	ctx, cancel := context.WithCancel(context.Background())
	s, err := dialConsumer(ctx, testSessionURI, testSessionOptions().connectionOptions(), peer.dial, consumerPeerVerify)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(context.Background()) })
	// Initialization context is not the returned connection's lifetime owner.
	cancel()
	for i := 0; i < 3; i++ {
		if err := s.Verify(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-s.Done():
		t.Fatal("completed initialization/check watchdog aborted healthy consumer")
	default:
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal("repeat close changed result", err)
	}
	if err := s.Verify(context.Background()); !errors.Is(err, ErrConsumerUnavailable) {
		t.Fatal("closed consumer reused", err)
	}
	if peer.socket.count.Load() != 1 {
		t.Fatal("repeat close duplicated socket cleanup")
	}
}

func TestConsumerSessionProducerWriteStallIsolation(t *testing.T) {
	producerPeer := newSessionPeer(t, "blocked_write", 1)
	o := testSessionOptions()
	o.OperationTimeout, o.WriteTimeout = 2*time.Second, 2*time.Second
	producer := dialPeerSession(t, producerPeer, o)
	consumerPeer := newSessionPeer(t, "healthy", 0)
	consumer := dialPeerConsumer(t, consumerPeer, o.connectionOptions(), consumerPeerVerify)
	if producer.connectionOwner == consumer.owner || producer.transport == consumer.owner.transport || producer.connection == consumer.owner.connection {
		t.Fatal("producer and consumer share connection ownership")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan PublishResult, 1)
	producerPeer.socket.trackWrites.Store(true)
	command := publisherCommand(t)
	go func() { r, _ := producer.Publish(ctx, command, time.Now()); done <- r }()
	select {
	case <-producerPeer.socket.writeEvents:
	case <-time.After(time.Second):
		t.Fatal("publisher never entered blocked socket write")
	}
	if err := consumer.Verify(context.Background()); err != nil {
		t.Fatal("producer write stall blocked consumer RPC", err)
	}
	select {
	case <-done:
		t.Fatal("producer was not still blocked during consumer verification")
	default:
	}
	cancel()
	select {
	case r := <-done:
		if r.Outcome != PublishUnknown {
			t.Fatal("unfinished publisher result was definite", r)
		}
	case <-time.After(time.Second):
		t.Fatal("publisher cancellation did not join")
	}
	if err := consumer.Verify(context.Background()); err != nil {
		t.Fatal("publisher abort retired consumer", err)
	}
	select {
	case <-consumer.Done():
		t.Fatal("consumer admission closed with producer")
	default:
	}
}

func TestConsumerSessionVerifyFailureDoesNotRetireProducer(t *testing.T) {
	producerPeer := newSessionPeer(t, "healthy", 1)
	producer := dialPeerSession(t, producerPeer, testSessionOptions())
	consumerPeer := newSessionPeer(t, "silent_channel_open", 0)
	var checks atomic.Int32
	o := testSessionOptions().connectionOptions()
	o.OperationTimeout = 100 * time.Millisecond
	consumer := dialPeerConsumer(t, consumerPeer, o, func(ctx context.Context, conn *amqp.Connection) error {
		if checks.Add(1) == 1 {
			return nil
		}
		return consumerPeerVerify(ctx, conn)
	})
	started := time.Now()
	if err := consumer.Verify(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled verification: %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("consumer verify RPC did not interrupt")
	}
	select {
	case <-consumer.Done():
	default:
		t.Fatal("failed consumer stayed admissible")
	}
	if r, err := producer.Publish(context.Background(), publisherCommand(t), time.Now()); err != nil || r.Outcome != PublishAccepted {
		t.Fatal("consumer failure retired producer", r, err)
	}
	if err := consumer.Verify(context.Background()); !errors.Is(err, ErrConsumerUnavailable) {
		t.Fatal("failed consumer reused", err)
	}
	if checks.Load() != 2 || consumerPeer.socket.count.Load() != 1 {
		t.Fatal("failed consumer retried or duplicated cleanup")
	}
}

func TestConsumerSessionBusyAndPrecancelDoNotAbortOtherWork(t *testing.T) {
	peer := newSessionPeer(t, "healthy", 0)
	entered, release := make(chan struct{}), make(chan struct{})
	var checks atomic.Int32
	s := dialPeerConsumer(t, peer, testSessionOptions().connectionOptions(), func(ctx context.Context, conn *amqp.Connection) error {
		if checks.Add(1) == 2 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return consumerPeerVerify(ctx, conn)
	})
	done := make(chan error, 1)
	go func() { done <- s.Verify(context.Background()) }()
	<-entered
	if err := s.Verify(context.Background()); !errors.Is(err, ErrConsumerBusy) {
		t.Fatal("parallel verification was admitted", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Verify(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("pre-cancelled check was admitted", err)
	}
	if err := s.Verify(nil); !errors.Is(err, ErrConsumerConfig) {
		t.Fatal("nil context accepted", err)
	}
	if peer.socket.count.Load() != 0 {
		t.Fatal("zero-write check interrupted active verification")
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("healthy verification did not finish")
	}
	if err := s.Verify(context.Background()); err != nil {
		t.Fatal("completed watchdog aborted later verification", err)
	}
}

func TestConsumerSessionConcurrentCloseInterruptsVerification(t *testing.T) {
	peer := newSessionPeer(t, "silent_channel_open", 0)
	var checks atomic.Int32
	o := testSessionOptions().connectionOptions()
	o.OperationTimeout = 2 * time.Second
	s := dialPeerConsumer(t, peer, o, func(ctx context.Context, conn *amqp.Connection) error {
		if checks.Add(1) == 1 {
			return nil
		}
		return consumerPeerVerify(ctx, conn)
	})
	peer.socket.trackWrites.Store(true)
	done := make(chan error, 1)
	go func() { done <- s.Verify(context.Background()) }()
	select {
	case <-peer.socket.writeEvents:
	case <-time.After(time.Second):
		t.Fatal("consumer verification never entered real transport")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = s.Close(ctx) }()
	}
	wg.Wait()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("shutdown verification reported ready")
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown abandoned verification RPC")
	}
	select {
	case <-s.owner.quiescent():
	default:
		t.Fatal("closed connection retained active check")
	}
	if peer.socket.count.Load() != 1 {
		t.Fatal("concurrent Close duplicated socket cleanup")
	}
}

func TestConsumerSessionCloseResponseMissingAndIdleEOF(t *testing.T) {
	for _, mode := range []string{"silent_connection_close", "healthy"} {
		t.Run(mode, func(t *testing.T) {
			peer := newSessionPeer(t, mode, 0)
			s := dialPeerConsumer(t, peer, testSessionOptions().connectionOptions(), consumerPeerVerify)
			if mode == "healthy" {
				peer.server.Close()
				select {
				case <-s.Done():
				case <-time.After(time.Second):
					t.Fatal("idle consumer EOF did not stop admission")
				}
				if err := s.Verify(context.Background()); !errors.Is(err, ErrConsumerUnavailable) {
					t.Fatal("idle disconnected consumer reused", err)
				}
			}
			started := time.Now()
			err := s.Close(context.Background())
			if mode == "silent_connection_close" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("missing close response reported graceful completion", err)
			}
			if time.Since(started) > time.Second {
				t.Fatal("consumer shutdown exceeded close bound")
			}
			if peer.socket.count.Load() != 1 {
				t.Fatal("consumer socket not closed exactly once")
			}
		})
	}
}

func TestConsumerSessionConfigurationRejectsBeforeDial(t *testing.T) {
	m, err := NewManagement("http://127.0.0.1:1", "observer", "SECRET")
	if err != nil {
		t.Fatal(err)
	}
	o := DefaultConnectionOptions()
	for _, tc := range []struct {
		name, address, vhost string
		options              ConnectionOptions
		management           *Management
	}{
		{"no_management", testSessionURI, "test", o, nil},
		{"empty_management", testSessionURI, "test", o, &Management{}},
		{"bad_vhost", testSessionURI, "different", o, m},
		{"bad_uri", "amqp://SECRET@%xx", "test", o, m},
		{"zero_options", testSessionURI, "test", ConnectionOptions{}, m},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := DialConsumer(context.Background(), tc.address, tc.management, tc.vhost, tc.options)
			if s != nil || !errors.Is(err, ErrConsumerConfig) || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("invalid config: %v", err)
			}
		})
	}
	peer := newSessionPeer(t, "healthy", 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s, err := dialConsumer(ctx, testSessionURI, o, peer.dial, consumerPeerVerify)
	if s != nil || !errors.Is(err, context.Canceled) || peer.socket.count.Load() != 0 {
		t.Fatal("pre-cancelled initialization performed transport work", err)
	}
}
