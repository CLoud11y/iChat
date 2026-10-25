package database

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"math/big"
	"strings"
	"time"

	"iChat/messaging"
)

var (
	ErrOutboxSettlementInput = errors.New("invalid outbox settlement input")
	ErrOutboxLeaseLost       = errors.New("outbox lease no longer owned")
)

type OutboxRetryReason string

const (
	OutboxRetryPublishFailed  OutboxRetryReason = "NOTIFICATION_FAILED"
	OutboxRetryPublishUnknown OutboxRetryReason = "NOTIFICATION_UNKNOWN"
	OutboxRetryUnavailable    OutboxRetryReason = "DEPENDENCY_UNAVAILABLE"
	OutboxRetryInvalidPayload OutboxRetryReason = "INVALID_NOTIFICATION"
)

type OutboxRetryOptions struct{ Base, Max time.Duration }

func (o OutboxRetryOptions) Validate() error {
	if o.Base < time.Millisecond || o.Max < o.Base || o.Max > time.Hour || o.Base%time.Microsecond != 0 || o.Max%time.Microsecond != 0 {
		return ErrOutboxSettlementInput
	}
	return nil
}

func (o OutboxRetryOptions) ceiling(attempt uint32) (time.Duration, error) {
	if o.Validate() != nil || attempt == 0 {
		return 0, ErrOutboxSettlementInput
	}
	cap := o.Base
	for i := uint32(1); i < attempt && cap < o.Max; i++ {
		if cap > o.Max/2 {
			cap = o.Max
		} else {
			cap *= 2
		}
	}
	return cap, nil
}

// Delay uses equal jitter in [ceiling/2, ceiling], rounded to microseconds.
// Exponential growth saturates at Max, including uint32's largest attempt.
func (o OutboxRetryOptions) Delay(attempt uint32) (time.Duration, error) {
	cap, err := o.ceiling(attempt)
	if err != nil {
		return 0, err
	}
	micros := int64(cap / time.Microsecond)
	lower := (micros + 1) / 2
	n, err := rand.Int(rand.Reader, big.NewInt(micros-lower+1))
	if err != nil {
		return 0, outboxFailure("retry entropy", err)
	}
	return time.Duration(lower+n.Int64()) * time.Microsecond, nil
}

type OutboxSettlement struct {
	EventID     string
	Status      string
	CompletedAt *time.Time
	AvailableAt *time.Time
}

const selectOutboxSettlementSQL = "SELECT event_id,status,lease_token,DATE_FORMAT(lease_until,'%Y-%m-%d %H:%i:%s.%f'),attempts,DATE_FORMAT(completed_at,'%Y-%m-%d %H:%i:%s.%f') FROM message_outbox WHERE event_id=? FOR UPDATE"
const completeOutboxSQL = "UPDATE message_outbox SET status='completed',completed_at=?,lease_token=NULL,lease_until=NULL,last_error=NULL WHERE event_id=? AND status='leased' AND lease_token=? AND lease_until=? AND attempts=? AND lease_until>UTC_TIMESTAMP(6) AND completed_at IS NULL"
const retryOutboxSQL = "UPDATE message_outbox SET status='pending',available_at=?,last_error=?,lease_token=NULL,lease_until=NULL WHERE event_id=? AND status='leased' AND lease_token=? AND lease_until=? AND attempts=? AND lease_until>UTC_TIMESTAMP(6) AND completed_at IS NULL"

// Complete records notification completion, never client delivery. Any error
// returns no success receipt, including commit/rollback uncertainty.
func (r *OutboxRepository) Complete(ctx context.Context, lease OutboxLease) (OutboxSettlement, error) {
	return r.settle(ctx, lease, true, 0, "")
}

// Retry releases a currently owned lease and schedules another claim. attempts
// increments on Claim only. Unknown notification results may be duplicated.
func (r *OutboxRepository) Retry(ctx context.Context, lease OutboxLease, options OutboxRetryOptions, reason OutboxRetryReason) (OutboxSettlement, error) {
	switch reason {
	case OutboxRetryPublishFailed, OutboxRetryPublishUnknown, OutboxRetryUnavailable, OutboxRetryInvalidPayload:
	default:
		return OutboxSettlement{}, ErrOutboxSettlementInput
	}
	delay, err := options.Delay(lease.Attempts)
	if err != nil {
		return OutboxSettlement{}, err
	}
	return r.settle(ctx, lease, false, delay, reason)
}

func validOutboxSettlementLease(lease OutboxLease) bool {
	until := lease.Until.UTC()
	if len(lease.EventID) != 64 || !messaging.ValidPublishAttemptID(lease.Token) || lease.Attempts == 0 || until.IsZero() || until.Year() < 1000 || until.Year() > 9999 || until.Nanosecond()%1000 != 0 {
		return false
	}
	for _, c := range lease.EventID {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func (r *OutboxRepository) settle(ctx context.Context, lease OutboxLease, complete bool, delay time.Duration, reason OutboxRetryReason) (result OutboxSettlement, err error) {
	if r == nil || r.db == nil {
		return result, ErrOutboxUnavailable
	}
	if ctx == nil || !validOutboxSettlementLease(lease) {
		return result, ErrOutboxSettlementInput
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return result, outboxFailure("begin settlement", err)
	}
	defer func() {
		if e := tx.Rollback(); e != nil && !errors.Is(e, sql.ErrTxDone) {
			err = errors.Join(err, outboxFailure("rollback settlement", errors.Join(ErrOutboxRollbackUnknown, e)))
		}
		if err != nil {
			result = OutboxSettlement{}
		}
	}()
	var id, status string
	var token, until, completed sql.NullString
	var attempts uint64
	err = tx.QueryRowContext(ctx, selectOutboxSettlementSQL, lease.EventID).Scan(&id, &status, &token, &until, &attempts, &completed)
	if errors.Is(err, sql.ErrNoRows) {
		return result, ErrOutboxLeaseLost
	}
	if err != nil {
		return result, outboxFailure("lock settlement", err)
	}
	if id != lease.EventID || status != "leased" || !token.Valid || token.String != lease.Token || !until.Valid || until.String != lease.Until.UTC().Format(mysqlMicrosecondLayout) || attempts != uint64(lease.Attempts) || completed.Valid {
		return result, ErrOutboxLeaseLost
	}
	// Sample after acquiring the lock, not before a possibly long lock wait.
	var clock string
	if err := tx.QueryRowContext(ctx, "SELECT DATE_FORMAT(UTC_TIMESTAMP(6),'%Y-%m-%d %H:%i:%s.%f')").Scan(&clock); err != nil {
		return result, outboxFailure("settlement clock", err)
	}
	now, parseErr := time.Parse(mysqlMicrosecondLayout, clock)
	if parseErr != nil || now.Year() < 1000 || now.Add(delay).Year() > 9999 {
		return result, ErrOutboxInvariant
	}
	if !lease.Until.After(now) {
		return result, ErrOutboxLeaseLost
	}
	var write sql.Result
	if complete {
		write, err = tx.ExecContext(ctx, completeOutboxSQL, clock, id, lease.Token, until.String, lease.Attempts)
	} else {
		write, err = tx.ExecContext(ctx, retryOutboxSQL, now.Add(delay).Format(mysqlMicrosecondLayout), string(reason), id, lease.Token, until.String, lease.Attempts)
	}
	if err != nil {
		return result, outboxFailure("update settlement", err)
	}
	n, err := write.RowsAffected()
	if err != nil {
		return result, outboxFailure("settlement affected rows", err)
	}
	if n == 0 {
		return result, ErrOutboxLeaseLost
	}
	if n != 1 {
		return result, ErrOutboxInvariant
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, outboxFailure("commit settlement", errors.Join(ErrOutboxCommitUnknown, err))
	}
	result = OutboxSettlement{EventID: id, Status: "completed", CompletedAt: &now}
	if !complete {
		available := now.Add(delay)
		result.Status, result.CompletedAt, result.AvailableAt = "pending", nil, &available
	}
	return result, nil
}
