package rabbitmq

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"

	amqp "github.com/rabbitmq/amqp091-go"
)

const QuarantineContentType = "application/vnd.ichat.quarantine.v1+json"
const QuarantineMessageType = "ichat.quarantine.v1"

// QuarantinePublishResult describes the target transfer attempt, not an HTTP
// command acceptance or a source delivery disposition.
type QuarantinePublishResult struct {
	Outcome          PublishOutcome
	TransportID      string
	PublishAttemptID string
	ConnectionEpoch  uint64
	ChannelEpoch     uint64
	DeliveryTag      uint64
}

// QuarantinePublisherSession owns a dedicated socket and confirm channels.
// It neither shares an API/consumer connection nor settles source deliveries.
type QuarantinePublisherSession struct{ session *PublisherSession }

// DialQuarantinePublisher requires provisioned topology. It declares nothing,
// disables automatic Recovery and never retries an uncertain publication.
func DialQuarantinePublisher(ctx context.Context, address string, options SessionOptions) (*QuarantinePublisherSession, error) {
	return dialQuarantinePublisher(ctx, address, options, (&net.Dialer{}).DialContext)
}

func dialQuarantinePublisher(ctx context.Context, address string, options SessionOptions, dial func(context.Context, string, string) (net.Conn, error)) (*QuarantinePublisherSession, error) {
	s, err := dialTargetPublisher(ctx, address, options, dial, nil, QuarantineExchange, QuarantineRoutingKey)
	if err != nil {
		return nil, err
	}
	return &QuarantinePublisherSession{session: s}, nil
}

func (s *QuarantinePublisherSession) Publish(ctx context.Context, envelope QuarantineEnvelope) (QuarantinePublishResult, error) {
	r := QuarantinePublishResult{Outcome: PublishNotStarted, TransportID: envelope.TransportID()}
	if s == nil || s.session == nil || s.session.connectionOwner == nil || ctx == nil {
		_, err := publishFailure(PublishResult{Outcome: PublishNotStarted}, ErrPublisherUnavailable)
		return r, err
	}
	if err := ctx.Err(); err != nil {
		return quarantinePublishFailure(r, err)
	}
	if !s.session.beginCall() {
		return quarantinePublishFailure(r, ErrPublisherUnavailable)
	}
	defer s.session.endCall()
	operationCtx, cancel := context.WithTimeout(ctx, s.session.options.OperationTimeout)
	defer cancel()
	return s.session.publisher.publishQuarantine(operationCtx, envelope)
}

func (p *Publisher) publishQuarantine(ctx context.Context, envelope QuarantineEnvelope) (QuarantinePublishResult, error) {
	r := QuarantinePublishResult{Outcome: PublishNotStarted, TransportID: envelope.TransportID()}
	if err := publishContext(ctx); err != nil {
		return quarantinePublishFailure(r, err)
	}
	// Only constructors/decoder can supply a nonempty immutable snapshot.
	if len(envelope.encoded) == 0 || envelope.id == "" {
		return quarantinePublishFailure(r, ErrQuarantineEnvelope)
	}
	if p == nil || p.exchange != QuarantineExchange || p.routingKey != QuarantineRoutingKey {
		return quarantinePublishFailure(r, ErrPublisherUnavailable)
	}
	result, err := p.publish(ctx, PublishResult{Outcome: PublishNotStarted}, func() (amqp.Publishing, error) {
		var attempt [16]byte
		if _, err := rand.Read(attempt[:]); err != nil {
			return amqp.Publishing{}, ErrPublisherUnavailable
		}
		return amqp.Publishing{ContentType: QuarantineContentType, Type: QuarantineMessageType,
			DeliveryMode: amqp.Persistent, MessageId: envelope.TransportID(), CorrelationId: hex.EncodeToString(attempt[:]), Body: envelope.Encode()}, nil
	})
	return QuarantinePublishResult{Outcome: result.Outcome, TransportID: envelope.TransportID(), PublishAttemptID: result.PublishAttemptID,
		ConnectionEpoch: result.ConnectionEpoch, ChannelEpoch: result.ChannelEpoch, DeliveryTag: result.DeliveryTag}, err
}

func quarantinePublishFailure(r QuarantinePublishResult, cause error) (QuarantinePublishResult, error) {
	_, err := publishFailure(PublishResult{Outcome: r.Outcome}, cause)
	return r, err
}

// Done signals stopped admission; Close is the join for this session's calls.
func (s *QuarantinePublisherSession) Done() <-chan struct{} {
	if s == nil || s.session == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	return s.session.Done()
}

func (s *QuarantinePublisherSession) Close(ctx context.Context) error {
	if s == nil || s.session == nil {
		return nil
	}
	return s.session.Close(ctx)
}

func (s *QuarantinePublisherSession) String() string   { return "RabbitMQ quarantine publisher session" }
func (s *QuarantinePublisherSession) GoString() string { return s.String() }
