package clientsim

import (
	"context"
	"errors"
	"time"
)

type RecoveryOptions struct {
	MaxRounds      int
	MaxSubmissions int
	BaseBackoff    time.Duration
	MaxBackoff     time.Duration
}

func DefaultRecoveryOptions() RecoveryOptions {
	return RecoveryOptions{MaxRounds: 8, MaxSubmissions: 2, BaseBackoff: time.Second, MaxBackoff: 15 * time.Second}
}

// Recover is an explicit finite run. A valid pending query may authorize one
// same-request repost in the next round; SQL/network errors never do so.
// Quarantine only polls, conflict stops, and terminal results never resend.
func (r *HTTPRunner) Recover(ctx context.Context, key string, options RecoveryOptions) error {
	if options.MaxRounds < 1 || options.MaxRounds > 100 || options.MaxSubmissions < 0 || options.MaxSubmissions > options.MaxRounds || options.BaseBackoff <= 0 || options.MaxBackoff < options.BaseBackoff || options.MaxBackoff > time.Minute {
		return ErrHTTPOptions
	}
	work, done, err := r.enter(ctx, key)
	if err != nil {
		return err
	}
	defer done()
	delay := options.BaseBackoff
	posts := 0
	repost := false
	var lastErr error
	for round := 0; round < options.MaxRounds; round++ {
		e, err := r.entry(work, key)
		if err != nil {
			return err
		}
		if e.State == Persisted || e.State == Rejected {
			return nil
		}
		if e.State == Conflict {
			return ErrBlocked
		}
		var advice httpAdvice
		if posts < options.MaxSubmissions && e.State != Quarantined && (e.State == Queued || repost) {
			posts++
			advice, err = r.submit(work, key)
			repost = false
		} else {
			advice, err = r.poll(work, key)
			repost = err == nil && advice.pending
		}
		lastErr = err
		if err != nil && !errors.Is(err, ErrHTTPUnavailable) && !errors.Is(err, ErrProtocol) {
			return err
		}
		current, readErr := r.entry(work, key)
		if readErr != nil {
			return readErr
		}
		if current.State == Persisted || current.State == Rejected {
			return nil
		}
		if current.State == Conflict {
			return ErrBlocked
		}
		if round+1 == options.MaxRounds {
			break
		}
		// Server hints are floors, capped at one minute for bounded scheduling.
		wait := max(delay, advice.after)
		if err := r.wait(work, wait); err != nil {
			return err
		}
		delay = min(delay*2, options.MaxBackoff)
	}
	if lastErr != nil {
		return safeError(ErrRecoveryExhausted, lastErr)
	}
	return ErrRecoveryExhausted
}

func waitRecovery(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
