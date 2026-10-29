package database

import (
	"context"
	"database/sql"
	"errors"

	"iChat/messaging"
	"iChat/models"
)

var (
	ErrDeliverySequence      = errors.New("delivery sequence exceeds committed watermark")
	ErrDeliveryCommitUnknown = errors.New("delivery ACK commit outcome unknown")
)

const advanceDeliverySQL = `INSERT INTO message_delivery_cursor
 (user_id,conversation_id,delivered_seq,updated_at) VALUES (?,?,?,UTC_TIMESTAMP(6))
 ON DUPLICATE KEY UPDATE
 updated_at=IF(delivered_seq<VALUES(delivered_seq),UTC_TIMESTAMP(6),updated_at),
 delivered_seq=GREATEST(delivered_seq,VALUES(delivered_seq))`
const selectDeliverySQL = `SELECT delivered_seq FROM message_delivery_cursor WHERE user_id=? AND conversation_id=? FOR UPDATE`

// AcknowledgeDelivery is called while the API's per-user current-session gate
// is held through this entire transaction. It never trusts Redis or a client's
// identity. Permission locks remain held until COMMIT/ROLLBACK; no network
// notification runs here. An error always returns a zero receipt, even when
// COMMIT may have succeeded; retrying the same ACK is safe.
func (r *MessageRepository) AcknowledgeDelivery(ctx context.Context, user uint64, conversationID string, seq messaging.Sequence) (committed messaging.Sequence, err error) {
	if ctx == nil || user == 0 {
		return 0, messaging.ErrInvalidInput
	}
	conversation, err := messaging.ParseConversationID(conversationID)
	if err != nil {
		return 0, err
	}
	if conversation.Type == messaging.Private && user != conversation.LowUser && user != conversation.HighUser {
		return 0, ErrConversationForbidden
	}
	if r == nil || r.db == nil {
		return 0, ErrCommandRepositoryUnavailable
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return 0, err
	}
	defer func() {
		if e := tx.Rollback(); e != nil && !errors.Is(e, sql.ErrTxDone) {
			err = errors.Join(err, e)
		}
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			committed = 0
		}
	}()
	active, err := lockActiveIdentity(ctx, tx, "SELECT id,deleted_at IS NULL FROM `user` WHERE id=? FOR SHARE", user)
	if err != nil {
		return 0, err
	}
	if !active {
		return 0, ErrConversationForbidden
	}
	if conversation.Type == messaging.Group {
		active, err = lockActiveIdentity(ctx, tx, "SELECT id,deleted_at IS NULL FROM `group` WHERE id=? FOR SHARE", conversation.GroupID)
		if err != nil {
			return 0, err
		}
		if active {
			active, err = lockActiveIdentity(ctx, tx, "SELECT id,deleted_at IS NULL FROM relation WHERE owner_id=? AND target_id=? AND type=? FOR SHARE", user, conversation.GroupID, models.GroupRelation)
			if err != nil {
				return 0, err
			}
		}
		if !active {
			return 0, ErrConversationForbidden
		}
	}
	var kind messaging.TargetType
	var low, high, group sql.Null[uint64]
	var last uint64
	err = tx.QueryRowContext(ctx, selectReadConversationSQL+" FOR SHARE", conversation.ID).Scan(&kind, &low, &high, &group, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrConversationForbidden
	}
	if err != nil {
		return 0, err
	}
	if kind != conversation.Type ||
		(kind == messaging.Private && (!low.Valid || !high.Valid || group.Valid || low.V != conversation.LowUser || high.V != conversation.HighUser)) ||
		(kind == messaging.Group && (low.Valid || high.Valid || !group.Valid || group.V != conversation.GroupID)) {
		return 0, ErrConversationInvariant
	}
	if uint64(seq) > last {
		return 0, ErrDeliverySequence
	}
	if _, err = tx.ExecContext(ctx, advanceDeliverySQL, user, conversation.ID, uint64(seq)); err != nil {
		return 0, err
	}
	var stored uint64
	if err = tx.QueryRowContext(ctx, selectDeliverySQL, user, conversation.ID).Scan(&stored); err != nil {
		return 0, err
	}
	if stored < uint64(seq) || stored > last {
		return 0, ErrConversationInvariant
	}
	if err = ctx.Err(); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, errors.Join(ErrDeliveryCommitUnknown, err)
	}
	return messaging.Sequence(stored), nil
}
