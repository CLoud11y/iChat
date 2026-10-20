package rabbitmq

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

func quarantinePeers(t *testing.T, peers ...*publisherPeer) *Publisher {
	t.Helper()
	index := 0
	p, err := newTargetPublisher(func() (publishChannel, error) {
		peer := peers[index]
		index++
		if peer.next == 0 {
			peer.next = 1
		}
		return peer, nil
	}, PublisherOptions{Slots: len(peers), ConnectionEpoch: 23}, QuarantineExchange, QuarantineRoutingKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func quarantineReturn(a publishedAttempt) amqp.Return {
	r := returnedAttempt(a)
	r.Type, r.ContentEncoding = a.message.Type, a.message.ContentEncoding
	return r
}

func TestQuarantinePublisherStableSnapshotFreshAttempts(t *testing.T) {
	peer := &publisherPeer{}
	p := quarantinePeers(t, peer)
	q := makeQuarantine(t, quarantineFixture())
	ids := map[string]bool{}
	for i := 1; i <= 3; i++ {
		r, err := p.publishQuarantine(publisherContext(t), q)
		if err != nil || r.Outcome != PublishAccepted || r.TransportID != q.TransportID() || r.DeliveryTag != uint64(i) || r.ConnectionEpoch != 23 || r.ChannelEpoch != 1 {
			t.Fatal("invalid quarantine confirmation", r, err)
		}
		if len(r.PublishAttemptID) != 32 || ids[r.PublishAttemptID] {
			t.Fatal("physical attempt was missing or reused")
		}
		ids[r.PublishAttemptID] = true
		a := peer.attempts[i-1]
		m := a.message
		if a.exchange != QuarantineExchange || a.key != QuarantineRoutingKey || !a.mandatory || a.immediate || m.ContentType != QuarantineContentType || m.Type != QuarantineMessageType || m.DeliveryMode != amqp.Persistent || m.MessageId != q.TransportID() || m.CorrelationId != r.PublishAttemptID || !bytes.Equal(m.Body, q.Encode()) {
			t.Fatal("wrong target or quarantine wire")
		}
		if m.Expiration != "" || m.UserId != "" || m.AppId != "" || m.ReplyTo != "" || len(m.Headers) != 0 || m.Priority != 0 {
			t.Fatal("source properties leaked onto target publication")
		}
		if _, err := DecodeQuarantineEnvelope(m.Body); err != nil {
			t.Fatal(err)
		}
	}
	if peer.registrations != [3]int{1, 1, 1} {
		t.Fatal("raw notification owner was replaced")
	}
}

func TestQuarantinePublisherACKFirstDrainsOriginalReturn(t *testing.T) {
	peer := &publisherPeer{}
	p := quarantinePeers(t, peer)
	q := makeQuarantine(t, quarantineFixture())
	s, err := p.acquire()
	if err != nil {
		t.Fatal(err)
	}
	msg := amqp.Publishing{ContentType: QuarantineContentType, Type: QuarantineMessageType, DeliveryMode: amqp.Persistent,
		MessageId: q.TransportID(), CorrelationId: "target-attempt", Body: q.Encode()}
	peer.onPublish = func(f *publisherPeer, a publishedAttempt) error {
		f.returns <- quarantineReturn(a)
		f.confirms <- amqp.Confirmation{DeliveryTag: a.tag, Ack: true}
		return nil
	}
	if err := peer.PublishWithContext(publisherContext(t), QuarantineExchange, QuarantineRoutingKey, true, false, msg); err != nil {
		t.Fatal(err)
	}
	ack := <-s.confirms
	r, err := p.confirmed(publisherContext(t), s, PublishResult{Outcome: PublishUnknown, DeliveryTag: 1}, msg, ack)
	if !errors.Is(err, ErrPublisherReturn) || r.Outcome != PublishUnroutable || peer.closeCount != 1 {
		t.Fatal("Return+ACK became accepted", r, err)
	}
	if _, err := p.publishQuarantine(publisherContext(t), q); !errors.Is(err, ErrPublisherUnavailable) {
		t.Fatal("returned slot reused")
	}
}

func TestQuarantinePublisherFailuresRetireAndIsolateSlots(t *testing.T) {
	tests := []struct {
		name    string
		outcome PublishOutcome
		cause   error
		fault   func(*publisherPeer, publishedAttempt) error
	}{
		{"return", PublishUnroutable, ErrPublisherReturn, func(f *publisherPeer, a publishedAttempt) error {
			f.returns <- quarantineReturn(a)
			f.confirms <- amqp.Confirmation{DeliveryTag: a.tag, Ack: true}
			return nil
		}},
		{"nack", PublishNacked, ErrPublisherNack, func(f *publisherPeer, a publishedAttempt) error {
			f.confirms <- amqp.Confirmation{DeliveryTag: a.tag}
			return nil
		}},
		{"wrong tag", PublishUnknown, ErrPublisherNotifications, func(f *publisherPeer, a publishedAttempt) error {
			f.confirms <- amqp.Confirmation{DeliveryTag: a.tag + 1, Ack: true}
			return nil
		}},
		{"wrong target", PublishUnknown, ErrPublisherNotifications, func(f *publisherPeer, a publishedAttempt) error {
			r := quarantineReturn(a)
			r.Exchange = CommandsExchange
			f.returns <- r
			return nil
		}},
		{"wrong body", PublishUnknown, ErrPublisherNotifications, func(f *publisherPeer, a publishedAttempt) error {
			r := quarantineReturn(a)
			r.Body = []byte("wrong")
			f.returns <- r
			return nil
		}},
		{"wrong attempt", PublishUnknown, ErrPublisherNotifications, func(f *publisherPeer, a publishedAttempt) error {
			r := quarantineReturn(a)
			r.CorrelationId = "old"
			f.returns <- r
			return nil
		}},
		{"wrong type", PublishUnknown, ErrPublisherNotifications, func(f *publisherPeer, a publishedAttempt) error {
			r := quarantineReturn(a)
			r.Type = "command"
			f.returns <- r
			return nil
		}},
		{"write", PublishUnknown, ErrPublisherWrite, func(f *publisherPeer, a publishedAttempt) error { return errors.New("secret write detail") }},
		{"closed notifications", PublishUnknown, ErrPublisherNotifications, func(f *publisherPeer, a publishedAttempt) error { close(f.confirms); return nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bad, good := &publisherPeer{onPublish: test.fault}, &publisherPeer{}
			p := quarantinePeers(t, bad, good)
			q := makeQuarantine(t, quarantineFixture())
			r, err := p.publishQuarantine(publisherContext(t), q)
			var typed *PublishError
			if !errors.Is(err, test.cause) || !errors.As(err, &typed) || r.Outcome != test.outcome || typed.Outcome != test.outcome || bad.closeCount != 1 {
				t.Fatal("wrong failure responsibility", r, err)
			}
			for _, format := range []string{"%v", "%+v", "%#v"} {
				printed := fmt.Sprintf(format, err)
				if strings.Contains(printed, "secret") || strings.Contains(printed, "trace") {
					t.Fatal("audit details leaked")
				}
			}
			// A late old confirm cannot enter the independent remaining slot.
			if test.name != "closed notifications" && len(bad.confirms) == 0 {
				bad.confirms <- amqp.Confirmation{DeliveryTag: 1, Ack: true}
			}
			if r, err := p.publishQuarantine(publisherContext(t), q); err != nil || r.Outcome != PublishAccepted || r.ChannelEpoch != 2 {
				t.Fatal("late signal reached another slot", r, err)
			}
		})
	}
}

func TestQuarantinePublisherPrewriteAndCapacity(t *testing.T) {
	peer := &publisherPeer{}
	p := quarantinePeers(t, peer)
	q := makeQuarantine(t, quarantineFixture())
	ctx, cancel := context.WithCancel(publisherContext(t))
	cancel()
	for _, c := range []context.Context{nil, context.Background(), ctx} {
		if r, err := p.publishQuarantine(c, q); err == nil || r.Outcome != PublishNotStarted || r.PublishAttemptID != "" {
			t.Fatal("invalid context wrote")
		}
	}
	if r, err := p.publishQuarantine(publisherContext(t), QuarantineEnvelope{}); !errors.Is(err, ErrQuarantineEnvelope) || r.Outcome != PublishNotStarted {
		t.Fatal("zero snapshot wrote")
	}
	slot, err := p.acquire()
	if err != nil {
		t.Fatal(err)
	}
	if r, err := p.publishQuarantine(publisherContext(t), q); !errors.Is(err, ErrPublisherBusy) || r.Outcome != PublishNotStarted || r.PublishAttemptID != "" {
		t.Fatal("busy request wrote")
	}
	p.release(slot)
	if len(peer.attempts) != 0 {
		t.Fatal("prewrite request reached AMQP")
	}
	if r, err := p.publishQuarantine(publisherContext(t), q); err != nil || r.Outcome != PublishAccepted {
		t.Fatal("prewrite failure retired healthy publisher")
	}
	commandPool := peersPublisher(t, &publisherPeer{})
	if r, err := commandPool.publishQuarantine(publisherContext(t), q); !errors.Is(err, ErrPublisherUnavailable) || r.Outcome != PublishNotStarted {
		t.Fatal("quarantine sent to command pool")
	}
}

func dialQuarantinePeer(t *testing.T, peer *sessionPeer, options SessionOptions) *QuarantinePublisherSession {
	t.Helper()
	s, err := dialQuarantinePublisher(context.Background(), testSessionURI, options, peer.dial)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(context.Background()) })
	return s
}

func TestQuarantineSessionBoundsAndIndependentConnection(t *testing.T) {
	for _, mode := range []string{"silent_handshake", "silent_confirm_select", "blocked_write", "lost_confirm", "silent_close"} {
		t.Run(mode, func(t *testing.T) {
			peer := newSessionPeer(t, mode, 1)
			o := testSessionOptions()
			o.ConnectTimeout = 100 * time.Millisecond
			o.OperationTimeout = 120 * time.Millisecond
			o.CloseTimeout = 70 * time.Millisecond
			started := time.Now()
			s, err := dialQuarantinePublisher(context.Background(), testSessionURI, o, peer.dial)
			if mode == "silent_handshake" || mode == "silent_confirm_select" {
				if err == nil || s != nil {
					t.Fatal("stalled setup succeeded")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { s.Close(context.Background()) })
				if mode == "silent_close" {
					if err := s.Close(context.Background()); err == nil {
						t.Fatal("unjoined close succeeded")
					}
				} else {
					r, err := s.Publish(context.Background(), makeQuarantine(t, quarantineFixture()))
					if err == nil || r.Outcome != PublishUnknown {
						t.Fatal("unknown target transfer declared success", r, err)
					}
					if mode == "lost_confirm" {
						select {
						case wire := <-peer.received:
							if _, err := DecodeQuarantineEnvelope(wire.body); err != nil {
								t.Fatal(err)
							}
						default:
							t.Fatal("no actual target write")
						}
					}
				}
			}
			if time.Since(started) > 2*time.Second {
				t.Fatal("owned I/O exceeded hard bound")
			}
			if peer.socket.count.Load() != 1 {
				t.Fatal("owned socket not closed once")
			}
		})
	}
	apiPeer := newSessionPeer(t, "normal", 1)
	api := dialPeerSession(t, apiPeer, testSessionOptions())
	peer := newSessionPeer(t, "normal", 1)
	s := dialQuarantinePeer(t, peer, testSessionOptions())
	q := makeQuarantine(t, quarantineFixture())
	if r, err := s.Publish(context.Background(), QuarantineEnvelope{}); err == nil || r.Outcome != PublishNotStarted {
		t.Fatal("invalid envelope wrote")
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r, err := api.Publish(context.Background(), publisherCommand(t), time.Now()); err != nil || r.Outcome != PublishAccepted {
		t.Fatal("quarantine close affected API socket")
	}
	if r, err := s.Publish(context.Background(), q); !errors.Is(err, ErrPublisherUnavailable) || r.Outcome != PublishNotStarted {
		t.Fatal("closed quarantine session admitted work")
	}
	if strings.Contains(fmt.Sprintf("%#v", s), "private-secret") {
		t.Fatal("session credentials leaked")
	}
}
