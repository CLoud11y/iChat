package rabbitmq

import (
	"context"
	"net"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Each call acquires a new owner; target-specific publishers share code only.
func dialTargetPublisher(ctx context.Context, address string, options SessionOptions, dial func(context.Context, string, string) (net.Conn, error), prepare func(context.Context, *amqp.Connection) error, exchange, routingKey string) (*PublisherSession, error) {
	if ctx == nil || !validSessionOptions(address, options) {
		return nil, ErrSessionConfig
	}
	var p *Publisher
	owner, err := dialOwnedConnection(ctx, address, options.connectionOptions(), dial, func(ctx context.Context, owner *connectionOwner) error {
		if prepare != nil {
			if err := prepare(ctx, owner.connection); err != nil {
				return err
			}
		}
		var err error
		p, err = newTargetPublisher(func() (publishChannel, error) { return owner.connection.Channel() }, options.Publisher, exchange, routingKey)
		if err != nil {
			return err
		}
		p.interrupt, p.closeTimeout = owner.abort, options.CloseTimeout
		owner.mu.Lock()
		owner.stopWork, owner.closeWork = p.stopAdmission, p.Close
		closed := owner.closed
		owner.mu.Unlock()
		if closed {
			return ErrSessionSetup
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &PublisherSession{connectionOwner: owner, publisher: p, verify: prepare}, nil
}
