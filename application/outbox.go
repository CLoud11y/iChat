package application

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"strconv"
	"time"

	"github.com/go-redis/redis/v8"
	mysql "github.com/go-sql-driver/mysql"
	"iChat/config"
	"iChat/database"
	"iChat/workers"
)

var _ RuntimeService = (*workers.OutboxService)(nil)
var _ RuntimeOutboxReadiness = (*workers.OutboxService)(nil)

// NewOutboxFactory borrows SQL/Redis. Their caller must retain both until every
// service Done closes, including when runtime Close returns pending.
func NewOutboxFactory(pool *sql.DB, client *redis.Client, options workers.OutboxServiceOptions) (RuntimeFactory, error) {
	if pool == nil || client == nil {
		return nil, workers.ErrOutboxServiceInput
	}
	return NewOutboxSourceFactory(func(context.Context) (workers.OutboxResources, error) { return outboxResources(pool, client, nil) }, options)
}

// NewOutboxSourceFactory creates idle services and confines a resource source
// to the Outbox role. Each invocation must supply independently owned resources.
func NewOutboxSourceFactory(source workers.OutboxSource, options workers.OutboxServiceOptions) (RuntimeFactory, error) {
	if source == nil || options.Validate() != nil {
		return nil, workers.ErrOutboxServiceInput
	}
	return func(ctx context.Context, inputs RoleDependencies) (RuntimeService, error) {
		if ctx == nil || !nilRoleCapability(inputs.Publisher) || len(inputs.Consumers) != 0 || inputs.PersistenceConsumers != nil || inputs.QuarantineConsumers != nil {
			return nil, workers.ErrOutboxServiceInput
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return workers.NewOutboxService(source, options)
	}, nil
}

// NewConfiguredOutboxFactory is pure until Start. Its dedicated SQL/Redis
// resources are closed by the service only after all callbacks actually join.
// It performs no migration, adopts no shared globals and has no MQ capability.
func NewConfiguredOutboxFactory(c config.Config, o workers.OutboxServiceOptions) (RuntimeFactory, error) {
	port, err := strconv.Atoi(c.MYSQL.Port)
	_, redisPort, addrErr := net.SplitHostPort(c.REDIS.Addr)
	rp, redisErr := strconv.Atoi(redisPort)
	if err != nil || port < 1 || port > 65535 || c.MYSQL.Host == "" || c.MYSQL.User == "" || c.MYSQL.DbName == "" || addrErr != nil || redisErr != nil || rp < 1 || rp > 65535 || c.REDIS.DB < 0 || c.REDIS.PoolSize < 1 || c.REDIS.PoolSize > 1024 || o.Validate() != nil {
		return nil, workers.ErrOutboxServiceInput
	}
	return NewOutboxSourceFactory(configuredOutboxSource(c, o), o)
}
func configuredOutboxSource(c config.Config, o workers.OutboxServiceOptions) workers.OutboxSource {
	return func(ctx context.Context) (workers.OutboxResources, error) {
		if err := ctx.Err(); err != nil {
			return workers.OutboxResources{}, err
		}
		cfg := mysql.NewConfig()
		cfg.User, cfg.Passwd, cfg.DBName = c.MYSQL.User, c.MYSQL.Password, c.MYSQL.DbName
		cfg.Net, cfg.Addr = "tcp", net.JoinHostPort(c.MYSQL.Host, c.MYSQL.Port)
		cfg.ParseTime, cfg.Loc = true, time.UTC
		cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout = o.ProbeTimeout, o.Loop.ClaimTimeout, o.Loop.SettlementTimeout
		cfg.Params = map[string]string{"charset": "utf8mb4"}
		connector, err := mysql.NewConnector(cfg)
		if err != nil {
			return workers.OutboxResources{}, err
		}
		pool := sql.OpenDB(connector)
		pool.SetMaxOpenConns(2)
		pool.SetMaxIdleConns(1)
		client := redis.NewClient(&redis.Options{Addr: c.REDIS.Addr, Password: c.REDIS.Password, DB: c.REDIS.DB, PoolSize: c.REDIS.PoolSize, MinIdleConns: 0, MaxRetries: -1, DialTimeout: o.ProbeTimeout, ReadTimeout: o.Loop.PublishTimeout, WriteTimeout: o.Loop.PublishTimeout, PoolTimeout: o.ProbeTimeout})
		return outboxResources(pool, client, func() error { return errors.Join(client.Close(), pool.Close()) })
	}
}
func outboxResources(pool *sql.DB, client *redis.Client, closeResources func() error) (workers.OutboxResources, error) {
	r := workers.OutboxResources{Close: closeResources}
	store, err := database.NewOutboxRepository(pool)
	if err != nil {
		return r, err
	}
	publisher, err := workers.NewRedisNotificationPublisher(client)
	if err != nil {
		return r, err
	}
	r.Store, r.Publisher = store, publisher
	r.ProbeStorage = func(ctx context.Context) error {
		if err := pool.PingContext(ctx); err != nil {
			return err
		}
		// Read-only column check catches missing reliable schema before ready.
		rows, err := pool.QueryContext(ctx, "SELECT event_id,event_type,command_id,conversation_id,message_id,payload,status,available_at,lease_token,lease_until,attempts,last_error,created_at,completed_at FROM message_outbox WHERE 1=0")
		if err != nil {
			return err
		}
		return rows.Close()
	}
	r.ProbeNotification = func(ctx context.Context) error { return client.Ping(ctx).Err() }
	return r, nil
}
