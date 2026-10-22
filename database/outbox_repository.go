package database

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"iChat/messaging"
)

var (
	ErrOutboxUnavailable     = errors.New("outbox repository unavailable")
	ErrOutboxClaimInput      = errors.New("invalid outbox claim options")
	ErrOutboxInvariant       = errors.New("outbox record invariant failed")
	ErrOutboxCommitUnknown   = errors.New("outbox claim commit outcome unknown")
	ErrOutboxRollbackUnknown = errors.New("outbox claim rollback outcome unknown")
)

// OutboxRepository borrows the pool. Claim does not publish, settle leases,
// update business messages/cursors, retry automatically or close the pool.
type OutboxRepository struct{ db *sql.DB }

func NewOutboxRepository(pool *sql.DB) (*OutboxRepository, error) {
	if pool == nil {
		return nil, ErrOutboxUnavailable
	}
	return &OutboxRepository{pool}, nil
}

type OutboxClaimOptions struct {
	Limit int
	Lease time.Duration
}

func (o OutboxClaimOptions) Validate() error {
	if o.Limit < 1 || o.Limit > 100 || o.Lease < time.Second || o.Lease > 5*time.Minute || o.Lease%time.Microsecond != 0 {
		return ErrOutboxClaimInput
	}
	return nil
}

// Payload is the unchanged stored JSON, not yet a validated notification or an
// authorization grant. Dispatch must decode and authorize it separately.
type OutboxLease struct {
	EventID                   string
	EventType                 messaging.OutboxEventType
	CommandID                 string
	ConversationID, MessageID *string
	Payload                   []byte
	Token                     string
	Until                     time.Time
	Attempts                  uint32
}

func (OutboxLease) String() string     { return "outbox lease (payload redacted)" }
func (l OutboxLease) GoString() string { return l.String() }

type outboxStoreError struct {
	stage string
	cause error
}

func (e *outboxStoreError) Error() string           { return "outbox store: " + e.stage }
func (e *outboxStoreError) GoString() string        { return e.Error() }
func (e *outboxStoreError) Unwrap() error           { return e.cause }
func outboxFailure(stage string, cause error) error { return &outboxStoreError{stage, cause} }

const selectOutboxClaimSQL = `SELECT event_id,event_type,command_id,conversation_id,message_id,payload,status,
 DATE_FORMAT(available_at,'%Y-%m-%d %H:%i:%s.%f'),lease_token,DATE_FORMAT(lease_until,'%Y-%m-%d %H:%i:%s.%f'),attempts,
 DATE_FORMAT(created_at,'%Y-%m-%d %H:%i:%s.%f'),DATE_FORMAT(completed_at,'%Y-%m-%d %H:%i:%s.%f')
 FROM message_outbox WHERE (status='pending' AND available_at<=?) OR (status='leased' AND lease_until<=?)
 ORDER BY available_at,created_at,event_id LIMIT ? FOR UPDATE SKIP LOCKED`
const updateOutboxClaimSQL = "UPDATE message_outbox SET status='leased',lease_token=?,lease_until=?,attempts=? WHERE event_id=? AND status=? AND lease_token <=> ? AND completed_at IS NULL"

type outboxClaimRow struct {
	id, kind, command     string
	conversation, message sql.NullString
	payload               []byte
	status, available     string
	token, until          sql.NullString
	attempts              uint64
	created               string
	completed             sql.NullString
}

// Claim uses a short READ COMMITTED transaction and one database UTC clock
// sample. All selected rows commit together; any uncertainty returns no leases.
// Expired leases are eligible again, with a fresh token and incremented attempt.
func (r *OutboxRepository) Claim(ctx context.Context, o OutboxClaimOptions) (leases []OutboxLease, err error) {
	if r == nil || r.db == nil {
		return nil, ErrOutboxUnavailable
	}
	if ctx == nil {
		return nil, ErrOutboxClaimInput
	}
	if err := o.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, outboxFailure("begin claim", err)
	}
	defer func() {
		if e := tx.Rollback(); e != nil && !errors.Is(e, sql.ErrTxDone) {
			err = errors.Join(err, outboxFailure("rollback claim", errors.Join(ErrOutboxRollbackUnknown, e)))
		}
		if err != nil {
			leases = nil
		}
	}()
	var clock string
	if err := tx.QueryRowContext(ctx, "SELECT DATE_FORMAT(UTC_TIMESTAMP(6),'%Y-%m-%d %H:%i:%s.%f')").Scan(&clock); err != nil {
		return nil, outboxFailure("claim clock", err)
	}
	now, err := time.Parse(mysqlMicrosecondLayout, clock)
	if err != nil || now.Year() < 1000 || now.Add(o.Lease).Year() > 9999 {
		return nil, ErrOutboxInvariant
	}
	rows, err := tx.QueryContext(ctx, selectOutboxClaimSQL, clock, clock, o.Limit)
	if err != nil {
		return nil, outboxFailure("select claim", err)
	}
	var selected []outboxClaimRow
	for rows.Next() {
		var row outboxClaimRow
		if e := rows.Scan(&row.id, &row.kind, &row.command, &row.conversation, &row.message, &row.payload, &row.status, &row.available, &row.token, &row.until, &row.attempts, &row.created, &row.completed); e != nil {
			err = outboxFailure("scan claim", e)
			break
		}
		selected = append(selected, row)
	}
	if e := rows.Err(); e != nil {
		err = errors.Join(err, outboxFailure("iterate claim", e))
	}
	if e := rows.Close(); e != nil {
		err = errors.Join(err, outboxFailure("close claim rows", e))
	}
	if err != nil {
		return nil, err
	}
	if len(selected) > o.Limit {
		return nil, ErrOutboxInvariant
	}
	leases = make([]OutboxLease, 0, len(selected))
	for _, row := range selected {
		if err := validateOutboxClaim(row, now); err != nil {
			return nil, err
		}
		token, err := outboxLeaseToken()
		if err != nil {
			return nil, outboxFailure("lease entropy", err)
		}
		until := now.Add(o.Lease)
		var previous any
		if row.token.Valid {
			previous = row.token.String
		}
		write, err := tx.ExecContext(ctx, updateOutboxClaimSQL, token, until.Format(mysqlMicrosecondLayout), row.attempts+1, row.id, row.status, previous)
		if err != nil {
			return nil, outboxFailure("update claim", err)
		}
		n, err := write.RowsAffected()
		if err != nil {
			return nil, outboxFailure("claim affected rows", err)
		}
		if n != 1 {
			return nil, ErrOutboxInvariant
		}
		leases = append(leases, OutboxLease{EventID: row.id, EventType: messaging.OutboxEventType(row.kind), CommandID: row.command, ConversationID: outboxNullable(row.conversation), MessageID: outboxNullable(row.message), Payload: append([]byte(nil), row.payload...), Token: token, Until: until, Attempts: uint32(row.attempts + 1)})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(selected) == 0 {
		return leases, nil
	} // no writes; defer releases read transaction
	if err := tx.Commit(); err != nil {
		return nil, outboxFailure("commit claim", errors.Join(ErrOutboxCommitUnknown, err))
	}
	return leases, nil
}

func validateOutboxClaim(row outboxClaimRow, now time.Time) error {
	id, e := messaging.OutboxEventID(row.command, messaging.OutboxEventType(row.kind))
	available, a := time.Parse(mysqlMicrosecondLayout, row.available)
	created, c := time.Parse(mysqlMicrosecondLayout, row.created)
	if e != nil || id != row.id || a != nil || c != nil || available.Year() < 1000 || created.Year() < 1000 || row.completed.Valid || row.attempts >= uint64(1<<32-1) || len(row.payload) == 0 || len(row.payload) > 1024*1024 || !json.Valid(row.payload) {
		return ErrOutboxInvariant
	}
	if row.status == "pending" {
		if row.token.Valid || row.until.Valid || available.After(now) {
			return ErrOutboxInvariant
		}
	} else if row.status == "leased" {
		until, e := time.Parse(mysqlMicrosecondLayout, row.until.String)
		if !row.token.Valid || !messaging.ValidPublishAttemptID(row.token.String) || !row.until.Valid || e != nil || until.Year() < 1000 || until.After(now) || row.attempts == 0 {
			return ErrOutboxInvariant
		}
	} else {
		return ErrOutboxInvariant
	}
	return nil
}

func outboxLeaseToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b[:])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:], nil
}

func outboxNullable(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}
