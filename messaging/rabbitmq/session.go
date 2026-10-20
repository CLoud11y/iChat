package rabbitmq

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"iChat/messaging"
)

var (
	ErrSessionConfig  = errors.New("invalid RabbitMQ session options or endpoint")
	ErrSessionConnect = errors.New("RabbitMQ TCP, TLS or AMQP handshake failed")
	ErrSessionSetup   = errors.New("RabbitMQ publisher channel setup failed")
	ErrSessionClose   = errors.New("RabbitMQ session graceful close failed")
)

type SessionOptions struct {
	Publisher        PublisherOptions
	ConnectTimeout   time.Duration
	OperationTimeout time.Duration
	WriteTimeout     time.Duration
	CloseTimeout     time.Duration
	Heartbeat        time.Duration
	TLS              *tls.Config
}

func DefaultSessionOptions() SessionOptions {
	o := DefaultConnectionOptions()
	return SessionOptions{Publisher: PublisherOptions{Slots: 8, ConnectionEpoch: 1}, ConnectTimeout: o.ConnectTimeout, OperationTimeout: o.OperationTimeout, WriteTimeout: o.WriteTimeout, CloseTimeout: o.CloseTimeout, Heartbeat: o.Heartbeat}
}

func (o SessionOptions) connectionOptions() ConnectionOptions {
	return ConnectionOptions{ConnectTimeout: o.ConnectTimeout, OperationTimeout: o.OperationTimeout, WriteTimeout: o.WriteTimeout, CloseTimeout: o.CloseTimeout, Heartbeat: o.Heartbeat, TLS: o.TLS}
}

// PublisherSession owns one socket, AMQP connection and publisher. Its private
// transport owner is reused as code, never as an instance, by consumer sessions.
type PublisherSession struct {
	*connectionOwner
	publisher *Publisher
	verify    func(context.Context, *amqp.Connection) error
}

func (s *PublisherSession) String() string { return "RabbitMQ owned publisher session" }

func DialPublisher(ctx context.Context, address string, options SessionOptions) (*PublisherSession, error) {
	return dialPublisher(ctx, address, options, (&net.Dialer{}).DialContext)
}

func dialPublisher(ctx context.Context, address string, options SessionOptions, dial func(context.Context, string, string) (net.Conn, error)) (*PublisherSession, error) {
	return dialPreparedPublisher(ctx, address, options, dial, nil)
}

// Preparation and all confirm channels share one setup deadline/watchdog.
func dialPreparedPublisher(ctx context.Context, address string, options SessionOptions, dial func(context.Context, string, string) (net.Conn, error), prepare func(context.Context, *amqp.Connection) error) (*PublisherSession, error) {
	return dialTargetPublisher(ctx, address, options, dial, prepare, CommandsExchange, SubmitRoutingKey)
}

// Only the supervisor calls this, serially. The check shares this connection's
// lifetime accounting with Publish, but uses a fresh passive channel. A failed
// or timed-out check retires the whole epoch, including unresolved publishes.
func (s *PublisherSession) verifyTopology(ctx context.Context) error {
	if s == nil || s.connectionOwner == nil || s.verify == nil || ctx == nil {
		return ErrSupervisorTopology
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.beginCall() {
		return ErrPublisherUnavailable
	}
	defer s.endCall()
	operationCtx, cancel := context.WithTimeout(ctx, s.options.OperationTimeout)
	defer cancel()
	stop := watchCancellation(operationCtx, s.abort)
	err := s.verify(operationCtx, s.connection)
	stop()
	if err != nil || operationCtx.Err() != nil {
		s.abort()
		return ErrSupervisorTopology
	}
	select {
	case <-s.Done():
		return ErrPublisherUnavailable
	default:
		return nil
	}
}

func validSessionOptions(address string, o SessionOptions) bool {
	return o.Publisher.Slots >= 1 && o.Publisher.Slots <= MaxPublisherSlots && o.Publisher.ConnectionEpoch != 0 &&
		validConnectionOptions(address, o.connectionOptions())
}

func (s *PublisherSession) Publish(ctx context.Context, command messaging.Command, receivedAt time.Time) (PublishResult, error) {
	r := PublishResult{Outcome: PublishNotStarted}
	if s == nil || s.connectionOwner == nil || ctx == nil {
		return publishFailure(r, ErrPublisherUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return publishFailure(r, err)
	}
	if !s.beginCall() {
		return publishFailure(r, ErrPublisherUnavailable)
	}
	defer s.endCall()
	operationCtx, cancel := context.WithTimeout(ctx, s.options.OperationTimeout)
	defer cancel()
	return s.publisher.Publish(operationCtx, command, receivedAt)
}

// Done signals stopped admission, not completed client teardown.
func (s *PublisherSession) Done() <-chan struct{} { return s.done }

func (s *PublisherSession) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	return s.connectionOwner.Close(ctx)
}
