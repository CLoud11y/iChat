package workers

import (
	"context"

	"github.com/go-redis/redis/v8"
	"iChat/messaging"
)

// RedisNotificationPublisher borrows the client; its resource owner must set
// bounded dial/read/write/pool timeouts. It never closes or reconfigures it.
type RedisNotificationPublisher struct{ client *redis.Client }

func NewRedisNotificationPublisher(client *redis.Client) (*RedisNotificationPublisher, error) {
	if client == nil {
		return nil, ErrOutboxWorkerInput
	}
	return &RedisNotificationPublisher{client: client}, nil
}
func (p *RedisNotificationPublisher) Publish(ctx context.Context, n messaging.Notification) error {
	if p == nil || p.client == nil || ctx == nil || !n.Valid() {
		return ErrOutboxWorkerInput
	}
	if _, ok := ctx.Deadline(); !ok {
		return ErrOutboxWorkerInput
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Zero subscribers is success; no delivery cursor or business state is
	// touched here. Any error after invocation is conservatively unknown.
	if err := p.client.Publish(ctx, n.Channel(), n.Body()).Err(); err != nil {
		return &OutboxError{Stage: "publish", cause: err}
	}
	return ctx.Err()
}
