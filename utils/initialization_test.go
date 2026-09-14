package utils

import (
	"context"
	"errors"
	"iChat/config"
	"testing"
)

func TestOpenRedisCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var conf config.Config
	conf.REDIS.Addr = "127.0.0.1:1"
	client, err := OpenRedis(ctx, conf)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("OpenRedis error = %v, want context.Canceled", err)
	}
	if client != nil || RDS != nil {
		t.Fatal("failed initialization must not expose a client")
	}
}

func TestMigrateLegacyRequiresDatabase(t *testing.T) {
	if err := MigrateLegacy(nil); err == nil {
		t.Fatal("migration without a database must return an error")
	}
}
