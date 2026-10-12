package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iChat/messaging"
	"strconv"
	"time"
)

const selectStatusCommandSQL = `SELECT sender_id,client_message_id,command_id,payload_hash,state,
 reason_code,message_id,conversation_id,
 DATE_FORMAT(created_at,'%Y-%m-%d %H:%i:%s.%f'),DATE_FORMAT(completed_at,'%Y-%m-%d %H:%i:%s.%f')
 FROM message_command WHERE sender_id=? AND client_message_id=?`

const selectStatusMessageSQL = `SELECT m.id,m.client_message_id,m.sender_id,m.receiver_id,m.type,m.media,m.content,m.conversation_id,m.seq,
 DATE_FORMAT(m.server_created_at,'%Y-%m-%d %H:%i:%s.%f')
 FROM message m JOIN conversation c ON c.id=m.conversation_id AND m.seq<=c.last_seq
 WHERE m.id=? AND m.sender_id=? AND m.client_message_id=? AND m.conversation_id=?`

const selectStatusQuarantineSQL = `SELECT 1 FROM message_quarantine
 WHERE sender_id=? AND client_message_id=? AND payload_hash=?
 AND state IN ('open','replay_pending') AND resolved_at IS NULL LIMIT 1`

// GetCommandStatus reads only the authenticated sender's logical command. All
// reads share one non-locking, read-only repeatable-read snapshot. Absence is
// unknown, not proof that MQ has no delivery. This method never polls MQ/Redis,
// changes a command, or rechecks historical permissions for a terminal result.
func (r *MessageRepository) GetCommandStatus(ctx context.Context, authenticatedSender uint64, clientMessageID, payloadHash string) (status messaging.CommandStatus, err error) {
	if r == nil || r.db == nil {
		return messaging.CommandStatus{}, ErrCommandRepositoryUnavailable
	}
	query, err := messaging.NormalizeStatusQuery(authenticatedSender, clientMessageID, payloadHash)
	if err != nil {
		return messaging.CommandStatus{}, err
	}
	if err := ctx.Err(); err != nil {
		return messaging.CommandStatus{}, err
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return messaging.CommandStatus{}, fmt.Errorf("begin command status snapshot: %w", err)
	}
	defer func() {
		// No writes: release the snapshot without introducing a needless COMMIT.
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("release command status snapshot: %w", rollbackErr))
		}
		if err != nil {
			status = messaging.CommandStatus{}
		}
	}()
	status = messaging.CommandStatus{ClientMessageID: query.ClientMessageID, PayloadHash: query.PayloadHash}
	record, readErr := scanCommandRow(tx.QueryRowContext(ctx, selectStatusCommandSQL, query.SenderID, query.ClientMessageID))
	if readErr == nil {
		storedQuery, validationErr := messaging.NormalizeStatusQuery(record.SenderID, record.ClientMessageID, record.PayloadHash)
		if validationErr != nil || record.SenderID != query.SenderID || record.ClientMessageID != query.ClientMessageID ||
			storedQuery.ClientMessageID != record.ClientMessageID || storedQuery.CommandID != record.CommandID {
			return messaging.CommandStatus{}, ErrCommandInvariant
		}
		if err := validateStoredCommand(record); err != nil {
			return messaging.CommandStatus{}, err
		}
		if record.PayloadHash != query.PayloadHash {
			status.State = messaging.StatusConflict
			return status, nil
		}
		if record.State == CommandRejected {
			status.State, status.ReasonCode = messaging.StatusRejected, record.ReasonCode
			return status, nil
		}
		message, err := readStatusMessage(ctx, tx, record)
		if err != nil {
			return messaging.CommandStatus{}, err
		}
		status.State, status.Message = messaging.StatusPersisted, &message
		return status, nil
	}
	if !errors.Is(readErr, sql.ErrNoRows) {
		return messaging.CommandStatus{}, fmt.Errorf("read command status: %w", readErr)
	}
	var exists int
	err = tx.QueryRowContext(ctx, selectStatusQuarantineSQL, query.SenderID, query.ClientMessageID, query.PayloadHash).Scan(&exists)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return messaging.CommandStatus{}, fmt.Errorf("read command quarantine status: %w", err)
	}
	status.State = messaging.StatusPendingOrUnknown
	if err == nil {
		status.State = messaging.StatusQuarantined
	}
	status.PollAfterMs = messaging.DefaultStatusPollAfterMs
	return status, nil
}

func readStatusMessage(ctx context.Context, tx *sql.Tx, record CommandResult) (messaging.MessageDTO, error) {
	var messageID, clientID, conversationID, content, createdText string
	var sender, target, seq uint64
	var kind messaging.TargetType
	var media messaging.Media
	err := tx.QueryRowContext(ctx, selectStatusMessageSQL, record.MessageID, record.SenderID, record.ClientMessageID, record.ConversationID).Scan(
		&messageID, &clientID, &sender, &target, &kind, &media, &content, &conversationID, &seq, &createdText)
	if errors.Is(err, sql.ErrNoRows) {
		return messaging.MessageDTO{}, fmt.Errorf("%w: persisted message missing or inconsistent", ErrCommandInvariant)
	}
	if err != nil {
		return messaging.MessageDTO{}, fmt.Errorf("read persisted status message: %w", err)
	}
	command, err := messaging.Normalize(sender, messaging.SendRequest{ClientMessageID: clientID, TargetType: kind,
		TargetID: strconv.FormatUint(target, 10), Media: media, Content: content})
	if err != nil || sender != record.SenderID || clientID != record.ClientMessageID || messageID != record.CommandID ||
		command.CommandID != record.CommandID || command.PayloadHash != record.PayloadHash || command.ConversationID != conversationID || conversationID != record.ConversationID {
		return messaging.MessageDTO{}, ErrCommandInvariant
	}
	createdAt, err := time.Parse(mysqlMicrosecondLayout, createdText)
	if err != nil {
		return messaging.MessageDTO{}, fmt.Errorf("%w: persisted message timestamp", ErrCommandInvariant)
	}
	message, err := messaging.NewMessageDTO(command, messaging.Sequence(seq), createdAt)
	if err != nil {
		return messaging.MessageDTO{}, fmt.Errorf("%w: persisted message fields", ErrCommandInvariant)
	}
	return message, nil
}
