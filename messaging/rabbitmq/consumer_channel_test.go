package rabbitmq

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

func consumerWireShort(reader *bytes.Reader) (string, error) {
	n, err := reader.ReadByte()
	if err != nil {
		return "", err
	}
	data := make([]byte, int(n))
	_, err = io.ReadFull(reader, data)
	return string(data), err
}

func consumerWireDelivery(conn net.Conn, channel uint16, tag string, sequence uint64) error {
	method := append(wireShort(tag), make([]byte, 9)...)
	binary.BigEndian.PutUint64(method[len(method)-9:], sequence)
	method = append(method, wireShort(CommandsExchange)...)
	method = append(method, wireShort(SubmitRoutingKey)...)
	if err := wireMethod(conn, channel, 60, 60, method); err != nil {
		return err
	}
	body := []byte(fmt.Sprintf("consumer-body-%d:中🙂\x00", sequence))
	header := make([]byte, 14)
	binary.BigEndian.PutUint16(header, 60)
	binary.BigEndian.PutUint64(header[4:], uint64(len(body)))
	binary.BigEndian.PutUint16(header[12:], 0x9000)
	header = append(header, wireShort("application/json")...)
	header = append(header, 2)
	if err := wireWrite(conn, wireFrame{2, channel, header}); err != nil {
		return err
	}
	return wireWrite(conn, wireFrame{3, channel, body})
}

func consumerMethod(t *testing.T, peer *sessionPeer, method uint16) wireFrame {
	t.Helper()
	select {
	case frame := <-peer.consumerMethods:
		if binary.BigEndian.Uint16(frame.payload[2:]) != method {
			t.Fatalf("unexpected AMQP method, expected %d", method)
		}
		return frame
	case <-time.After(time.Second):
		t.Fatal("consumer wire method was not observed")
	}
	return wireFrame{}
}

func openPeerConsumerChannel(t *testing.T, mode, queue string, o ConnectionOptions) (*ConsumerChannel, *sessionPeer) {
	t.Helper()
	peer := newSessionPeer(t, mode, 0)
	s := dialPeerConsumer(t, peer, o, func(context.Context, *amqp.Connection) error { return nil })
	stream, err := s.OpenConsumer(context.Background(), queue, 2)
	if err != nil {
		t.Fatal(err)
	}
	qos := consumerMethod(t, peer, 10)
	if len(qos.payload) != 11 || binary.BigEndian.Uint32(qos.payload[4:]) != 0 || binary.BigEndian.Uint16(qos.payload[8:]) != 2 || qos.payload[10] != 0 {
		t.Fatal("prefetch was unlimited, size-based or global")
	}
	consume := consumerMethod(t, peer, 20)
	reader := bytes.NewReader(consume.payload[6:])
	gotQueue, _ := consumerWireShort(reader)
	gotTag, _ := consumerWireShort(reader)
	flags, err := reader.ReadByte()
	if err != nil || flags != 0 || gotQueue != queue || gotTag != stream.consumerTag {
		t.Fatal("consume was autoAck/exclusive/noWait or had wrong identity")
	}
	t.Cleanup(func() {
		_ = stream.Close(context.Background())
		select {
		case <-stream.monitorDone:
		case <-time.After(time.Second):
			t.Error("consumer monitor did not exit")
		}
	})
	return stream, peer
}

func TestConsumerChannelManualSingleDispositionAndIdentity(t *testing.T) {
	stream, peer := openPeerConsumerChannel(t, "consumer_messages", PersistQueue, testSessionOptions().connectionOptions())
	first, err := stream.Next(context.Background())
	if err != nil || first.Message.DeliveryTag != 1 || first.Message.DeliveryMode != 2 || string(first.Message.Body) != "consumer-body-1:中🙂\x00" {
		t.Fatal("delivery properties/body changed", err)
	}
	if _, err := stream.Next(context.Background()); !errors.Is(err, ErrConsumerBusy) {
		t.Fatal("worker acquired another pending delivery", err)
	}
	select {
	case <-peer.consumerMethods:
		t.Fatal("delivery receipt auto-acknowledged")
	default:
	}
	copy := *first
	for _, value := range []any{first, copy} {
		for _, format := range []string{"%v", "%+v", "%#v"} {
			if strings.Contains(fmt.Sprintf(format, value), "consumer-body") {
				t.Fatal("delivery formatting leaked body")
			}
		}
	}
	first.Message.DeliveryTag = 999 // Public metadata cannot change wire target.
	first.Message.ConsumerTag = "another-worker"
	if err := first.Ack(context.Background()); err != nil {
		t.Fatal(err)
	}
	ack := consumerMethod(t, peer, 80)
	if binary.BigEndian.Uint64(ack.payload[4:]) != 1 || ack.payload[12] != 0 {
		t.Fatal("ACK used mutable tag or multiple=true")
	}
	second, err := stream.Next(context.Background())
	if err != nil || second.Message.DeliveryTag != 2 {
		t.Fatal("second delivery unavailable", err)
	}
	if err := copy.Ack(context.Background()); !errors.Is(err, ErrConsumerDeliverySettled) {
		t.Fatal("copied old handle acknowledged new work", err)
	}
	if err := second.Reject(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	reject := consumerMethod(t, peer, 90)
	if reject.channel != ack.channel || binary.BigEndian.Uint64(reject.payload[4:]) != 2 || reject.payload[12] != 1 {
		t.Fatal("reject used wrong original channel/tag/requeue")
	}
	select {
	case <-peer.consumerMethods:
		t.Fatal("duplicate disposition sent another frame")
	default:
	}
}

func TestConsumerChannelValidationIdleTimeoutAndQuarantineGuard(t *testing.T) {
	stream, peer := openPeerConsumerChannel(t, "consumer_idle", PersistQueue, testSessionOptions().connectionOptions())
	for _, tc := range []struct {
		queue    string
		prefetch int
	}{{"foreign.queue", 1}, {PersistQueue, 0}, {PersistQueue, 257}} {
		if _, err := stream.session.OpenConsumer(context.Background(), tc.queue, tc.prefetch); !errors.Is(err, ErrConsumerConfig) {
			t.Fatal("unsafe channel configuration accepted", err)
		}
	}
	if _, err := stream.session.OpenConsumer(context.Background(), PersistQueue, 1); !errors.Is(err, ErrConsumerBusy) {
		t.Fatal("unbounded second worker channel opened", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := stream.Next(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("idle wait was not bounded", err)
	}
	if peer.socket.count.Load() != 0 {
		t.Fatal("idle wait timeout retired healthy consumer")
	}
	if err := stream.session.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	quarantine, quarantinePeer := openPeerConsumerChannel(t, "consumer_messages", QuarantineQueue, testSessionOptions().connectionOptions())
	delivery, err := quarantine.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := delivery.Reject(context.Background(), false); !errors.Is(err, ErrConsumerConfig) {
		t.Fatal("quarantine audit could be discarded", err)
	}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if err := delivery.Ack(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatal("pre-cancelled ACK was written", err)
	}
	select {
	case <-quarantinePeer.consumerMethods:
		t.Fatal("invalid/pre-cancelled disposition wrote a frame")
	default:
	}
	if err := delivery.Ack(context.Background()); err != nil {
		t.Fatal("invalid calls consumed delivery ownership", err)
	}
	consumerMethod(t, quarantinePeer, 80)
	if err := (&ConsumerDelivery{}).Ack(context.Background()); !errors.Is(err, ErrConsumerDelivery) {
		t.Fatal("forged zero delivery handle accepted", err)
	}
}

func TestConsumerChannelSetupRPCBounds(t *testing.T) {
	for _, mode := range []string{"silent_channel_open", "consumer_silent_qos", "consumer_silent_consume"} {
		t.Run(mode, func(t *testing.T) {
			peer := newSessionPeer(t, mode, 0)
			o := testSessionOptions().connectionOptions()
			o.OperationTimeout = 100 * time.Millisecond
			s := dialPeerConsumer(t, peer, o, func(context.Context, *amqp.Connection) error { return nil })
			started := time.Now()
			if ch, err := s.OpenConsumer(context.Background(), PersistQueue, 1); ch != nil || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("incomplete channel setup became ready", err)
			}
			if time.Since(started) > time.Second || peer.socket.count.Load() != 1 {
				t.Fatal("setup RPC was not interrupted/cleaned up")
			}
		})
	}
}

func TestConsumerChannelBlockedDispositionAndShutdownBounds(t *testing.T) {
	for _, action := range []string{"ack_timeout", "reject_timeout", "close_during_ack"} {
		t.Run(action, func(t *testing.T) {
			o := testSessionOptions().connectionOptions()
			if action == "close_during_ack" {
				o.OperationTimeout, o.WriteTimeout = 2*time.Second, 2*time.Second
			}
			stream, peer := openPeerConsumerChannel(t, "consumer_blocked_ack", PersistQueue, o)
			delivery, err := stream.Next(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			peer.socket.trackWrites.Store(true)
			done := make(chan error, 1)
			go func() {
				if action == "reject_timeout" {
					done <- delivery.Reject(context.Background(), true)
				} else {
					done <- delivery.Ack(context.Background())
				}
			}()
			select {
			case <-peer.socket.writeEvents:
			case <-time.After(time.Second):
				t.Fatal("disposition never entered blocked write")
			}
			copy := *delivery
			if err := copy.Ack(context.Background()); !errors.Is(err, ErrConsumerBusy) {
				t.Fatal("parallel disposition was admitted", err)
			}
			select {
			case <-peer.socket.writeEvents:
				t.Fatal("parallel disposition entered socket write")
			default:
			}
			if action == "close_during_ack" {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
				defer cancel()
				_ = stream.Close(ctx)
			}
			select {
			case err := <-done:
				if !errors.Is(err, ErrConsumerDispositionUnknown) {
					t.Fatal("blocked disposition was definite", err)
				}
			case <-time.After(time.Second):
				t.Fatal("disposition RPC/write was abandoned")
			}
			if peer.socket.count.Load() != 1 {
				t.Fatal("failed disposition did not close its own socket exactly once")
			}
			if err := delivery.Ack(context.Background()); !errors.Is(err, ErrConsumerUnavailable) {
				t.Fatal("unknown disposition was automatically reused", err)
			}
		})
	}
}

func TestConsumerChannelWaitingNextCancellationAndClose(t *testing.T) {
	stream, peer := openPeerConsumerChannel(t, "consumer_idle", PersistQueue, testSessionOptions().connectionOptions())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := stream.Next(ctx); done <- err }()
	waitForGate := func() {
		deadline := time.Now().Add(time.Second)
		for len(stream.gate) == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if len(stream.gate) == 0 {
			t.Fatal("Next did not enter wait")
		}
	}
	waitForGate()
	if _, err := stream.Next(context.Background()); !errors.Is(err, ErrConsumerBusy) {
		t.Fatal("parallel Next was admitted", err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("idle Next cancellation failed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("idle wait was abandoned")
	}
	if peer.socket.count.Load() != 0 {
		t.Fatal("idle cancellation closed healthy consumer")
	}
	go func() { _, err := stream.Next(context.Background()); done <- err }()
	waitForGate()
	if err := stream.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrConsumerUnavailable) {
			t.Fatal("closed Next did not stop", err)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not join idle Next")
	}
	select {
	case <-stream.monitorDone:
	default:
		t.Fatal("close did not join notification monitor")
	}
}

func TestConsumerChannelBrokerCancelAndMalformedDelivery(t *testing.T) {
	for _, mode := range []string{"consumer_cancel", "consumer_bad_tag"} {
		t.Run(mode, func(t *testing.T) {
			peer := newSessionPeer(t, mode, 0)
			s := dialPeerConsumer(t, peer, testSessionOptions().connectionOptions(), func(context.Context, *amqp.Connection) error { return nil })
			stream, err := s.OpenConsumer(context.Background(), PersistQueue, 1)
			if mode == "consumer_cancel" && errors.Is(err, ErrConsumerUnavailable) {
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "consumer_bad_tag" {
				if d, err := stream.Next(context.Background()); d != nil || !errors.Is(err, ErrConsumerDelivery) {
					t.Fatal("zero wire tag was exposed", err)
				}
			}
			select {
			case <-s.Done():
			case <-time.After(time.Second):
				t.Fatal("broker cancellation/bad delivery did not retire worker")
			}
			if err := s.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			select {
			case <-stream.monitorDone:
			default:
				t.Fatal("monitor did not join")
			}
		})
	}
}
