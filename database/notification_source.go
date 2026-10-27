package database

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"

	"github.com/go-redis/redis/v8"
	"iChat/models"
)

// NotificationSource borrows SQL and Redis. Its subscriptions reuse the owned
// confirmation/forwarding/Close implementation, on independent v1 channels.
type NotificationSource struct {
	pool   *sql.DB
	client *redis.Client
}

func NewNotificationSource(pool *sql.DB, client *redis.Client) (*NotificationSource, error) {
	if pool == nil || client == nil {
		return nil, ErrStorageUnavailable
	}
	return &NotificationSource{pool, client}, nil
}
func (s *NotificationSource) Channels(ctx context.Context, user uint64, limit int) (channels []string, err error) {
	if s == nil || s.pool == nil || ctx == nil || user == 0 || limit < 1 || limit > 1024 {
		return nil, ErrInvalidMessage
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tx, err := s.pool.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, notificationError("channels begin", err)
	}
	defer func() {
		if e := tx.Rollback(); e != nil && !errors.Is(e, sql.ErrTxDone) {
			err = errors.Join(err, notificationError("channels release", e))
		}
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			channels = nil
		}
	}()
	var active bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM `user` WHERE id=? AND deleted_at IS NULL)", user).Scan(&active); err != nil {
		return nil, notificationError("channels user", err)
	}
	if !active {
		return nil, ErrConversationForbidden
	}
	rows, err := tx.QueryContext(ctx, "SELECT DISTINCT g.id FROM `group` g JOIN relation r ON r.target_id=g.id WHERE g.deleted_at IS NULL AND r.deleted_at IS NULL AND r.owner_id=? AND r.type=? ORDER BY g.id LIMIT ?", user, models.GroupRelation, limit+1)
	if err != nil {
		return nil, notificationError("channels groups", err)
	}
	defer func() {
		if e := rows.Close(); e != nil {
			err = errors.Join(err, notificationError("channels rows", e))
		}
	}()
	channels = []string{"ichat:notify:v1:user:" + strconv.FormatUint(user, 10)}
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			return nil, notificationError("channels scan", err)
		}
		if id == 0 || len(channels) > limit {
			return nil, ErrInvalidMessage
		}
		channels = append(channels, "ichat:notify:v1:group:"+strconv.FormatUint(id, 10))
	}
	if err := rows.Err(); err != nil {
		return nil, notificationError("channels iterate", err)
	}
	return channels, nil
}
func (s *NotificationSource) Subscribe(ctx context.Context, channels []string) (Subscription, error) {
	if s == nil || s.client == nil || ctx == nil || len(channels) == 0 || len(channels) > 1025 {
		return nil, ErrInvalidMessage
	}
	seen := make(map[string]bool, len(channels))
	for _, channel := range channels {
		if !strings.HasPrefix(channel, "ichat:notify:v1:user:") && !strings.HasPrefix(channel, "ichat:notify:v1:group:") {
			return nil, ErrInvalidMessage
		}
		id := strings.TrimPrefix(channel, "ichat:notify:v1:user:")
		if id == channel {
			id = strings.TrimPrefix(channel, "ichat:notify:v1:group:")
		}
		n, err := strconv.ParseUint(id, 10, 64)
		if err != nil || n == 0 || strconv.FormatUint(n, 10) != id || seen[channel] {
			return nil, ErrInvalidMessage
		}
		seen[channel] = true
	}
	return (&msgManager{rds: s.client}).subscribeChannels(ctx, append([]string(nil), channels...))
}
