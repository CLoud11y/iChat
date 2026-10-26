package database

import (
	"context"
	"database/sql"
	"errors"
	"strconv"

	"iChat/messaging"
	"iChat/models"
)

const selectNotificationCommandSQL = `SELECT sender_id,client_message_id,command_id,payload_hash,state,
 reason_code,message_id,conversation_id,
 DATE_FORMAT(created_at,'%Y-%m-%d %H:%i:%s.%f'),DATE_FORMAT(completed_at,'%Y-%m-%d %H:%i:%s.%f')
 FROM message_command WHERE command_id=?`

// ResolveNotification materializes a canonical frame under current permission
// locks, then releases all SQL resources before returning. Empty/nil suppresses
// missing or unauthorized hints; errors never return content. Neither Outbox
// payload nor Redis subscription is proof of entitlement. No network in SQL.
func (r *MessageRepository) ResolveNotification(ctx context.Context, reader uint64, h messaging.NotificationHint) (frame messaging.ServerFrame, err error) {
	if r == nil || r.db == nil {
		return frame, ErrCommandRepositoryUnavailable
	}
	if ctx == nil || reader == 0 || !h.Valid() {
		return frame, messaging.ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return frame, err
	}
	if !h.AllowsUser(reader) {
		return frame, nil
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return frame, notificationError("begin", err)
	}
	defer func() {
		if e := tx.Rollback(); e != nil && !errors.Is(e, sql.ErrTxDone) {
			err = errors.Join(err, notificationError("release", e))
		}
		if err == nil && ctx.Err() != nil {
			err = ctx.Err()
		}
		if err != nil {
			frame = messaging.ServerFrame{}
		}
	}()
	active, err := lockActiveIdentity(ctx, tx, "SELECT id,deleted_at IS NULL FROM `user` WHERE id=? FOR SHARE", reader)
	if err != nil {
		return frame, notificationError("reader", err)
	}
	if !active {
		return frame, nil
	}
	record, err := scanCommandRow(tx.QueryRowContext(ctx, selectNotificationCommandSQL, h.CommandID()))
	if errors.Is(err, sql.ErrNoRows) {
		return frame, nil
	}
	if err != nil {
		return frame, notificationError("command", err)
	}
	q, e := messaging.NormalizeStatusQuery(record.SenderID, record.ClientMessageID, record.PayloadHash)
	if e != nil || q.CommandID != record.CommandID || q.ClientMessageID != record.ClientMessageID || record.CommandID != h.CommandID() || validateStoredCommand(record) != nil {
		return frame, ErrCommandInvariant
	}
	if h.EventType() == messaging.EventCommandResult {
		if record.SenderID != reader {
			return frame, nil
		}
		if h.ClientMessageID() != record.ClientMessageID || h.PayloadHash() != record.PayloadHash || h.State() != string(record.State) || h.ReasonCode() != record.ReasonCode {
			return frame, ErrCommandInvariant
		}
		if record.State == CommandRejected {
			return messaging.NewCommandResultFrame(reader, messaging.CommandResultData{ClientMessageID: record.ClientMessageID, PayloadHash: record.PayloadHash, State: string(record.State), ReasonCode: record.ReasonCode})
		}
	} else if record.State != CommandPersisted {
		return frame, ErrCommandInvariant
	}
	if record.ConversationID != h.ConversationID() || record.MessageID != h.MessageID() {
		return frame, ErrCommandInvariant
	}
	conversation, e := messaging.ParseConversationID(record.ConversationID)
	if e != nil {
		return frame, ErrCommandInvariant
	}
	allowed := conversation.Type == messaging.Private && (reader == conversation.LowUser || reader == conversation.HighUser)
	if conversation.Type == messaging.Group {
		allowed, err = lockActiveIdentity(ctx, tx, "SELECT id,deleted_at IS NULL FROM `group` WHERE id=? FOR SHARE", conversation.GroupID)
		if err != nil {
			return frame, notificationError("group", err)
		}
		if allowed {
			allowed, err = lockActiveIdentity(ctx, tx, "SELECT id,deleted_at IS NULL FROM relation WHERE owner_id=? AND target_id=? AND type=? FOR SHARE", reader, conversation.GroupID, models.GroupRelation)
			if err != nil {
				return frame, notificationError("membership", err)
			}
		}
	}
	if !allowed && h.EventType() == messaging.EventMessagePersisted {
		return frame, nil
	}
	m, err := readStatusMessage(ctx, tx, record)
	if err != nil {
		return frame, notificationError("message", err)
	}
	if m.Seq != h.Sequence() {
		return frame, ErrCommandInvariant
	}
	if h.EventType() == messaging.EventMessagePersisted {
		// The user channel must be the actual private recipient, not merely
		// another participant named by a syntactically plausible hint.
		if m.TargetType == messaging.Private && m.TargetID != strconv.FormatUint(reader, 10) {
			return frame, ErrCommandInvariant
		}
		return messaging.NewMessageFrame(m)
	}
	d := messaging.CommandResultData{ClientMessageID: record.ClientMessageID, PayloadHash: record.PayloadHash, State: string(record.State), MessageID: m.MessageID, ConversationID: m.ConversationID, Seq: &m.Seq}
	if allowed {
		d.Message = &m
	}
	return messaging.NewCommandResultFrame(reader, d)
}

type notificationReadError struct {
	stage string
	cause error
}

func (e *notificationReadError) Error() string    { return "notification read: " + e.stage }
func (e *notificationReadError) GoString() string { return e.Error() }
func (e *notificationReadError) Unwrap() error    { return e.cause }
func notificationError(stage string, cause error) error {
	return &notificationReadError{stage: stage, cause: cause}
}
