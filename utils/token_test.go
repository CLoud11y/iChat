package utils

import (
	"context"
	"errors"
	"iChat/config"
	"math"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/golang-jwt/jwt/v5"
)

type fakeBlacklist struct {
	banned            bool
	readErr, writeErr error
	reads, writes     int
	token             string
	ttl               time.Duration
	ctx               context.Context
}

func (b *fakeBlacklist) Contains(ctx context.Context, token string) (bool, error) {
	b.reads++
	b.ctx, b.token = ctx, token
	return b.banned, b.readErr
}
func (b *fakeBlacklist) Add(ctx context.Context, token string, ttl time.Duration) error {
	b.writes++
	b.ctx, b.token, b.ttl = ctx, token, ttl
	return b.writeErr
}
func tokenConfig() config.Config {
	var conf config.Config
	conf.JWT.Key = "test-key"
	conf.JWT.TokenHourLifeSpan = 1
	return conf
}
func testTokens(t *testing.T, store TokenBlacklist) *TokenService {
	t.Helper()
	s, err := NewTokenService(tokenConfig(), store)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func tokenFor(t *testing.T, s *TokenService) string {
	t.Helper()
	raw, err := s.GenerateToken(123)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestRedisReadFailureRejectsToken(t *testing.T) {
	rds := redis.NewClient(&redis.Options{})
	_ = rds.Close()
	s := testTokens(t, NewRedisTokenBlacklist(rds))
	if _, err := s.ValidateToken(context.Background(), tokenFor(t, s)); !errors.Is(err, ErrAuthUnavailable) || !errors.Is(err, redis.ErrClosed) {
		t.Fatalf("closed Redis read error=%v", err)
	}
}

func TestRedisWriteFailureRejectsLogout(t *testing.T) {
	store := &fakeBlacklist{writeErr: redis.ErrClosed}
	s := testTokens(t, store)
	if _, err := s.RevokeToken(context.Background(), tokenFor(t, s)); !errors.Is(err, ErrAuthUnavailable) || !errors.Is(err, redis.ErrClosed) {
		t.Fatalf("failed blacklist write error=%v", err)
	}
	if store.writes != 1 {
		t.Fatal("revocation did not try the write")
	}
}

func TestTokenValidationAndRevocation(t *testing.T) {
	store := &fakeBlacklist{}
	s := testTokens(t, store)
	now := time.Unix(1700000000, 500000000)
	s.now = func() time.Time { return now }
	raw := tokenFor(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	identity, err := s.ValidateToken(ctx, raw)
	if err != nil || identity.UserID != 123 {
		t.Fatalf("identity=%+v err=%v", identity, err)
	}
	if _, err := s.RevokeToken(ctx, raw); err != nil {
		t.Fatal(err)
	}
	if store.ctx != ctx || store.token != raw || store.ttl != time.Hour-500*time.Millisecond || store.writes != 1 {
		t.Fatalf("incorrect blacklist write: ttl=%v writes=%d", store.ttl, store.writes)
	}
	store.banned = true
	if _, err := s.ValidateToken(ctx, raw); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("banned token error=%v", err)
	}
	if _, err := s.RevokeToken(ctx, raw); !errors.Is(err, ErrUnauthenticated) || store.writes != 1 {
		t.Fatal("banned token wrote again")
	}
}

func TestInvalidClaimsNeverAccessBlacklist(t *testing.T) {
	for _, tc := range []struct {
		name   string
		uid    any
		expiry any
		method jwt.SigningMethod
		key    string
	}{
		{"missing_uid", nil, time.Now().Add(time.Hour).Unix(), jwt.SigningMethodHS256, "test-key"},
		{"zero_uid", 0, time.Now().Add(time.Hour).Unix(), jwt.SigningMethodHS256, "test-key"},
		{"negative_uid", -1, time.Now().Add(time.Hour).Unix(), jwt.SigningMethodHS256, "test-key"},
		{"fractional_uid", 1.5, time.Now().Add(time.Hour).Unix(), jwt.SigningMethodHS256, "test-key"},
		{"string_uid", "123", time.Now().Add(time.Hour).Unix(), jwt.SigningMethodHS256, "test-key"},
		{"overflow_uid", math.MaxFloat64, time.Now().Add(time.Hour).Unix(), jwt.SigningMethodHS256, "test-key"},
		{"missing_expiry", 123, nil, jwt.SigningMethodHS256, "test-key"},
		{"expired", 123, time.Now().Add(-time.Second).Unix(), jwt.SigningMethodHS256, "test-key"},
		{"wrong_key", 123, time.Now().Add(time.Hour).Unix(), jwt.SigningMethodHS256, "other-key"},
		{"wrong_algorithm", 123, time.Now().Add(time.Hour).Unix(), jwt.SigningMethodHS384, "test-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeBlacklist{}
			s := testTokens(t, store)
			claims := jwt.MapClaims{"user_id": tc.uid, "exp": tc.expiry, "authorized": true}
			raw, err := jwt.NewWithClaims(tc.method, claims).SignedString([]byte(tc.key))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.RevokeToken(context.Background(), raw); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("invalid token error=%v", err)
			}
			if store.reads != 0 || store.writes != 0 {
				t.Fatal("invalid token touched blacklist")
			}
		})
	}
}

func TestTokenExpiresBeforeRevocationWrite(t *testing.T) {
	store := &fakeBlacklist{}
	s := testTokens(t, store)
	now := time.Unix(1700000000, 0)
	s.now = func() time.Time { return now }
	raw := tokenFor(t, s)
	calls := 0
	s.now = func() time.Time {
		calls++
		if calls == 1 {
			return now
		}
		return now.Add(time.Hour)
	}
	if _, err := s.RevokeToken(context.Background(), raw); !errors.Is(err, ErrUnauthenticated) || store.writes != 0 {
		t.Fatalf("expired token caused write: err=%v writes=%d", err, store.writes)
	}
}

func TestMissingAuthDependencies(t *testing.T) {
	for _, store := range []TokenBlacklist{nil, NewRedisTokenBlacklist(nil)} {
		s := testTokens(t, store)
		if _, err := s.ValidateToken(context.Background(), tokenFor(t, s)); !errors.Is(err, ErrAuthUnavailable) {
			t.Fatalf("missing store error=%v", err)
		}
	}
	var missing *TokenService
	if _, err := missing.RevokeToken(context.Background(), "token"); !errors.Is(err, ErrAuthUnavailable) {
		t.Fatal(err)
	}
	for _, hours := range []int{0, -1, int(math.MaxInt64/int64(time.Hour)) + 1} {
		conf := tokenConfig()
		conf.JWT.TokenHourLifeSpan = hours
		if _, err := NewTokenService(conf, nil); !errors.Is(err, ErrAuthUnavailable) {
			t.Fatalf("lifetime %d error=%v", hours, err)
		}
	}
	conf := tokenConfig()
	conf.JWT.Key = ""
	if _, err := NewTokenService(conf, nil); !errors.Is(err, ErrAuthUnavailable) {
		t.Fatal("empty signing key accepted")
	}
}

type fakeTokenRedis struct {
	getErr, setErr error
	ctx            context.Context
	ttl            time.Duration
	key            string
}

func (r *fakeTokenRedis) Get(ctx context.Context, key string) *redis.StringCmd {
	r.ctx, r.key = ctx, key
	return redis.NewStringResult("", r.getErr)
}
func (r *fakeTokenRedis) Set(ctx context.Context, key string, _ interface{}, ttl time.Duration) *redis.StatusCmd {
	r.ctx, r.key, r.ttl = ctx, key, ttl
	return redis.NewStatusResult("OK", r.setErr)
}

func TestRedisBlacklistAdapter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, readErr := range []error{nil, redis.Nil, context.Canceled, context.DeadlineExceeded, redis.ErrClosed} {
		client := &fakeTokenRedis{getErr: readErr, setErr: redis.ErrClosed}
		store := &redisTokenBlacklist{client: client}
		banned, err := store.Contains(ctx, "original-token-key")
		if readErr == nil {
			if !banned || err != nil {
				t.Fatal("existing key not banned")
			}
		} else if readErr == redis.Nil {
			if banned || err != nil {
				t.Fatal("redis.Nil should mean not banned")
			}
		} else if banned || !errors.Is(err, readErr) {
			t.Fatalf("lost Redis error: %v", err)
		}
		if client.ctx != ctx || client.key != "original-token-key" {
			t.Fatal("request context or legacy key changed")
		}
		if err := store.Add(ctx, "original-token-key", time.Second); !errors.Is(err, redis.ErrClosed) {
			t.Fatal("Set error swallowed")
		}
		if client.ctx != ctx || client.ttl != time.Second {
			t.Fatal("Set context or TTL changed")
		}
	}
}
