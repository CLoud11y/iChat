package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"iChat/messaging"
	"iChat/models"
)

const selectConversationReaderSQL = "SELECT EXISTS(SELECT 1 FROM `user` WHERE id=? AND deleted_at IS NULL)"
const selectVisibleConversationsSQL = `SELECT c.id,c.type,c.private_user_a,c.private_user_b,c.group_id,
 c.last_seq,COALESCE(d.delivered_seq,0)
 FROM conversation c
 LEFT JOIN message_delivery_cursor d ON d.user_id=? AND d.conversation_id=c.id
 WHERE c.id>? AND (
   c.private_user_a=? OR c.private_user_b=? OR
   EXISTS(SELECT 1 FROM ` + "`group`" + ` g JOIN relation r ON r.target_id=g.id
     WHERE g.id=c.group_id AND g.deleted_at IS NULL AND r.deleted_at IS NULL
       AND r.owner_id=? AND r.type=?)
 ) ORDER BY c.id ASC LIMIT ?`

// ListConversations discovers existing committed conversations in one read-only
// snapshot, including current membership, watermarks and this user's cursor.
// It does not lock/skip rows, read Redis, or use updated_at as an incremental
// watermark. Each periodic scan must start over; pages do not share a snapshot.
func (r *MessageRepository) ListConversations(ctx context.Context, reader uint64, cursor string, limit int) (page messaging.ConversationPage, err error) {
	if ctx == nil {
		return page, messaging.ErrInvalidInput
	}
	query, err := messaging.NormalizeConversationListQuery(reader, cursor, limit)
	if err != nil {
		return page, err
	}
	if r == nil || r.db == nil {
		return page, ErrCommandRepositoryUnavailable
	}
	if err := ctx.Err(); err != nil {
		return page, err
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return page, fmt.Errorf("begin conversation snapshot: %w", err)
	}
	defer func() {
		if e := tx.Rollback(); e != nil && !errors.Is(e, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("release conversation snapshot: %w", e))
		}
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			page = messaging.ConversationPage{}
		}
	}()
	var active bool
	if err := tx.QueryRowContext(ctx, selectConversationReaderSQL, query.ReaderID).Scan(&active); err != nil {
		return page, fmt.Errorf("read conversation reader: %w", err)
	}
	if !active {
		return page, ErrConversationForbidden
	}
	rows, err := tx.QueryContext(ctx, selectVisibleConversationsSQL,
		query.ReaderID, query.Cursor, query.ReaderID, query.ReaderID, query.ReaderID, models.GroupRelation, query.Limit+1)
	if err != nil {
		return page, fmt.Errorf("read visible conversations: %w", err)
	}
	defer func() {
		if e := rows.Close(); e != nil {
			err = errors.Join(err, fmt.Errorf("close conversation rows: %w", e))
		}
	}()
	page = messaging.ConversationPage{Items: make([]messaging.ConversationSummary, 0, query.Limit), NextCursor: query.Cursor}
	previous, count := query.Cursor, 0
	for rows.Next() {
		var id string
		var kind messaging.TargetType
		var low, high, group sql.Null[uint64]
		var lastSeq, delivered uint64
		if err := rows.Scan(&id, &kind, &low, &high, &group, &lastSeq, &delivered); err != nil {
			return page, fmt.Errorf("scan conversation: %w", err)
		}
		conversation, e := messaging.ParseConversationID(id)
		if e != nil || id <= previous || delivered > lastSeq || kind != conversation.Type ||
			(kind == messaging.Private && (!low.Valid || !high.Valid || group.Valid || low.V != conversation.LowUser || high.V != conversation.HighUser || (reader != low.V && reader != high.V))) ||
			(kind == messaging.Group && (low.Valid || high.Valid || !group.Valid || group.V != conversation.GroupID)) {
			return page, ErrConversationInvariant
		}
		count++
		if count > query.Limit+1 {
			return page, ErrConversationInvariant
		}
		previous = id
		if count > query.Limit {
			page.HasMore = true
			continue // The bounded lookahead is validated but is not returned.
		}
		page.Items = append(page.Items, messaging.ConversationSummary{ConversationID: id, LastSeq: messaging.Sequence(lastSeq), DeliveredSeq: messaging.Sequence(delivered)})
		page.NextCursor = id
	}
	if err := rows.Err(); err != nil {
		return page, fmt.Errorf("iterate conversations: %w", err)
	}
	return page, nil
}
