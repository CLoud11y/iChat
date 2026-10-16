package health

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Storage monitors connectivity, not schema correctness or write permissions.
// Probe must honor its context. One probe runs at a time, without detached work.
type Storage struct {
	mu      sync.RWMutex
	ready   bool
	checked time.Time
	maxAge  time.Duration
	cancel  context.CancelFunc
	done    chan struct{}
}

func StartStorage(probe func(context.Context) error, interval, timeout time.Duration) *Storage {
	if probe == nil || interval <= 0 || timeout <= 0 {
		panic("health: invalid storage probe configuration")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Storage{maxAge: interval + 2*timeout, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		defer func() { s.mu.Lock(); s.ready = false; s.mu.Unlock() }()
		for {
			if ctx.Err() != nil {
				return
			}
			checkCtx, checkCancel := context.WithTimeout(ctx, timeout)
			err := probe(checkCtx)
			ready := err == nil && checkCtx.Err() == nil
			checkCancel()
			s.mu.Lock()
			s.ready, s.checked = ready && ctx.Err() == nil, time.Now()
			s.mu.Unlock()
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return s
}

func (s *Storage) StorageReady() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ready && time.Since(s.checked) <= s.maxAge
}

// Stop cancels an in-flight probe and waits before the owner closes its pool.
func (s *Storage) Stop() { s.cancel(); <-s.done }

// StopContext requests stop even for an expired observer. A timed-out caller
// can wait again; the probe still owns its pool until Done closes.
func (s *Storage) StopContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("health: nil stop context")
	}
	s.cancel()
	s.mu.Lock()
	s.ready = false
	s.mu.Unlock()
	select {
	case <-s.done:
		return nil
	default:
	}
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Storage) Done() <-chan struct{} { return s.done }
