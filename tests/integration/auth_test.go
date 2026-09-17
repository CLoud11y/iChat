//go:build integration

package integration

import (
	"context"
	"errors"
	"iChat/tests/testenv"
	"iChat/utils"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
)

func TestAuthRedisIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env, err := testenv.Start(ctx, filepath.Join("..", "..", "docker-compose.test.yml"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("auth fixture run=%s", env.RunID())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := env.Close(ctx); err != nil {
			t.Errorf("cleanup %s: %v", env.RunID(), err)
		}
	})
	conf, err := env.Config()
	if err != nil {
		t.Fatal(err)
	}
	rds, err := utils.OpenRedis(ctx, conf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rds.Close() })
	conf.JWT.Key, conf.JWT.TokenHourLifeSpan = "isolated-auth-test", 1
	tokens, err := utils.NewTokenService(conf, utils.NewRedisTokenBlacklist(rds))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := tokens.GenerateToken(123)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tokens.ValidateToken(ctx, raw); err != nil {
		t.Fatalf("missing blacklist key rejected valid token: %v", err)
	}
	if _, err := tokens.RevokeToken(ctx, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := tokens.ValidateToken(ctx, raw); !errors.Is(err, utils.ErrUnauthenticated) {
		t.Fatalf("revoked token error=%v", err)
	}
	ttl, err := rds.PTTL(ctx, raw).Result()
	if err != nil || ttl <= 0 || ttl > time.Hour {
		t.Fatalf("blacklist TTL=%v err=%v", ttl, err)
	}
	second, err := tokens.GenerateToken(456)
	if err != nil {
		t.Fatal(err)
	}
	// Reads succeed while Redis itself rejects SET. This exercises a real write
	// failure after successful authentication, not just a disconnected client.
	if err := rds.Do(ctx, "ACL", "SETUSER", "auth_readonly", "on", ">"+env.RunID(), "~*", "+get", "+ping").Err(); err != nil {
		t.Fatal(err)
	}
	readOnly := redis.NewClient(&redis.Options{
		Addr: conf.REDIS.Addr, Username: "auth_readonly", Password: env.RunID(),
	})
	t.Cleanup(func() { _ = readOnly.Close() })
	readOnlyTokens, err := utils.NewTokenService(conf, utils.NewRedisTokenBlacklist(readOnly))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readOnlyTokens.ValidateToken(ctx, second); err != nil {
		t.Fatal(err)
	}
	if _, err := readOnlyTokens.RevokeToken(ctx, second); !errors.Is(err, utils.ErrAuthUnavailable) {
		t.Fatalf("Redis ACL write denial error=%v", err)
	}
	if count, err := rds.Exists(ctx, second).Result(); err != nil || count != 0 {
		t.Fatalf("failed revocation changed blacklist: count=%d err=%v", count, err)
	}
	canceled, cancelRequest := context.WithCancel(ctx)
	cancelRequest()
	if _, err := tokens.ValidateToken(canceled, second); !errors.Is(err, utils.ErrAuthUnavailable) {
		t.Fatalf("canceled request error=%v", err)
	}
	if err := rds.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := tokens.ValidateToken(ctx, second); !errors.Is(err, utils.ErrAuthUnavailable) || !errors.Is(err, redis.ErrClosed) {
		t.Fatalf("closed Redis error=%v", err)
	}
}
