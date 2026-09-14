package utils

import (
	"context"
	"fmt"
	"iChat/config"

	"github.com/go-redis/redis/v8"
)

var RDS *redis.Client

func OpenRedis(ctx context.Context, conf config.Config) (*redis.Client, error) {
	// 初始化redis
	rds := redis.NewClient(&redis.Options{
		Addr:         conf.REDIS.Addr,
		Password:     conf.REDIS.Password,
		PoolSize:     conf.REDIS.PoolSize,
		DB:           conf.REDIS.DB,
		MinIdleConns: conf.REDIS.MinIdleConn,
	})
	_, err := rds.Ping(ctx).Result()
	if err != nil {
		_ = rds.Close()
		return nil, fmt.Errorf("open redis: %w", err)
	}
	return rds, nil
}
