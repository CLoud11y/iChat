package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iChat/messaging"
	"iChat/models"
	"strconv"
	"time"
)

var ErrConversationForbidden = errors.New("conversation read forbidden")

const selectReadMembershipSQL = "SELECT EXISTS(SELECT 1 FROM `group` g JOIN relation r ON r.target_id=g.id WHERE g.id=? AND g.deleted_at IS NULL AND r.owner_id=? AND r.type=? AND r.deleted_at IS NULL)"
const selectReadConversationSQL = `SELECT type,private_user_a,private_user_b,group_id,last_seq FROM conversation WHERE id=?`
const selectReadMessagesSQL = `SELECT m.id,m.client_message_id,m.sender_id,m.receiver_id,m.type,m.media,m.content,m.conversation_id,m.seq,
 DATE_FORMAT(m.server_created_at,'%Y-%m-%d %H:%i:%s.%f'),c.payload_hash
 FROM message m JOIN message_command c ON c.command_id=m.id AND c.message_id=m.id
 AND c.sender_id=m.sender_id AND c.client_message_id=m.client_message_id
 AND c.conversation_id=m.conversation_id AND c.state='persisted' AND c.reason_code IS NULL
 WHERE m.conversation_id=? AND m.seq<=?`
const selectSyncMessagesSQL = selectReadMessagesSQL + ` AND m.seq>? ORDER BY m.seq ASC LIMIT ?`
const selectHistoryMessagesSQL = selectReadMessagesSQL + ` ORDER BY m.seq DESC LIMIT ?`

// SyncMessages reads committed messages after the caller's local cursor. It
// never consults Redis or the server delivery cursor, including after local loss.
func (r *MessageRepository) SyncMessages(ctx context.Context, authenticatedReader uint64, conversationID, afterSeq string, limit int) (messaging.SyncPage, error) {
	query, err := messaging.NormalizePageQuery(authenticatedReader, conversationID, afterSeq, limit)
	if err != nil {
		return messaging.SyncPage{}, err
	}
	watermark, items, more, err := r.readMessagePage(ctx, query, false)
	if err != nil {
		return messaging.SyncPage{}, err
	}
	page := messaging.SyncPage{ConversationID: conversationID, AfterSeq: query.Cursor, NextAfterSeq: query.Cursor,
		Items: items, HighWatermark: watermark, HasMore: more}
	if len(items) > 0 {
		page.NextAfterSeq = items[len(items)-1].Seq
	}
	return page, nil
}

// HistoryMessages reads backwards from an exclusive cursor; an empty cursor
// starts at the snapshot's latest message. Authorization is rechecked each page.
func (r *MessageRepository) HistoryMessages(ctx context.Context, authenticatedReader uint64, conversationID, beforeSeq string, limit int) (messaging.HistoryPage, error) {
	query, err := messaging.NormalizePageQuery(authenticatedReader, conversationID, beforeSeq, limit)
	if err != nil {
		return messaging.HistoryPage{}, err
	}
	watermark, items, more, err := r.readMessagePage(ctx, query, true)
	if err != nil {
		return messaging.HistoryPage{}, err
	}
	page := messaging.HistoryPage{ConversationID: conversationID, Items: items, NextBeforeSeq: query.Cursor,
		HighWatermark: watermark, HasMore: more}
	if query.HasCursor {
		page.BeforeSeq = &query.Cursor
	}
	for low, high := 0, len(items)-1; low < high; low, high = low+1, high-1 {
		items[low], items[high] = items[high], items[low]
	}
	if len(items) > 0 {
		page.NextBeforeSeq = items[0].Seq
	}
	return page, nil
}

func (r *MessageRepository) readMessagePage(ctx context.Context, query messaging.PageQuery, history bool) (watermark messaging.Sequence, items []messaging.MessageDTO, more bool, err error) {
	if r == nil || r.db == nil {
		return 0, nil, false, ErrCommandRepositoryUnavailable
	}
	conversation := query.Conversation
	if conversation.Type == messaging.Private && query.ReaderID != conversation.LowUser && query.ReaderID != conversation.HighUser {
		return 0, nil, false, ErrConversationForbidden
	}
	if err := ctx.Err(); err != nil {
		return 0, nil, false, err
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return 0, nil, false, fmt.Errorf("begin message page snapshot: %w", err)
	}
	defer func() {
		if releaseErr := tx.Rollback(); releaseErr != nil && !errors.Is(releaseErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("release message page snapshot: %w", releaseErr))
		}
		if err != nil {
			watermark, items, more = 0, nil, false
		}
	}()
	if conversation.Type == messaging.Group {
		var member bool
		if err := tx.QueryRowContext(ctx, selectReadMembershipSQL, conversation.GroupID, query.ReaderID, models.GroupRelation).Scan(&member); err != nil {
			return 0, nil, false, fmt.Errorf("read conversation membership: %w", err)
		}
		if !member {
			return 0, nil, false, ErrConversationForbidden
		}
	}
	var kind messaging.TargetType
	var low, high, group sql.Null[uint64]
	var lastSeq uint64
	err = tx.QueryRowContext(ctx, selectReadConversationSQL, conversation.ID).Scan(&kind, &low, &high, &group, &lastSeq)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, nil, false, fmt.Errorf("read conversation watermark: %w", err)
	}
	if err == nil && (kind != conversation.Type ||
		(kind == messaging.Private && (!low.Valid || !high.Valid || group.Valid || low.V != conversation.LowUser || high.V != conversation.HighUser)) ||
		(kind == messaging.Group && (low.Valid || high.Valid || !group.Valid || group.V != conversation.GroupID))) {
		return 0, nil, false, ErrCommandInvariant
	}
	watermark = messaging.Sequence(lastSeq)
	var count, start uint64
	statement := selectSyncMessagesSQL
	args := []any{conversation.ID, lastSeq, uint64(query.Cursor), query.Limit + 1}
	if history {
		start = lastSeq
		if query.HasCursor {
			if query.Cursor == 0 {
				start = 0
			} else if uint64(query.Cursor)-1 < start {
				start = uint64(query.Cursor) - 1
			}
		}
		count = start
		statement, args = selectHistoryMessagesSQL, []any{conversation.ID, start, query.Limit + 1}
	} else {
		if uint64(query.Cursor) > lastSeq {
			return 0, nil, false, fmt.Errorf("%w: afterSeq exceeds watermark", messaging.ErrInvalidInput)
		}
		count = lastSeq - uint64(query.Cursor)
		if count > 0 {
			start = uint64(query.Cursor) + 1
		}
	}
	items = make([]messaging.MessageDTO, 0, query.Limit)
	if count == 0 {
		return watermark, items, false, nil
	}
	// Read one bounded lookahead row. Missing expected rows, including the
	// lookahead, are corruption; never silently advance across a sequence hole.
	want := min(count, uint64(query.Limit+1))
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return 0, nil, false, fmt.Errorf("read message page: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close message page rows: %w", closeErr))
		}
	}()
	var read uint64
	for rows.Next() {
		message, scanErr := scanReadMessage(rows, conversation.ID)
		if scanErr != nil {
			return 0, nil, false, scanErr
		}
		if read >= want {
			return 0, nil, false, fmt.Errorf("%w: excessive message rows", ErrCommandInvariant)
		}
		expected := start
		if history {
			expected -= read
		} else {
			expected += read
		}
		if uint64(message.Seq) != expected {
			return 0, nil, false, fmt.Errorf("%w: message sequence gap", ErrCommandInvariant)
		}
		if read < uint64(query.Limit) {
			items = append(items, message)
		}
		read++
	}
	if err := rows.Err(); err != nil {
		return 0, nil, false, fmt.Errorf("iterate message page: %w", err)
	}
	if read != want {
		return 0, nil, false, fmt.Errorf("%w: incomplete message sequence range", ErrCommandInvariant)
	}
	return watermark, items, count > uint64(query.Limit), nil
}

func scanReadMessage(row interface{ Scan(...any) error }, requestedConversation string) (messaging.MessageDTO, error) {
	var id, clientID, conversationID, content, createdText, payloadHash string
	var sender, target, seq uint64
	var kind messaging.TargetType
	var media messaging.Media
	if err := row.Scan(&id, &clientID, &sender, &target, &kind, &media, &content, &conversationID, &seq, &createdText, &payloadHash); err != nil {
		return messaging.MessageDTO{}, fmt.Errorf("scan message page: %w", err)
	}
	command, err := messaging.Normalize(sender, messaging.SendRequest{ClientMessageID: clientID, TargetType: kind,
		TargetID: strconv.FormatUint(target, 10), Media: media, Content: content})
	if err != nil || command.ClientMessageID != clientID || command.CommandID != id || command.PayloadHash != payloadHash ||
		command.ConversationID != conversationID || conversationID != requestedConversation {
		return messaging.MessageDTO{}, ErrCommandInvariant
	}
	createdAt, err := time.Parse(mysqlMicrosecondLayout, createdText)
	if err != nil {
		return messaging.MessageDTO{}, ErrCommandInvariant
	}
	message, err := messaging.NewMessageDTO(command, messaging.Sequence(seq), createdAt)
	if err != nil {
		return messaging.MessageDTO{}, ErrCommandInvariant
	}
	return message, nil
}
