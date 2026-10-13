package rabbitmq

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"iChat/messaging"
)

type publishedAttempt struct {
	message              amqp.Publishing
	tag                  uint64
	exchange, key        string
	mandatory, immediate bool
}
type publisherPeer struct {
	mu            sync.Mutex
	returns       chan amqp.Return
	confirms      chan amqp.Confirmation
	closes        chan *amqp.Error
	registrations [3]int
	next          uint64
	closed        bool
	closeCount    int
	confirmErr    error
	attempts      []publishedAttempt
	onPublish     func(*publisherPeer, publishedAttempt) error
}

func (f *publisherPeer) NotifyReturn(c chan amqp.Return) chan amqp.Return {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.returns = c
	f.registrations[0]++
	return c
}
func (f *publisherPeer) NotifyPublish(c chan amqp.Confirmation) chan amqp.Confirmation {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.confirms = c
	f.registrations[1]++
	return c
}
func (f *publisherPeer) NotifyClose(c chan *amqp.Error) chan *amqp.Error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes = c
	f.registrations[2]++
	return c
}
func (f *publisherPeer) Confirm(noWait bool) error {
	if noWait || f.registrations != [3]int{1, 1, 1} || cap(f.returns) != 1 || cap(f.confirms) != 1 || cap(f.closes) != 1 {
		return errors.New("unsafe notification setup")
	}
	return f.confirmErr
}
func (f *publisherPeer) GetNextPublishSeqNo() uint64 { f.mu.Lock(); defer f.mu.Unlock(); return f.next }
func (f *publisherPeer) IsClosed() bool              { f.mu.Lock(); defer f.mu.Unlock(); return f.closed }
func (f *publisherPeer) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	f.closeCount++
	return nil
}
func (f *publisherPeer) PublishWithContext(ctx context.Context, exchange, key string, mandatory, immediate bool, msg amqp.Publishing) error {
	f.mu.Lock()
	a := publishedAttempt{msg, f.next, exchange, key, mandatory, immediate}
	f.next++
	f.attempts = append(f.attempts, a)
	f.mu.Unlock()
	if f.onPublish != nil {
		return f.onPublish(f, a)
	}
	f.confirms <- amqp.Confirmation{DeliveryTag: a.tag, Ack: true}
	return nil
}

func publisherCommand(t *testing.T) messaging.Command {
	t.Helper()
	c, err := messaging.Normalize(12, messaging.SendRequest{ClientMessageID: "8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1", TargetType: messaging.Private, TargetID: "34", Media: messaging.Text, Content: "中🙂\x00 publisher"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func publisherContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}
func peersPublisher(t *testing.T, peers ...*publisherPeer) *Publisher {
	t.Helper()
	index := 0
	p, err := newPublisher(func() (publishChannel, error) {
		f := peers[index]
		index++
		if f.next == 0 {
			f.next = 1
		}
		return f, nil
	}, PublisherOptions{Slots: len(peers), ConnectionEpoch: 7})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}
func returnedAttempt(a publishedAttempt) amqp.Return {
	return amqp.Return{ReplyCode: 312, Exchange: a.exchange, RoutingKey: a.key, ContentType: a.message.ContentType, DeliveryMode: a.message.DeliveryMode, MessageId: a.message.MessageId, CorrelationId: a.message.CorrelationId, Body: a.message.Body}
}

func TestPublisherValidatedPersistentAttemptsAndReuse(t *testing.T) {
	f := &publisherPeer{}
	p := peersPublisher(t, f)
	c := publisherCommand(t)
	ctx := publisherContext(t)
	var previous string
	for i := uint64(1); i <= 3; i++ {
		r, err := p.Publish(ctx, c, time.Now())
		if err != nil || r.Outcome != PublishAccepted || r.CommandID != c.CommandID || r.DeliveryTag != i || r.ConnectionEpoch != 7 || r.ChannelEpoch != 1 || r.PublishAttemptID == previous {
			t.Fatalf("invalid accepted result=%+v err=%v", r, err)
		}
		a := f.attempts[i-1]
		if a.exchange != CommandsExchange || a.key != SubmitRoutingKey || !a.mandatory || a.immediate || a.message.DeliveryMode != amqp.Persistent || a.message.ContentType != "application/json" || a.message.MessageId != c.CommandID || a.message.CorrelationId != r.PublishAttemptID || a.message.Expiration != "" || len(a.message.Headers) != 0 {
			t.Fatal("wrong AMQP publication contract")
		}
		e, err := messaging.DecodeEnvelope(a.message.Body)
		if err != nil || e.CommandID != c.CommandID || e.PayloadHash != c.PayloadHash || e.Content != c.Content || e.PublishAttemptID != r.PublishAttemptID {
			t.Fatal("envelope changed", err)
		}
		previous = r.PublishAttemptID
	}
	if f.closeCount != 0 || f.registrations != [3]int{1, 1, 1} {
		t.Fatal("successful slot retired or listeners duplicated")
	}
}

func TestPublisherACKFirstDrainsOriginalReturn(t *testing.T) {
	// Invoke the actual ACK decision path deterministically after reading ACK
	// first. Both original raw channels are already populated by a peer.
	f := &publisherPeer{}
	p := peersPublisher(t, f)
	s, err := p.acquire()
	if err != nil {
		t.Fatal(err)
	}
	c := publisherCommand(t)
	e, err := messaging.NewEnvelope(c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(e)
	msg := amqp.Publishing{ContentType: "application/json", DeliveryMode: amqp.Persistent, MessageId: c.CommandID, CorrelationId: e.PublishAttemptID, Body: body}
	f.onPublish = func(f *publisherPeer, a publishedAttempt) error {
		f.returns <- returnedAttempt(a)
		f.confirms <- amqp.Confirmation{DeliveryTag: a.tag, Ack: true}
		return nil
	}
	if err := f.PublishWithContext(publisherContext(t), CommandsExchange, SubmitRoutingKey, true, false, msg); err != nil {
		t.Fatal(err)
	}
	ack := <-s.confirms
	r, err := p.confirmed(publisherContext(t), s, PublishResult{Outcome: PublishUnknown, DeliveryTag: 1, ConnectionEpoch: 7, ChannelEpoch: 1}, msg, ack)
	if !errors.Is(err, ErrPublisherReturn) || r.Outcome != PublishUnroutable || f.closeCount != 1 {
		t.Fatalf("return+ACK became accepted: %+v %v", r, err)
	}
	if _, err := p.acquire(); !errors.Is(err, ErrPublisherUnavailable) {
		t.Fatal("returned slot was reused", err)
	}
}

func TestPublisherNotificationFailuresRetire(t *testing.T) {
	cases := []struct {
		name    string
		outcome PublishOutcome
		cause   error
		act     func(*publisherPeer, publishedAttempt) error
	}{
		{"return_before_ack", PublishUnroutable, ErrPublisherReturn, func(f *publisherPeer, a publishedAttempt) error {
			f.returns <- returnedAttempt(a)
			f.confirms <- amqp.Confirmation{DeliveryTag: a.tag, Ack: true}
			return nil
		}},
		{"wrong_command", PublishUnknown, ErrPublisherNotifications, func(f *publisherPeer, a publishedAttempt) error {
			r := returnedAttempt(a)
			r.MessageId = "other"
			f.returns <- r
			return nil
		}},
		{"wrong_attempt", PublishUnknown, ErrPublisherNotifications, func(f *publisherPeer, a publishedAttempt) error {
			r := returnedAttempt(a)
			r.CorrelationId = "old"
			f.returns <- r
			return nil
		}},
		{"wrong_body", PublishUnknown, ErrPublisherNotifications, func(f *publisherPeer, a publishedAttempt) error {
			r := returnedAttempt(a)
			r.Body = []byte("other")
			f.returns <- r
			return nil
		}},
		{"nack", PublishNacked, ErrPublisherNack, func(f *publisherPeer, a publishedAttempt) error {
			f.confirms <- amqp.Confirmation{DeliveryTag: a.tag, Ack: false}
			return nil
		}},
		{"wrong_tag", PublishUnknown, ErrPublisherNotifications, func(f *publisherPeer, a publishedAttempt) error {
			f.confirms <- amqp.Confirmation{DeliveryTag: a.tag + 1, Ack: true}
			return nil
		}},
		{"write_error", PublishUnknown, ErrPublisherWrite, func(f *publisherPeer, a publishedAttempt) error { return io.ErrUnexpectedEOF }},
		{"channel_close", PublishUnknown, ErrPublisherUnavailable, func(f *publisherPeer, a publishedAttempt) error {
			f.closes <- &amqp.Error{Code: 504, Reason: "secret body"}
			return nil
		}},
		{"closed_returns", PublishUnknown, ErrPublisherNotifications, func(f *publisherPeer, a publishedAttempt) error { close(f.returns); return nil }},
		{"closed_confirms", PublishUnknown, ErrPublisherNotifications, func(f *publisherPeer, a publishedAttempt) error { close(f.confirms); return nil }},
		{"close_plus_ack", PublishUnknown, nil, func(f *publisherPeer, a publishedAttempt) error {
			f.closes <- &amqp.Error{Code: 504}
			f.confirms <- amqp.Confirmation{DeliveryTag: a.tag, Ack: true}
			return nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &publisherPeer{onPublish: tc.act}
			p := peersPublisher(t, f)
			r, err := p.Publish(publisherContext(t), publisherCommand(t), time.Now())
			if err == nil || r.Outcome != tc.outcome || (tc.cause != nil && !errors.Is(err, tc.cause)) {
				t.Fatalf("unsafe outcome=%+v err=%v", r, err)
			}
			var failure *PublishError
			if !errors.As(err, &failure) || failure.Outcome != r.Outcome {
				t.Fatal("missing typed outcome")
			}
			if f.closeCount != 1 {
				t.Fatal("failure did not retire channel")
			}
			if _, err := p.Publish(publisherContext(t), publisherCommand(t), time.Now()); !errors.Is(err, ErrPublisherUnavailable) {
				t.Fatal("retired slot reused", err)
			}
		})
	}
}

func TestPublisherCapacityAndSlotIsolation(t *testing.T) {
	started := make(chan uint64, 2)
	proceed := make(chan struct{})
	var wg sync.WaitGroup
	peers := []*publisherPeer{{}, {}}
	for i, f := range peers {
		epoch := uint64(i + 1)
		f.onPublish = func(f *publisherPeer, a publishedAttempt) error {
			started <- epoch
			<-proceed
			f.confirms <- amqp.Confirmation{DeliveryTag: a.tag, Ack: true}
			return nil
		}
	}
	p := peersPublisher(t, peers...)
	ctx := publisherContext(t)
	c := publisherCommand(t)
	results := make(chan PublishResult, 2)
	failures := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r, err := p.Publish(ctx, c, time.Now()); results <- r; failures <- err }()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	r, err := p.Publish(ctx, c, time.Now())
	if !errors.Is(err, ErrPublisherBusy) || r.Outcome != PublishNotStarted || r.PublishAttemptID != "" {
		t.Fatal("full pool wrote or created attempt", r, err)
	}
	close(proceed)
	wg.Wait()
	seen := map[uint64]bool{}
	for i := 0; i < 2; i++ {
		r := <-results
		if err := <-failures; err != nil || r.Outcome != PublishAccepted || seen[r.ChannelEpoch] {
			t.Fatal("slots shared attempt", err)
		}
		seen[r.ChannelEpoch] = true
	}
	for _, f := range peers {
		if len(f.attempts) != 1 || f.closeCount != 0 {
			t.Fatal("unbounded writes or premature slot retirement")
		}
	}
}

func TestPublisherLateAckCannotReachAnotherSlot(t *testing.T) {
	first := &publisherPeer{onPublish: func(*publisherPeer, publishedAttempt) error { return io.EOF }}
	started := make(chan publishedAttempt, 1)
	second := &publisherPeer{onPublish: func(f *publisherPeer, a publishedAttempt) error { started <- a; return nil }}
	p := peersPublisher(t, first, second)
	ctx := publisherContext(t)
	c := publisherCommand(t)
	if r, err := p.Publish(ctx, c, time.Now()); err == nil || r.Outcome != PublishUnknown {
		t.Fatal("write failure accepted")
	}
	done := make(chan PublishResult, 1)
	errs := make(chan error, 1)
	go func() { r, err := p.Publish(ctx, c, time.Now()); done <- r; errs <- err }()
	var a publishedAttempt
	select {
	case a = <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	first.confirms <- amqp.Confirmation{DeliveryTag: 1, Ack: true}
	select {
	case <-done:
		t.Fatal("old slot ACK completed a new attempt")
	default:
	}
	second.confirms <- amqp.Confirmation{DeliveryTag: a.tag, Ack: true}
	select {
	case r := <-done:
		if err := <-errs; err != nil || r.Outcome != PublishAccepted || r.ChannelEpoch != 2 || r.DeliveryTag != 1 {
			t.Fatal("new slot correlation failed", r, err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestPublisherCancellationAndPreflightNoWrite(t *testing.T) {
	f := &publisherPeer{}
	p := peersPublisher(t, f)
	c := publisherCommand(t)
	ctx, cancel := context.WithCancel(publisherContext(t))
	cancel()
	for _, ctx := range []context.Context{ctx, context.Background()} {
		if r, err := p.Publish(ctx, c, time.Now()); err == nil || r.Outcome != PublishNotStarted {
			t.Fatal("unbounded or canceled context wrote")
		}
	}
	bad := c
	bad.Content += "tampered"
	if r, err := p.Publish(publisherContext(t), bad, time.Now()); err == nil || r.Outcome != PublishNotStarted {
		t.Fatal("invalid command wrote")
	}
	if r, err := p.Publish(publisherContext(t), c, time.Time{}); err == nil || r.Outcome != PublishNotStarted {
		t.Fatal("invalid timestamp wrote")
	}
	if len(f.attempts) != 0 || f.closeCount != 0 {
		t.Fatal("prewrite failure touched channel")
	}
	f.confirms <- amqp.Confirmation{DeliveryTag: 1, Ack: true}
	if r, err := p.Publish(publisherContext(t), c, time.Now()); !errors.Is(err, ErrPublisherNotifications) || r.Outcome != PublishNotStarted || len(f.attempts) != 0 || f.closeCount != 1 {
		t.Fatal("stale notification admitted publication", r, err)
	}
}

func TestPublisherConfirmTimeoutAndConcurrentClose(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "close"}[shutdown], func(t *testing.T) {
			started := make(chan struct{})
			f := &publisherPeer{onPublish: func(*publisherPeer, publishedAttempt) error { close(started); return nil }}
			p := peersPublisher(t, f)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				r, err := p.Publish(ctx, publisherCommand(t), time.Now())
				if r.Outcome != PublishUnknown {
					t.Error("in-flight failure was definite")
				}
				done <- err
			}()
			<-started
			if shutdown {
				p.Close()
			}
			select {
			case err := <-done:
				if err == nil || (!shutdown && !errors.Is(err, context.DeadlineExceeded)) {
					t.Fatal("timeout/close accepted", err)
				}
			case <-time.After(time.Second):
				t.Fatal("confirm wait did not exit")
			}
			p.Close()
			if f.closeCount != 1 {
				t.Fatal("channel closed more than once")
			}
		})
	}
}

func TestPublisherConstructionGuards(t *testing.T) {
	for _, o := range []PublisherOptions{{Slots: 0, ConnectionEpoch: 1}, {Slots: 65, ConnectionEpoch: 1}, {Slots: 1, ConnectionEpoch: 0}} {
		if _, err := newPublisher(func() (publishChannel, error) { t.Fatal("invalid options opened channel"); return nil, nil }, o); err == nil {
			t.Fatal("invalid options accepted")
		}
	}
	if _, err := NewPublisher(nil, PublisherOptions{Slots: 1, ConnectionEpoch: 1}); err == nil {
		t.Fatal("nil connection accepted")
	}
	first := &publisherPeer{next: 1}
	second := &publisherPeer{next: 1, confirmErr: io.EOF}
	index := 0
	if _, err := newPublisher(func() (publishChannel, error) {
		index++
		if index == 1 {
			return first, nil
		}
		return second, nil
	}, PublisherOptions{Slots: 2, ConnectionEpoch: 1}); err == nil || first.closeCount != 1 || second.closeCount != 1 {
		t.Fatal("partial constructor leaked channels")
	}
}
