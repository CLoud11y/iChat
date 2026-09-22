package database

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"github.com/go-redis/redis/v8"
)

// Shared by every consumer of dirty:msgs in this Redis database. No TTL: an
// expired lease cannot safely fence an already running MySQL transaction.
const LegacyWorkerLockKey = "dirty:msgs:worker-owner"

var ErrLegacyWorkerBusy = errors.New("legacy worker already registered; stop the old process before recovering its lock")
var ErrLegacyWorkerOwnership = errors.New("legacy worker ownership lost; restart requires operator recovery")

type LegacyWorkerLock struct {
	rds   *redis.Client
	token string
	mu    sync.Mutex
	lost  bool
}

func AcquireLegacyWorker(ctx context.Context, rds *redis.Client) (*LegacyWorkerLock, error) {
	if rds == nil {
		return nil, ErrStorageUnavailable
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	l := &LegacyWorkerLock{rds: rds, token: hex.EncodeToString(b)}
	ok, err := rds.SetNX(ctx, LegacyWorkerLockKey, l.token, 0).Result()
	// A lost SET response is ambiguous. Never delete or retry ownership here.
	if err != nil {
		return nil, fmt.Errorf("register legacy worker (lock may remain): %w", err)
	}
	if !ok {
		return nil, ErrLegacyWorkerBusy
	}
	return l, nil
}

func (l *LegacyWorkerLock) Check(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lost {
		return ErrLegacyWorkerOwnership
	}
	owner, err := l.rds.Get(ctx, LegacyWorkerLockKey).Result()
	if err != nil || owner != l.token {
		l.lost = true
		return errors.Join(ErrLegacyWorkerOwnership, err)
	}
	return nil
}

// Release must only run after all jobs have stopped. A stale owner cannot
// delete a successor's lock. Releasing a missing lock is deliberately an error.
func (l *LegacyWorkerLock) Release(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lost = true
	n, err := l.rds.Eval(ctx, `if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end return 0`, []string{LegacyWorkerLockKey}, l.token).Int()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrLegacyWorkerOwnership
	}
	return nil
}

// Ownership and prefix validation happen in the SAME Redis operation.
func (l *LegacyWorkerLock) Acknowledge(ctx context.Context, prefix []string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lost {
		return ErrLegacyWorkerOwnership
	}
	if len(prefix) == 0 || len(prefix) > DirtyMessageBatchSize {
		return errors.New("invalid dirty prefix size")
	}
	args := make([]interface{}, 1, len(prefix)+1)
	args[0] = l.token
	for _, raw := range prefix {
		args = append(args, raw)
	}
	n, err := l.rds.Eval(ctx, `if redis.call('GET', KEYS[2]) ~= ARGV[1] then return -1 end
table.remove(ARGV, 1)
`+acknowledgeDirtyPrefix, []string{getDirtyMsgsKey(), LegacyWorkerLockKey}, args...).Int()
	if err != nil {
		l.lost = true
		return errors.Join(ErrLegacyWorkerOwnership, err)
	}
	if n == -1 {
		l.lost = true
		return ErrLegacyWorkerOwnership
	}
	if n != 1 {
		return ErrDirtyMessagePrefixMismatch
	}
	return nil
}
