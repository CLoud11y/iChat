package database

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"iChat/messaging"
)

var (
	ErrQuarantineResolveInput     = errors.New("quarantine audit has no validated command identity")
	ErrQuarantineResolveChanged   = errors.New("quarantine resolution selection changed")
	ErrQuarantineTerminalPending  = errors.New("quarantine command has no verified terminal result")
	ErrQuarantineTerminalConflict = errors.New("quarantine command terminal identity or payload conflict")
)

type QuarantineResolveSelection struct {
	TransportID     string
	PreviousAttempt string
}

type QuarantineResolveResult struct {
	TransportID     string       `json:"transport_id"`
	CommandID       string       `json:"command_id"`
	PayloadHash     string       `json:"payload_hash"`
	TerminalState   CommandState `json:"terminal_state"`
	ReasonCode      string       `json:"reason_code,omitempty"`
	ReplayAttemptID *string      `json:"replay_attempt_id"`
	Outcome         string       `json:"resolution_outcome"`
	ResolvedAt      *time.Time   `json:"resolved_at,omitempty"`
}

const updateQuarantineResolvedSQL = "UPDATE message_quarantine SET state='resolved',resolved_at=? WHERE transport_id=? AND state=? AND replay_attempt_id <=> ? AND resolved_at IS NULL"

// Resolve verifies the canonical terminal command and the audit in the same
// transaction. It never publishes, rewrites a business result or clears an
// attempt. An already resolved row is reverified without changing its timestamp.
func (r *QuarantineRepository) Resolve(ctx context.Context, selection QuarantineResolveSelection) (result QuarantineResolveResult, err error) {
	if err := r.queryReady(ctx); err != nil {
		return result, err
	}
	if !ValidQuarantineTransportID(selection.TransportID) || (selection.PreviousAttempt != "" && !messaging.ValidPublishAttemptID(selection.PreviousAttempt)) {
		return result, ErrQuarantineQuery
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return result, quarantineStoreFailure("begin resolution", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, quarantineStoreFailure("rollback resolution", errors.Join(ErrQuarantineRollbackUnknown, rollbackErr)))
		}
		if err != nil && result.TransportID != "" {
			result.Outcome, result.ResolvedAt = "unconfirmed", nil
		}
	}()
	var row quarantineAuditRow
	err = tx.QueryRowContext(ctx, strings.TrimSuffix(readQuarantineAuditSQL, " FOR SHARE")+" FOR UPDATE", selection.TransportID).Scan(&row.id, &row.sender, &row.client, &row.command, &row.hash, &row.body, &row.document, &row.code, &row.detail, &row.state, &row.replay, &row.created, &row.resolved)
	if errors.Is(err, sql.ErrNoRows) {
		return result, ErrQuarantineNotFound
	}
	if err != nil {
		return result, quarantineStoreFailure("read resolution audit", err)
	}
	input, err := validateQuarantineAudit(row)
	if err != nil {
		return result, err
	}
	if row.id != selection.TransportID || input.Envelope().TransportID() != row.id {
		return result, ErrQuarantineConflict
	}
	if row.replay.String != selection.PreviousAttempt {
		return result, ErrQuarantineResolveChanged
	}
	identity, identified := input.Identity()
	if !identified {
		return result, ErrQuarantineResolveInput
	}
	sender, parseErr := strconv.ParseUint(identity.SenderID, 10, 64)
	if parseErr != nil {
		return result, ErrQuarantineInvariant
	}
	// A locking read sees a committed terminal, including a writer that was
	// finishing while this transaction started. No quarantine/status fallback
	// or publisher receipt can stand in for this record.
	record, readErr := scanCommandRow(tx.QueryRowContext(ctx, selectStatusCommandSQL+" FOR SHARE", sender, identity.ClientMessageID))
	if errors.Is(readErr, sql.ErrNoRows) {
		return result, ErrQuarantineTerminalPending
	}
	if readErr != nil {
		return result, quarantineStoreFailure("read resolution terminal", readErr)
	}
	if record.SenderID != sender || record.ClientMessageID != identity.ClientMessageID || record.CommandID != identity.CommandID || record.PayloadHash != identity.PayloadHash {
		return result, ErrQuarantineTerminalConflict
	}
	if err := validateStoredCommand(record); err != nil {
		return result, err
	}
	if record.State == CommandPersisted {
		if _, err := readStatusMessage(ctx, tx, record); err != nil {
			return result, quarantineStoreFailure("verify resolution message", err)
		}
	}
	result = QuarantineResolveResult{TransportID: row.id, CommandID: record.CommandID, PayloadHash: record.PayloadHash,
		TerminalState: record.State, ReasonCode: record.ReasonCode, ReplayAttemptID: quarantineNullable(row.replay), Outcome: "unconfirmed"}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if row.state == "resolved" {
		stamp, _ := time.Parse(mysqlMicrosecondLayout, row.resolved.String) // validated above
		result.Outcome, result.ResolvedAt = "already_resolved", &stamp
		return result, nil // read-only verification; defer joins rollback
	}
	stamp := time.Now().UTC().Truncate(time.Microsecond)
	var previous any
	if row.replay.Valid {
		previous = row.replay.String
	}
	write, err := tx.ExecContext(ctx, updateQuarantineResolvedSQL, stamp.Format(mysqlMicrosecondLayout), row.id, row.state, previous)
	if err != nil {
		return result, quarantineStoreFailure("update resolution", err)
	}
	count, err := write.RowsAffected()
	if err != nil {
		return result, quarantineStoreFailure("resolution affected rows", err)
	}
	if count != 1 {
		return result, ErrQuarantineResolveChanged
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, quarantineStoreFailure("commit resolution", errors.Join(ErrQuarantineCommitUnknown, err))
	}
	result.Outcome, result.ResolvedAt = "resolved", &stamp
	return result, nil
}
