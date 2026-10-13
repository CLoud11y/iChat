package rabbitmq

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

const MaxConsumerPrefetch = 256

var (
	ErrConsumerChannelSetup       = errors.New("RabbitMQ consumer channel setup failed")
	ErrConsumerDelivery           = errors.New("invalid RabbitMQ consumer delivery handle or notification")
	ErrConsumerDeliverySettled    = errors.New("RabbitMQ delivery has already been settled or attempted")
	ErrConsumerDispositionUnknown = errors.New("RabbitMQ delivery disposition write outcome is unknown")
)

// ConsumerMessage has no AMQP Acknowledger. Mutable metadata never determines
// the channel/tag used by Ack/Reject; those come from a private delivery record.
type ConsumerMessage struct {
	Body                                          []byte
	Headers                                       amqp.Table
	ContentType, ContentEncoding                  string
	DeliveryMode, Priority                        uint8
	CorrelationID, ReplyTo, Expiration, MessageID string
	Timestamp                                     time.Time
	Type, UserID, AppID                           string
	ConsumerTag                                   string
	DeliveryTag                                   uint64
	Redelivered                                   bool
	Exchange, RoutingKey                          string
}

type ConsumerDelivery struct {
	Message ConsumerMessage
	record  *consumerDeliveryRecord
}

type consumerDeliveryRecord struct {
	channel *ConsumerChannel
	tag     uint64
	settled bool // Protected by channel.mu.
}

// ConsumerChannel is one sequential worker's subscription. Its session owns a
// dedicated connection; Close closes that session and requeues unacked work.
type ConsumerChannel struct {
	session            *ConsumerSession
	channel            *amqp.Channel
	deliveries         <-chan amqp.Delivery
	closes             <-chan *amqp.Error
	cancels            <-chan string
	consumerTag, queue string
	gate               chan struct{}
	monitorDone        chan struct{}
	mu                 sync.Mutex
	active             *consumerDeliveryRecord
}

func (*ConsumerChannel) String() string { return "RabbitMQ owned consumer channel" }
func (ConsumerDelivery) String() string {
	return "RabbitMQ owned consumer delivery (content redacted)"
}

func (d ConsumerDelivery) GoString() string { return d.String() }

// OpenConsumer creates at most one consumption channel for this session's
// lifetime. Multiple workers use multiple independently owned sessions.
// Qos and Consume both wait for replies, with autoAck=false and global=false.
func (s *ConsumerSession) OpenConsumer(ctx context.Context, queue string, prefetch int) (*ConsumerChannel, error) {
	if s == nil || s.owner == nil {
		return nil, ErrConsumerUnavailable
	}
	if ctx == nil || (queue != PersistQueue && queue != QuarantineQueue) || prefetch < 1 || prefetch > MaxConsumerPrefetch {
		return nil, ErrConsumerConfig
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var randomTag [16]byte
	if _, err := rand.Read(randomTag[:]); err != nil {
		return nil, ErrConsumerConfig
	}
	if !s.owner.beginCall() {
		return nil, ErrConsumerUnavailable
	}
	defer s.owner.endCall()
	s.owner.mu.Lock()
	if s.owner.closed {
		s.owner.mu.Unlock()
		return nil, ErrConsumerUnavailable
	}
	if s.opened {
		s.owner.mu.Unlock()
		return nil, ErrConsumerBusy
	}
	s.opened = true
	s.owner.mu.Unlock()
	operationCtx, cancel := context.WithTimeout(ctx, s.owner.options.OperationTimeout)
	defer cancel()
	stop := watchCancellation(operationCtx, s.owner.abort)
	ch, err := s.owner.connection.Channel()
	var stream *ConsumerChannel
	if err == nil {
		stream = &ConsumerChannel{session: s, channel: ch, queue: queue, consumerTag: "ichat-consumer-" + hex.EncodeToString(randomTag[:]), gate: make(chan struct{}, 1), monitorDone: make(chan struct{})}
		stream.closes = ch.NotifyClose(make(chan *amqp.Error, 1))
		stream.cancels = ch.NotifyCancel(make(chan string, 1))
		err = ch.Qos(prefetch, 0, false)
		if err == nil {
			stream.deliveries, err = ch.Consume(queue, stream.consumerTag, false, false, false, false, nil)
		}
	}
	if err == nil {
		s.owner.mu.Lock()
		if s.owner.closed {
			err = ErrConsumerUnavailable
		} else {
			// Close first stops admission, then joins this monitor before AMQP
			// teardown. The monitor never performs an AMQP RPC or holds mu.
			s.owner.closeWork = func() { <-stream.monitorDone }
			go stream.monitor()
		}
		s.owner.mu.Unlock()
	}
	stop()
	if err != nil || operationCtx.Err() != nil {
		s.owner.abort()
		if operationCtx.Err() != nil {
			return nil, operationCtx.Err()
		}
		return nil, ErrConsumerChannelSetup
	}
	select {
	case <-s.owner.done:
		return nil, ErrConsumerUnavailable
	default:
	}
	return stream, nil
}

func (s *ConsumerChannel) monitor() {
	defer close(s.monitorDone)
	select {
	case <-s.session.Done():
		return
	case <-s.closes:
	case <-s.cancels:
	}
	// A broker cancel/channel failure retires this worker connection; it must
	// not leave unacked messages stranded on an open, idle consumer session.
	s.session.owner.abort()
}

// Next never auto-acks. At most one delivery is handed to the worker at a time;
// the broker's positive prefetch bounds deliveries still buffered by the client.
// An idle wait timeout is zero-write and does not retire the connection.
func (s *ConsumerChannel) Next(ctx context.Context) (*ConsumerDelivery, error) {
	if s == nil || s.session == nil {
		return nil, ErrConsumerUnavailable
	}
	if ctx == nil {
		return nil, ErrConsumerConfig
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case s.gate <- struct{}{}:
		defer func() { <-s.gate }()
	default:
		return nil, ErrConsumerBusy
	}
	if !s.session.owner.beginCall() {
		return nil, ErrConsumerUnavailable
	}
	defer s.session.owner.endCall()
	s.mu.Lock()
	pending := s.active != nil
	s.mu.Unlock()
	if pending {
		return nil, ErrConsumerBusy
	}
	waitCtx, cancel := context.WithTimeout(ctx, s.session.owner.options.OperationTimeout)
	defer cancel()
	var delivery amqp.Delivery
	select {
	case <-s.session.Done():
		return nil, ErrConsumerUnavailable
	case <-waitCtx.Done():
		return nil, waitCtx.Err()
	case value, ok := <-s.deliveries:
		if !ok {
			s.session.owner.abort()
			return nil, ErrConsumerUnavailable
		}
		delivery = value
	}
	if delivery.ConsumerTag != s.consumerTag || delivery.DeliveryTag == 0 {
		s.session.owner.abort()
		return nil, ErrConsumerDelivery
	}
	select {
	case <-s.session.Done():
		return nil, ErrConsumerUnavailable
	default:
	}
	record := &consumerDeliveryRecord{channel: s, tag: delivery.DeliveryTag}
	s.mu.Lock()
	s.active = record
	s.mu.Unlock()
	return &ConsumerDelivery{record: record, Message: ConsumerMessage{
		Body: delivery.Body, Headers: delivery.Headers, ContentType: delivery.ContentType, ContentEncoding: delivery.ContentEncoding,
		DeliveryMode: delivery.DeliveryMode, Priority: delivery.Priority, CorrelationID: delivery.CorrelationId, ReplyTo: delivery.ReplyTo,
		Expiration: delivery.Expiration, MessageID: delivery.MessageId, Timestamp: delivery.Timestamp, Type: delivery.Type,
		UserID: delivery.UserId, AppID: delivery.AppId, ConsumerTag: delivery.ConsumerTag, DeliveryTag: delivery.DeliveryTag,
		Redelivered: delivery.Redelivered, Exchange: delivery.Exchange, RoutingKey: delivery.RoutingKey,
	}}, nil
}

// Ack writes basic.ack on the original channel, always multiple=false. A nil
// error proves a completed local write, not a broker reply or a database commit.
func (d *ConsumerDelivery) Ack(ctx context.Context) error { return d.disposition(ctx, true, false) }

// Reject writes a single basic.reject. Quarantine has no further DLX, so
// rejecting it without requeue is forbidden to preserve the audit responsibility.
func (d *ConsumerDelivery) Reject(ctx context.Context, requeue bool) error {
	return d.disposition(ctx, false, requeue)
}

func (d *ConsumerDelivery) disposition(ctx context.Context, ack, requeue bool) error {
	if d == nil || d.record == nil || d.record.channel == nil {
		return ErrConsumerDelivery
	}
	if ctx == nil {
		return ErrConsumerConfig
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s, record := d.record.channel, d.record
	if !ack && !requeue && s.queue == QuarantineQueue {
		return ErrConsumerConfig
	}
	select {
	case s.gate <- struct{}{}:
		defer func() { <-s.gate }()
	default:
		return ErrConsumerBusy
	}
	if !s.session.owner.beginCall() {
		return ErrConsumerUnavailable
	}
	defer s.session.owner.endCall()
	s.mu.Lock()
	if record.settled || s.active != record {
		s.mu.Unlock()
		return ErrConsumerDeliverySettled
	}
	// Never hold the delivery mutex across network I/O: abort must be able to
	// stop admission and close the socket that interrupts this exact write.
	record.settled = true
	s.mu.Unlock()
	operationCtx, cancel := context.WithTimeout(ctx, s.session.owner.options.OperationTimeout)
	defer cancel()
	stop := watchCancellation(operationCtx, s.session.owner.abort)
	var err error
	if ack {
		err = s.channel.Ack(record.tag, false)
	} else {
		err = s.channel.Reject(record.tag, requeue)
	}
	stop()
	s.mu.Lock()
	s.active = nil
	s.mu.Unlock()
	if err != nil || operationCtx.Err() != nil {
		s.session.owner.abort()
		return errors.Join(ErrConsumerDispositionUnknown, operationCtx.Err())
	}
	select {
	case <-s.session.Done():
		return ErrConsumerDispositionUnknown
	default:
		return nil
	}
}

func (s *ConsumerChannel) Done() <-chan struct{} { return s.session.Done() }

func (s *ConsumerChannel) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	return s.session.Close(ctx)
}
