package rabbitmq

import (
	"context"
	"errors"
	"net"

	amqp "github.com/rabbitmq/amqp091-go"
)

var (
	ErrConsumerConfig      = errors.New("invalid RabbitMQ consumer connection configuration")
	ErrConsumerTopology    = errors.New("consumer connection topology did not verify")
	ErrConsumerUnavailable = errors.New("RabbitMQ consumer connection is unavailable")
	ErrConsumerBusy        = errors.New("RabbitMQ consumer topology verification is already in progress")
)

// ConsumerSession owns a fresh, consumer-only TCP/AMQP connection. This initial
// lifecycle API verifies topology but does not yet subscribe, expose channels,
// ACK deliveries, publish messages or redial after failure.
type ConsumerSession struct {
	owner  *connectionOwner
	verify func(context.Context, *amqp.Connection) error
	gate   chan struct{}
}

func (*ConsumerSession) String() string { return "RabbitMQ owned consumer session" }

// DialConsumer performs bounded connect/setup and verifies the actual new AMQP
// connection against management's read-only local topology contract. The ctx
// controls initialization; after success, the caller explicitly owns Close.
func DialConsumer(ctx context.Context, address string, management *Management, vhost string, options ConnectionOptions) (*ConsumerSession, error) {
	if ctx == nil || management == nil || management.client == nil || !validConnectionOptions(address, options) {
		return nil, ErrConsumerConfig
	}
	uri, err := amqp.ParseURI(address)
	if err != nil || vhost == "" || uri.Vhost != vhost {
		return nil, ErrConsumerConfig
	}
	return dialConsumer(ctx, address, options, (&net.Dialer{}).DialContext, func(ctx context.Context, conn *amqp.Connection) error {
		return verifyNewConnection(ctx, conn, management, vhost)
	})
}

func dialConsumer(ctx context.Context, address string, options ConnectionOptions, dial func(context.Context, string, string) (net.Conn, error), verify func(context.Context, *amqp.Connection) error) (*ConsumerSession, error) {
	if verify == nil {
		return nil, ErrConsumerConfig
	}
	owner, err := dialOwnedConnection(ctx, address, options, dial, func(ctx context.Context, owner *connectionOwner) error {
		return verify(ctx, owner.connection)
	})
	if err != nil {
		if errors.Is(err, ErrSessionConfig) {
			return nil, ErrConsumerConfig
		}
		if errors.Is(err, ErrSessionSetup) || errors.Is(err, ErrSupervisorTopology) {
			return nil, ErrConsumerTopology
		}
		return nil, err
	}
	return &ConsumerSession{owner: owner, verify: verify, gate: make(chan struct{}, 1)}, nil
}

// Verify performs a fresh, explicit bounded check on this consumer connection.
// Only one check may be in flight. Busy/pre-cancelled requests do not interrupt
// a healthy connection; an active check failure retires only this connection.
func (s *ConsumerSession) Verify(ctx context.Context) error {
	if s == nil || s.owner == nil {
		return ErrConsumerUnavailable
	}
	if ctx == nil {
		return ErrConsumerConfig
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case s.gate <- struct{}{}:
		defer func() { <-s.gate }()
	default:
		return ErrConsumerBusy
	}
	if !s.owner.beginCall() {
		return ErrConsumerUnavailable
	}
	defer s.owner.endCall()
	operationCtx, cancel := context.WithTimeout(ctx, s.owner.options.OperationTimeout)
	stop := watchCancellation(operationCtx, s.owner.abort)
	err := s.verify(operationCtx, s.owner.connection)
	stop()
	operationErr := operationCtx.Err()
	cancel()
	if err != nil || operationErr != nil {
		s.owner.abort()
		if operationErr != nil {
			return operationErr
		}
		return ErrConsumerTopology
	}
	select {
	case <-s.owner.done:
		return ErrConsumerUnavailable
	default:
		return nil
	}
}

// Done signals stopped admission. Close also waits for owned active checks.
func (s *ConsumerSession) Done() <-chan struct{} { return s.owner.done }

func (s *ConsumerSession) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	return s.owner.Close(ctx)
}
