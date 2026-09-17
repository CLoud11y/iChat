package utils

import (
	"context"
	"errors"
	"fmt"
	"iChat/config"
	"math"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/golang-jwt/jwt/v5"
)

var (
	ErrUnauthenticated = errors.New("unauthenticated")
	ErrAuthUnavailable = errors.New("authentication dependency unavailable")
)

type TokenBlacklist interface {
	Contains(context.Context, string) (bool, error)
	Add(context.Context, string, time.Duration) error
}

type tokenRedis interface {
	Get(context.Context, string) *redis.StringCmd
	Set(context.Context, string, interface{}, time.Duration) *redis.StatusCmd
}

type redisTokenBlacklist struct{ client tokenRedis }

func NewRedisTokenBlacklist(client *redis.Client) TokenBlacklist {
	if client == nil {
		return &redisTokenBlacklist{}
	}
	return &redisTokenBlacklist{client: client}
}

func (b *redisTokenBlacklist) Contains(ctx context.Context, token string) (bool, error) {
	if b == nil || b.client == nil {
		return false, ErrAuthUnavailable
	}
	err := b.client.Get(ctx, token).Err()
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, redis.Nil):
		return false, nil
	default:
		return false, err
	}
}

func (b *redisTokenBlacklist) Add(ctx context.Context, token string, ttl time.Duration) error {
	if b == nil || b.client == nil {
		return ErrAuthUnavailable
	}
	if ttl <= 0 {
		return ErrUnauthenticated
	}
	// Keep legacy blacklist keys so already revoked tokens remain revoked.
	return b.client.Set(ctx, token, 0, ttl).Err()
}

type TokenService struct {
	key       []byte
	lifetime  time.Duration
	blacklist TokenBlacklist
	now       func() time.Time
}

type tokenClaims struct {
	UserID     uint `json:"user_id"`
	Authorized bool `json:"authorized"`
	jwt.RegisteredClaims
}

type TokenIdentity struct {
	UserID    uint
	ExpiresAt time.Time
}

func NewTokenService(conf config.Config, blacklist TokenBlacklist) (*TokenService, error) {
	hours := conf.JWT.TokenHourLifeSpan
	if conf.JWT.Key == "" || hours <= 0 || int64(hours) > math.MaxInt64/int64(time.Hour) {
		return nil, fmt.Errorf("%w: JWT key and a valid positive lifetime are required", ErrAuthUnavailable)
	}
	return &TokenService{
		key: []byte(conf.JWT.Key), lifetime: time.Duration(hours) * time.Hour,
		blacklist: blacklist, now: time.Now,
	}, nil
}

func (s *TokenService) GenerateToken(userID uint) (string, error) {
	if s == nil || len(s.key) == 0 || s.now == nil || s.lifetime <= 0 {
		return "", ErrAuthUnavailable
	}
	if userID == 0 {
		return "", ErrUnauthenticated
	}
	claims := tokenClaims{UserID: userID, Authorized: true, RegisteredClaims: jwt.RegisteredClaims{
		ExpiresAt: jwt.NewNumericDate(s.now().Add(s.lifetime)),
	}}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.key)
}

func (s *TokenService) ValidateToken(ctx context.Context, raw string) (TokenIdentity, error) {
	if s == nil || len(s.key) == 0 || s.now == nil {
		return TokenIdentity{}, ErrAuthUnavailable
	}
	claims := &tokenClaims{}
	token, err := jwt.ParseWithClaims(raw, claims, func(*jwt.Token) (any, error) {
		return s.key, nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired(), jwt.WithTimeFunc(s.now))
	if err != nil || !token.Valid || claims.UserID == 0 {
		return TokenIdentity{}, ErrUnauthenticated
	}
	if s.blacklist == nil {
		return TokenIdentity{}, ErrAuthUnavailable
	}
	banned, err := s.blacklist.Contains(ctx, raw)
	if err != nil {
		return TokenIdentity{}, fmt.Errorf("%w: read token blacklist: %w", ErrAuthUnavailable, err)
	}
	if banned {
		return TokenIdentity{}, ErrUnauthenticated
	}
	return TokenIdentity{UserID: claims.UserID, ExpiresAt: claims.ExpiresAt.Time}, nil
}

// RevokeToken validates the token itself; callers cannot supply untrusted claims
// or a different expiry. Local logout effects must wait for this to succeed.
func (s *TokenService) RevokeToken(ctx context.Context, raw string) (TokenIdentity, error) {
	identity, err := s.ValidateToken(ctx, raw)
	if err != nil {
		return TokenIdentity{}, err
	}
	ttl := identity.ExpiresAt.Sub(s.now())
	if ttl <= 0 {
		return TokenIdentity{}, ErrUnauthenticated
	}
	if err := s.blacklist.Add(ctx, raw, ttl); err != nil {
		return TokenIdentity{}, fmt.Errorf("%w: write token blacklist: %w", ErrAuthUnavailable, err)
	}
	return identity, nil
}

// RespondAuthError exposes a stable protocol, never raw Redis/JWT diagnostics.
func RespondAuthError(c *gin.Context, err error) {
	status, reason, message := http.StatusUnauthorized, "UNAUTHENTICATED", "unauthenticated"
	if errors.Is(err, ErrAuthUnavailable) {
		status, reason, message = http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "authentication dependency unavailable"
	}
	c.AbortWithStatusJSON(status, gin.H{
		"code": -1, "msg": message, "data": gin.H{"reasonCode": reason},
	})
}
