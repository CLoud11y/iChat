package utils

import (
	"context"
	"errors"
	"iChat/config"
	"net"
	"testing"
)

func TestMySQLConstructionModes(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	defer func() { _ = listener.Close(); <-done }()
	var conf config.Config
	conf.MYSQL.Host, conf.MYSQL.Port, err = net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conf.MYSQL.User, conf.MYSQL.DbName = "offline", "offline"
	db, err := NewMySQL(conf)
	if err != nil {
		t.Fatalf("lazy construction: %v", err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if pool.Stats().OpenConnections != 0 {
		t.Fatal("lazy constructor opened a connection")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := pool.PingContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ping: %v", err)
	}
	if strict, err := OpenMySQL(conf); err == nil || strict != nil {
		t.Fatal("migration opener accepted unavailable MySQL")
	}
}

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
