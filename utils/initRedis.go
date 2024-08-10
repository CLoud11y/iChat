package utils

import (
	"context"
	"fmt"
	"iChat/config"

	"github.com/go-redis/redis/v8"
)

var RDS *redis.Client

func init() {
	// 初始化redis
	RDS = redis.NewClient(&redis.Options{
		Addr:         config.Conf.REDIS.Addr,
		Password:     config.Conf.REDIS.Password,
		PoolSize:     config.Conf.REDIS.PoolSize,
		DB:           config.Conf.REDIS.DB,
		MinIdleConns: config.Conf.REDIS.MinIdleConn,
	})
	ctx := context.Background()
	result, err := RDS.Ping(ctx).Result()
	if err != nil {
		panic(err)
	}

	fmt.Println("redis init success: ", result)
}
