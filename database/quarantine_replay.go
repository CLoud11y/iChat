package database

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"

	"iChat/messaging"
	"iChat/messaging/rabbitmq"
)

var ErrQuarantineReplayChanged = errors.New("quarantine replay selection changed or resolved")

const updateQuarantineReplaySQL = "UPDATE message_quarantine SET state='replay_pending',replay_attempt_id=? WHERE transport_id=? AND state=? AND replay_attempt_id <=> ? AND resolved_at IS NULL"

// RecordReplay records a confirmed physical attempt, never a business terminal
// result. The caller owns publication; no SQL transaction is held across MQ.
// A stale snapshot cannot replace another replay or reopen a resolved record.
func (r *QuarantineRepository) RecordReplay(ctx context.Context, expected QuarantineDetail, confirmed rabbitmq.PublishResult) (err error) {
	if err := r.queryReady(ctx); err != nil {
		return err
	}
	identity, identified := expected.Input.Identity()
	document, encodeErr := expected.Input.EncodeAudit()
	if encodeErr != nil || !identified || expected.Summary.TransportID != expected.Input.Envelope().TransportID() ||
		confirmed.Outcome != rabbitmq.PublishAccepted || confirmed.CommandID != identity.CommandID ||
		!messaging.ValidPublishAttemptID(confirmed.PublishAttemptID) || confirmed.ConnectionEpoch == 0 || confirmed.ChannelEpoch == 0 || confirmed.DeliveryTag == 0 {
		return ErrQuarantineQuery
	}
	if expected.Summary.State != "open" && expected.Summary.State != "replay_pending" {
		return ErrQuarantineReplayChanged
	}
	source, _ := expected.Input.Envelope().Source()
	wire, decodeErr := messaging.DecodeEnvelope(source.Body)
	if decodeErr != nil || wire.PublishAttemptID == confirmed.PublishAttemptID || (expected.Summary.ReplayAttemptID != nil && *expected.Summary.ReplayAttemptID == confirmed.PublishAttemptID) {
		return ErrQuarantineQuery
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return quarantineStoreFailure("begin replay", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, quarantineStoreFailure("rollback replay", errors.Join(ErrQuarantineRollbackUnknown, rollbackErr)))
		}
	}()
	var row quarantineAuditRow
	err = tx.QueryRowContext(ctx, strings.TrimSuffix(readQuarantineAuditSQL, " FOR SHARE")+" FOR UPDATE", expected.Summary.TransportID).Scan(&row.id, &row.sender, &row.client, &row.command, &row.hash, &row.body, &row.document, &row.code, &row.detail, &row.state, &row.replay, &row.created, &row.resolved)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrQuarantineNotFound
	}
	if err != nil {
		return quarantineStoreFailure("read replay", err)
	}
	stored, err := validateQuarantineAudit(row)
	if err != nil {
		return err
	}
	if row.id != expected.Summary.TransportID || stored.Envelope().TransportID() != row.id {
		return ErrQuarantineConflict
	}
	storedDocument, _ := stored.EncodeAudit()
	if !bytes.Equal(document, storedDocument) {
		return ErrQuarantineConflict
	}
	var previous any
	if expected.Summary.ReplayAttemptID != nil {
		previous = *expected.Summary.ReplayAttemptID
	}
	if row.state != expected.Summary.State || row.resolved.Valid || row.replay.Valid != (previous != nil) || (row.replay.Valid && row.replay.String != previous) {
		return ErrQuarantineReplayChanged
	}
	result, err := tx.ExecContext(ctx, updateQuarantineReplaySQL, confirmed.PublishAttemptID, row.id, row.state, previous)
	if err != nil {
		return quarantineStoreFailure("update replay", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return quarantineStoreFailure("replay affected rows", err)
	}
	if count != 1 {
		return ErrQuarantineReplayChanged
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return quarantineStoreFailure("commit replay", errors.Join(ErrQuarantineCommitUnknown, err))
	}
	return nil
}
