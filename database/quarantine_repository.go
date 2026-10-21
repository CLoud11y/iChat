package database

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"iChat/messaging/rabbitmq"

	mysql "github.com/go-sql-driver/mysql"
)

var (
	ErrQuarantineRepositoryUnavailable = errors.New("quarantine repository unavailable")
	ErrQuarantineInvariant             = errors.New("quarantine audit invariant failed")
	ErrQuarantineConflict              = errors.New("quarantine transport content conflict")
	ErrQuarantineCommitUnknown         = errors.New("quarantine audit commit outcome unknown")
	ErrQuarantineRollbackUnknown       = errors.New("quarantine audit rollback outcome unknown")
)

type QuarantineAuditDisposition string

const (
	QuarantineInserted    QuarantineAuditDisposition = "inserted"
	QuarantineExisting    QuarantineAuditDisposition = "existing"
	quarantineAuditDetail                            = "QUARANTINE_AUDIT_V1"
)

type QuarantineAuditResult struct {
	TransportID string
	Disposition QuarantineAuditDisposition
}

// QuarantineRepository borrows the pool. It never owns MQ deliveries, pool
// shutdown, retries, business writes or resolved status. RecordReplay is a
// separate, explicitly invoked transition after confirmed publication.
type QuarantineRepository struct{ db *sql.DB }

func NewQuarantineRepository(db *sql.DB) (*QuarantineRepository, error) {
	if db == nil {
		return nil, ErrQuarantineRepositoryUnavailable
	}
	return &QuarantineRepository{db: db}, nil
}

type quarantineStoreError struct {
	stage string
	cause error
}

func (e *quarantineStoreError) Error() string    { return "quarantine audit store: " + e.stage }
func (e *quarantineStoreError) GoString() string { return e.Error() }
func (e *quarantineStoreError) Unwrap() error    { return e.cause }
func quarantineStoreFailure(stage string, cause error) error {
	return &quarantineStoreError{stage, cause}
}

const insertQuarantineAuditSQL = "INSERT INTO message_quarantine (transport_id,sender_id,client_message_id,command_id,payload_hash,raw_body,raw_headers,error_code,error_detail,state,created_at) VALUES (?,?,?,?,?,?,?,?,?,'open',?)"
const readQuarantineAuditSQL = "SELECT transport_id,sender_id,client_message_id,command_id,payload_hash,raw_body,raw_headers,error_code,error_detail,state,replay_attempt_id,DATE_FORMAT(created_at,'%Y-%m-%d %H:%i:%s.%f'),DATE_FORMAT(resolved_at,'%Y-%m-%d %H:%i:%s.%f') FROM message_quarantine WHERE transport_id=? FOR SHARE"

type quarantineAuditRow struct {
	id                            string
	sender, client, command, hash sql.NullString
	body, document                []byte
	code, detail, state           string
	replay                        sql.NullString
	created                       string
	resolved                      sql.NullString
}

// Save returns success only after a new transaction commits or an existing
// committed record is fully verified and the read transaction rolls back.
func (r *QuarantineRepository) Save(ctx context.Context, input rabbitmq.QuarantineInput) (result QuarantineAuditResult, err error) {
	if r == nil || r.db == nil {
		return result, ErrQuarantineRepositoryUnavailable
	}
	if ctx == nil {
		return result, rabbitmq.ErrQuarantineInput
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	document, err := input.EncodeAudit()
	if err != nil {
		return result, err
	}
	created := input.ReceivedAt().UTC().Truncate(time.Microsecond)
	if created.Year() < 1000 || created.Year() > 9999 {
		return result, rabbitmq.ErrQuarantineInput
	}
	source, err := input.Envelope().Source()
	if err != nil {
		return result, err
	}
	identity, identified := input.Identity()
	var sender, client, command, hash any
	if identified {
		value, parseErr := strconv.ParseUint(identity.SenderID, 10, 64)
		if parseErr != nil {
			return result, rabbitmq.ErrQuarantineInput
		}
		sender, client, command, hash = value, identity.ClientMessageID, identity.CommandID, identity.PayloadHash
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return result, quarantineStoreFailure("begin", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, quarantineStoreFailure("rollback", errors.Join(ErrQuarantineRollbackUnknown, rollbackErr)))
		}
		if err != nil {
			result = QuarantineAuditResult{}
		}
	}()
	// JSON is UTF-8 text; the MySQL driver's interpolated []byte parameter has
	// binary character set semantics and cannot be assigned to a JSON column.
	write, writeErr := tx.ExecContext(ctx, insertQuarantineAuditSQL, input.Envelope().TransportID(), sender, client, command, hash, source.Body, string(document), input.Envelope().ErrorCode(), quarantineAuditDetail, created.Format(mysqlMicrosecondLayout))
	inserted := writeErr == nil
	if !inserted {
		// Accept only the driver's direct, definite duplicate error. Mixed or
		// unknown errors are not evidence that an existing record is durable.
		duplicate, ok := writeErr.(*mysql.MySQLError)
		if !ok || duplicate.Number != 1062 {
			return result, quarantineStoreFailure("insert", writeErr)
		}
	} else {
		count, countErr := write.RowsAffected()
		if countErr != nil {
			return result, quarantineStoreFailure("affected rows", countErr)
		}
		if count != 1 {
			return result, ErrQuarantineInvariant
		}
	}
	var row quarantineAuditRow
	err = tx.QueryRowContext(ctx, readQuarantineAuditSQL, input.Envelope().TransportID()).Scan(&row.id, &row.sender, &row.client, &row.command, &row.hash, &row.body, &row.document, &row.code, &row.detail, &row.state, &row.replay, &row.created, &row.resolved)
	if err != nil {
		return result, quarantineStoreFailure("read", err)
	}
	stored, err := validateQuarantineAudit(row)
	if err != nil {
		return result, err
	}
	a, _ := stored.Envelope().StableContent()
	b, _ := input.Envelope().StableContent()
	if row.id != input.Envelope().TransportID() || stored.Envelope().TransportID() != row.id || !bytes.Equal(a, b) {
		return result, ErrQuarantineConflict
	}
	if inserted {
		saved, encodeErr := stored.EncodeAudit()
		if encodeErr != nil || !bytes.Equal(saved, document) || row.state != "open" || row.replay.Valid || row.resolved.Valid {
			return result, ErrQuarantineInvariant
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if !inserted {
		// No writes occurred on duplicate; the shared locking read sees the
		// latest committed row without converting duplicate shared locks to X.
		return QuarantineAuditResult{row.id, QuarantineExisting}, nil
	}
	if err := tx.Commit(); err != nil {
		return result, quarantineStoreFailure("commit", errors.Join(ErrQuarantineCommitUnknown, err))
	}
	return QuarantineAuditResult{row.id, QuarantineInserted}, nil
}

func validateQuarantineAudit(row quarantineAuditRow) (rabbitmq.QuarantineInput, error) {
	input, err := rabbitmq.DecodeQuarantineAudit(row.document)
	if err != nil {
		return rabbitmq.QuarantineInput{}, ErrQuarantineInvariant
	}
	source, err := input.Envelope().Source()
	if err != nil || !bytes.Equal(source.Body, row.body) || row.code != input.Envelope().ErrorCode() || row.detail != quarantineAuditDetail || row.created != input.ReceivedAt().UTC().Truncate(time.Microsecond).Format(mysqlMicrosecondLayout) {
		return rabbitmq.QuarantineInput{}, ErrQuarantineInvariant
	}
	identity, ok := input.Identity()
	want := []sql.NullString{{String: identity.SenderID, Valid: ok}, {String: identity.ClientMessageID, Valid: ok}, {String: identity.CommandID, Valid: ok}, {String: identity.PayloadHash, Valid: ok}}
	got := []sql.NullString{row.sender, row.client, row.command, row.hash}
	for i := range want {
		if got[i] != want[i] {
			return rabbitmq.QuarantineInput{}, ErrQuarantineInvariant
		}
	}
	// Preserve future CLI state. These checks reject malformed rows without
	// reopening a resolved record or replacing an independent replay attempt.
	if row.replay.Valid {
		if !quarantineReplayID(row.replay.String) {
			return rabbitmq.QuarantineInput{}, ErrQuarantineInvariant
		}
	}
	switch row.state {
	case "open":
		if row.resolved.Valid {
			return rabbitmq.QuarantineInput{}, ErrQuarantineInvariant
		}
	case "replay_pending":
		if !row.replay.Valid || row.resolved.Valid {
			return rabbitmq.QuarantineInput{}, ErrQuarantineInvariant
		}
	case "resolved":
		stamp, parseErr := time.Parse(mysqlMicrosecondLayout, row.resolved.String)
		if !row.resolved.Valid || parseErr != nil || stamp.Year() < 1000 {
			return rabbitmq.QuarantineInput{}, ErrQuarantineInvariant
		}
	default:
		return rabbitmq.QuarantineInput{}, ErrQuarantineInvariant
	}
	return input, nil
}

func quarantineReplayID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
