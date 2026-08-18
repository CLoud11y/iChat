package utils

import (
	"context"
	"iChat/config"

	"github.com/go-redis/redis/v8"
	"github.com/sirupsen/logrus"
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
	_, err := RDS.Ping(ctx).Result()
	if err != nil {
		panic(err)
	}

	Logger().WithFields(logrus.Fields{
		"address":  config.Conf.REDIS.Addr,
		"database": config.Conf.REDIS.DB,
	}).Info("redis connection initialized")
}
