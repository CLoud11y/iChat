package rabbitmq

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"iChat/messaging"
)

type PublishOutcome string

const (
	PublishAccepted   PublishOutcome = "accepted"
	PublishNotStarted PublishOutcome = "not_published"
	PublishUnroutable PublishOutcome = "unroutable"
	PublishNacked     PublishOutcome = "nacked"
	PublishUnknown    PublishOutcome = "publish_unknown"
	MaxPublisherSlots                = 64
)

var (
	ErrPublisherBusy          = errors.New("publisher slots are busy")
	ErrPublisherUnavailable   = errors.New("publisher is closed or has no live slots")
	ErrPublisherNotifications = errors.New("publisher notification or sequence does not match the session")
	ErrPublisherWrite         = errors.New("AMQP publication failed after write may have started")
	ErrPublisherReturn        = errors.New("publication could not be routed")
	ErrPublisherNack          = errors.New("broker negatively acknowledged publication")
	ErrPublisherDeadline      = errors.New("publication context must have a deadline")
)

// PublishResult describes this attempt only. accepted proves routed MQ receipt,
// not a MySQL commit or recipient delivery. CommandID stays stable across retries.
type PublishResult struct {
	Outcome          PublishOutcome
	CommandID        string
	PublishAttemptID string
	ConnectionEpoch  uint64
	ChannelEpoch     uint64
	DeliveryTag      uint64
}

type PublishError struct {
	Outcome PublishOutcome
	Cause   error
}

func (e *PublishError) Error() string {
	return fmt.Sprintf("RabbitMQ publish %s: %v", e.Outcome, e.Cause)
}
func (e *PublishError) Unwrap() error { return e.Cause }

type PublisherOptions struct {
	Slots           int
	ConnectionEpoch uint64
}

// A channel is privately owned by one slot. Only the current Publish execution
// for that slot reads its original notification channels; no forwarding goroutine
// or async return map is used. Raw notification buffers cover its one outstanding.
type publishChannel interface {
	NotifyReturn(chan amqp.Return) chan amqp.Return
	NotifyPublish(chan amqp.Confirmation) chan amqp.Confirmation
	NotifyClose(chan *amqp.Error) chan *amqp.Error
	Confirm(bool) error
	GetNextPublishSeqNo() uint64
	PublishWithContext(context.Context, string, string, bool, bool, amqp.Publishing) error
	IsClosed() bool
	Close() error
}

type publisherSlot struct {
	channel   publishChannel
	returns   <-chan amqp.Return
	confirms  <-chan amqp.Confirmation
	closes    <-chan *amqp.Error
	epoch     uint64
	nextTag   uint64
	retired   bool // guarded by Publisher.mu
	closeOnce sync.Once
}

// Publisher owns its channels, but not the supplied connection. Callers must
// provide bounded transport I/O and close the connection on transport failure.
// P3.3 never reconnects or replaces retired slots; the supervisor follows in P3.4.
type Publisher struct {
	mu              sync.Mutex
	closed          bool
	live            int
	available       chan *publisherSlot
	slots           []*publisherSlot
	done            chan struct{}
	connectionEpoch uint64
	// Set only by an owned, bounded session. Plain NewPublisher callers still
	// own their transport and must provide their own interruption policy.
	interrupt    func()
	closeTimeout time.Duration
}

func NewPublisher(connection *amqp.Connection, options PublisherOptions) (*Publisher, error) {
	if connection == nil || connection.IsClosed() || connection.IsRecoveryEnabled() || connection.IsTopologyRecoveryEnabled() {
		return nil, ErrPublisherUnavailable
	}
	return newPublisher(func() (publishChannel, error) { return connection.Channel() }, options)
}

func newPublisher(open func() (publishChannel, error), options PublisherOptions) (*Publisher, error) {
	if options.Slots < 1 || options.Slots > MaxPublisherSlots || options.ConnectionEpoch == 0 {
		return nil, errors.New("publisher requires 1 to 64 slots and a nonzero connection epoch")
	}
	p := &Publisher{available: make(chan *publisherSlot, options.Slots), done: make(chan struct{}), connectionEpoch: options.ConnectionEpoch}
	for i := 0; i < options.Slots; i++ {
		ch, err := open()
		if err != nil || ch == nil {
			p.Close()
			return nil, ErrPublisherUnavailable
		}
		s := &publisherSlot{channel: ch, epoch: uint64(i + 1), nextTag: 1}
		p.slots = append(p.slots, s)
		// Register exactly once and before enabling confirm mode.
		s.returns = ch.NotifyReturn(make(chan amqp.Return, 1))
		s.confirms = ch.NotifyPublish(make(chan amqp.Confirmation, 1))
		s.closes = ch.NotifyClose(make(chan *amqp.Error, 1))
		if ch.Confirm(false) != nil || ch.GetNextPublishSeqNo() != 1 || !s.clean() {
			p.Close()
			return nil, ErrPublisherUnavailable
		}
		p.live++
		p.available <- s
	}
	return p, nil
}

// Publish fails immediately on a full pool, before generating an attempt or
// writing bytes. It creates a fresh validated server envelope for every attempt.
// Context bounds confirm waiting, but cannot interrupt v1.15.0's in-flight write;
// transport write deadlines are a separate requirement (P3.4).
func (p *Publisher) Publish(ctx context.Context, command messaging.Command, receivedAt time.Time) (PublishResult, error) {
	r := PublishResult{Outcome: PublishNotStarted}
	if ctx == nil {
		return publishFailure(r, ErrPublisherDeadline)
	}
	if err := ctx.Err(); err != nil {
		return publishFailure(r, err)
	}
	if _, ok := ctx.Deadline(); !ok {
		return publishFailure(r, ErrPublisherDeadline)
	}
	if err := messaging.ValidateCommand(command); err != nil {
		return publishFailure(r, err)
	}
	r.CommandID = command.CommandID
	if p == nil {
		return publishFailure(r, ErrPublisherUnavailable)
	}
	s, err := p.acquire()
	if err != nil {
		return publishFailure(r, err)
	}
	// Until accepted, an acquired slot stays exclusively owned by this call.
	envelope, err := messaging.NewEnvelope(command, receivedAt)
	if err != nil {
		p.release(s)
		return publishFailure(r, err)
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		p.release(s)
		return publishFailure(r, messaging.ErrInvalidInput)
	}
	r.PublishAttemptID, r.ConnectionEpoch, r.ChannelEpoch = envelope.PublishAttemptID, p.connectionEpoch, s.epoch
	if err := ctx.Err(); err != nil {
		p.release(s)
		return publishFailure(r, err)
	}
	if !s.clean() || !p.active(s) || s.nextTag == 0 || s.nextTag == math.MaxUint64 || s.channel.GetNextPublishSeqNo() != s.nextTag {
		p.retire(s)
		return publishFailure(r, ErrPublisherNotifications)
	}
	r.DeliveryTag = s.nextTag
	message := amqp.Publishing{ContentType: "application/json", DeliveryMode: amqp.Persistent, MessageId: command.CommandID, CorrelationId: envelope.PublishAttemptID, Body: body}
	r.Outcome = PublishUnknown // Set before handing control to a network write.
	if p.interrupt != nil {
		stop := watchCancellation(ctx, p.interrupt)
		defer stop()
	}
	if err := s.channel.PublishWithContext(ctx, CommandsExchange, SubmitRoutingKey, true, false, message); err != nil {
		p.retire(s)
		return publishFailure(r, ErrPublisherWrite)
	}
	for {
		select {
		case <-ctx.Done():
			p.retire(s)
			return publishFailure(r, ctx.Err())
		case <-p.done:
			p.retire(s)
			return publishFailure(r, ErrPublisherUnavailable)
		case <-s.closes:
			p.retire(s)
			return publishFailure(r, ErrPublisherUnavailable)
		case returned, ok := <-s.returns:
			return p.returned(s, r, message, returned, ok)
		case confirm, ok := <-s.confirms:
			if !ok || confirm.DeliveryTag != r.DeliveryTag {
				p.retire(s)
				return publishFailure(r, ErrPublisherNotifications)
			}
			return p.confirmed(ctx, s, r, message, confirm)
		}
	}
}

// confirmed drains the ORIGINAL Return channel before interpreting an ACK.
// Fixed v1.15.0 dispatches Return synchronously before the following confirm.
// With one outstanding and cap=1, the matching Return cannot be lost to a full
// buffer. The scheduler may read ACK first; this nonblocking drain handles it.
func (p *Publisher) confirmed(ctx context.Context, s *publisherSlot, r PublishResult, message amqp.Publishing, confirm amqp.Confirmation) (PublishResult, error) {
	select {
	case returned, ok := <-s.returns:
		return p.returned(s, r, message, returned, ok)
	default:
	}
	if !confirm.Ack {
		r.Outcome = PublishNacked
		p.retire(s)
		return publishFailure(r, ErrPublisherNack)
	}
	// Detect closed, duplicated or contaminated notifications and cancellation
	// before the success linearization point and before returning the slot.
	if err := ctx.Err(); err != nil {
		p.retire(s)
		return publishFailure(r, err)
	}
	if !s.clean() || s.channel.GetNextPublishSeqNo() != r.DeliveryTag+1 {
		p.retire(s)
		return publishFailure(r, ErrPublisherNotifications)
	}
	s.nextTag++
	if !p.release(s) {
		p.retire(s)
		return publishFailure(r, ErrPublisherUnavailable)
	}
	r.Outcome = PublishAccepted
	return r, nil
}

func (p *Publisher) returned(s *publisherSlot, r PublishResult, msg amqp.Publishing, returned amqp.Return, ok bool) (PublishResult, error) {
	p.retire(s)
	if !ok || returned.MessageId != msg.MessageId || returned.CorrelationId != msg.CorrelationId || returned.Exchange != CommandsExchange || returned.RoutingKey != SubmitRoutingKey || returned.ReplyCode != 312 || returned.ContentType != msg.ContentType || returned.DeliveryMode != msg.DeliveryMode || !bytes.Equal(returned.Body, msg.Body) {
		return publishFailure(r, ErrPublisherNotifications)
	}
	r.Outcome = PublishUnroutable
	return publishFailure(r, ErrPublisherReturn)
}

func publishFailure(r PublishResult, cause error) (PublishResult, error) {
	return r, &PublishError{Outcome: r.Outcome, Cause: cause}
}

func (s *publisherSlot) clean() bool {
	if s.channel.IsClosed() {
		return false
	}
	select {
	case <-s.closes:
		return false
	default:
	}
	select {
	case <-s.returns:
		return false
	default:
	}
	select {
	case <-s.confirms:
		return false
	default:
	}
	return true
}

func (p *Publisher) acquire() (*publisherSlot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.live == 0 {
		return nil, ErrPublisherUnavailable
	}
	select {
	case s := <-p.available:
		return s, nil
	default:
		return nil, ErrPublisherBusy
	}
}

func (p *Publisher) active(s *publisherSlot) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.closed && !s.retired
}

func (p *Publisher) release(s *publisherSlot) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || s.retired {
		return false
	}
	p.available <- s
	return true
}

func (p *Publisher) retire(s *publisherSlot) {
	p.mu.Lock()
	if !s.retired {
		s.retired = true
		p.live--
	}
	p.mu.Unlock()
	p.closeSlot(s)
}

func (p *Publisher) closeSlot(s *publisherSlot) {
	s.closeOnce.Do(func() {
		if p.interrupt != nil {
			ctx, cancel := context.WithTimeout(context.Background(), p.closeTimeout)
			defer cancel()
			stop := watchCancellation(ctx, p.interrupt)
			defer stop()
		}
		s.channel.Close()
	})
}

// Close stops admission before closing channels and leaves the connection to its
// owner. Like channel creation/retirement, close RPCs require bounded transport.
func (p *Publisher) Close() {
	if p == nil {
		return
	}
	p.stopAdmission()
	for _, s := range p.slots {
		p.closeSlot(s)
	}
}

// stopAdmission performs no AMQP RPC. A transport watchdog must use this before
// closing the socket, rather than waiting for Close to acquire client I/O locks.
func (p *Publisher) stopAdmission() {
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		close(p.done)
	}
	for _, s := range p.slots {
		if !s.retired {
			s.retired = true
		}
	}
	p.live = 0
	p.mu.Unlock()
}
