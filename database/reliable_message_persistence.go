package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"iChat/messaging"
	"math"
	"strconv"
	"time"
)

var (
	ErrConversationInvariant = errors.New("conversation record invariant violated")
	ErrConversationExhausted = errors.New("conversation sequence is exhausted")
)

const mysqlMicrosecondLayout = "2006-01-02 15:04:05.000000"

const claimConversationSQL = `INSERT INTO conversation
 (id,type,private_user_a,private_user_b,group_id,last_seq,updated_at)
 VALUES (?,?,?,?,?,0,UTC_TIMESTAMP(6))
 ON DUPLICATE KEY UPDATE id=conversation.id`

const lockConversationSQL = `SELECT type,private_user_a,private_user_b,group_id,last_seq,
 DATE_FORMAT(UTC_TIMESTAMP(6),'%Y-%m-%d %H:%i:%s.%f')
 FROM conversation WHERE id=? FOR UPDATE`

const insertReliableMessageSQL = `INSERT INTO message
 (id,client_message_id,sender_id,receiver_id,type,media,content,conversation_id,seq,client_sent_at,server_created_at)
 VALUES (?,?,?,?,?,?,?,?,?,?,?)`

const advanceConversationSQL = `UPDATE conversation SET last_seq=?,updated_at=? WHERE id=? AND last_seq=?`

const insertMessageOutboxSQL = `INSERT INTO message_outbox
 (event_id,event_type,command_id,conversation_id,message_id,payload,status,available_at,created_at)
 VALUES (?,?,?,?,?,?,'pending',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6))`

// PersistCommand is the production transaction entry for a canonical command.
// It owns authorization, sequence allocation, message and Outbox writes; callers
// must not ACK until it succeeds. SQL errors have no automatic retry, and a
// failed Commit retains ExecuteCommand's explicit unknown-outcome semantics.
func (r *MessageRepository) PersistCommand(ctx context.Context, command messaging.Command) (CommandExecution, error) {
	return r.ExecuteCommand(ctx, command, func(ctx context.Context, tx CommandTransaction, command messaging.Command) (CommandDecision, error) {
		reason, err := authorizeCommand(ctx, tx, command)
		if err != nil {
			return CommandDecision{}, err
		}
		decision := CommandDecision{State: CommandRejected, ReasonCode: reason}
		var message *messaging.MessageDTO
		if reason == "" {
			stored, err := persistAuthorizedMessage(ctx, tx, command)
			if err != nil {
				return CommandDecision{}, err
			}
			message = &stored
			decision = CommandDecision{State: CommandPersisted}
		}
		if err := writeCommandOutbox(ctx, tx, command, decision, message); err != nil {
			return CommandDecision{}, err
		}
		return decision, nil
	})
}

func persistAuthorizedMessage(ctx context.Context, tx CommandTransaction, command messaging.Command) (messaging.MessageDTO, error) {
	sender, _ := strconv.ParseUint(command.SenderID, 10, 64) // ExecuteCommand validated IDs.
	target, _ := strconv.ParseUint(command.TargetID, 10, 64)
	var userA, userB, groupID any
	if command.TargetType == messaging.Private {
		low, high := sender, target
		if low > high {
			low, high = high, low
		}
		userA, userB = low, high
	} else {
		groupID = target
	}
	// The duplicate update is deliberately a no-op. It acquires the existing
	// conversation lock without changing its counter, identity or timestamp.
	if _, err := tx.ExecContext(ctx, claimConversationSQL, command.ConversationID, command.TargetType, userA, userB, groupID); err != nil {
		return messaging.MessageDTO{}, fmt.Errorf("claim conversation: %w", err)
	}
	var storedType messaging.TargetType
	var storedA, storedB, storedGroup sql.Null[uint64]
	var lastSeq uint64
	var stamp string
	if err := tx.QueryRowContext(ctx, lockConversationSQL, command.ConversationID).Scan(&storedType, &storedA, &storedB, &storedGroup, &lastSeq, &stamp); err != nil {
		return messaging.MessageDTO{}, fmt.Errorf("lock conversation: %w", err)
	}
	if storedType != command.TargetType ||
		(command.TargetType == messaging.Private && (!storedA.Valid || !storedB.Valid || storedGroup.Valid || storedA.V != userA || storedB.V != userB)) ||
		(command.TargetType == messaging.Group && (storedA.Valid || storedB.Valid || !storedGroup.Valid || storedGroup.V != target)) {
		return messaging.MessageDTO{}, ErrConversationInvariant
	}
	if lastSeq == math.MaxUint64 {
		return messaging.MessageDTO{}, ErrConversationExhausted
	}
	createdAt, err := time.Parse(mysqlMicrosecondLayout, stamp)
	if err != nil {
		return messaging.MessageDTO{}, fmt.Errorf("%w: server timestamp", ErrConversationInvariant)
	}
	seq := lastSeq + 1
	message, err := messaging.NewMessageDTO(command, messaging.Sequence(seq), createdAt)
	if err != nil {
		return messaging.MessageDTO{}, err
	}
	clientSentAt, err := clientTimeForMySQL(command.ClientSentAt)
	if err != nil {
		return messaging.MessageDTO{}, err
	}
	if _, err := tx.ExecContext(ctx, insertReliableMessageSQL, command.CommandID, command.ClientMessageID,
		sender, target, command.TargetType, command.Media, command.Content, command.ConversationID, seq, clientSentAt, stamp); err != nil {
		return messaging.MessageDTO{}, fmt.Errorf("insert reliable message: %w", err)
	}
	write, err := tx.ExecContext(ctx, advanceConversationSQL, seq, stamp, command.ConversationID, lastSeq)
	if err != nil {
		return messaging.MessageDTO{}, fmt.Errorf("advance conversation: %w", err)
	}
	count, err := write.RowsAffected()
	if err != nil {
		return messaging.MessageDTO{}, fmt.Errorf("verify conversation advance: %w", err)
	}
	if count != 1 {
		return messaging.MessageDTO{}, ErrConversationInvariant
	}
	return message, nil
}

// Bind explicit UTC clock text, not time.Time: the legacy MySQL pool uses
// loc=Local, which would otherwise convert UTC values before writing DATETIME.
// Truncate explicitly so MySQL cannot round nanoseconds into the next second.
func clientTimeForMySQL(value string) (any, error) {
	if value == "" {
		return nil, nil
	}
	stamp, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil, err
	}
	return stamp.UTC().Truncate(time.Microsecond).Format(mysqlMicrosecondLayout), nil
}

func writeCommandOutbox(ctx context.Context, tx CommandTransaction, command messaging.Command, decision CommandDecision, message *messaging.MessageDTO) error {
	if message != nil {
		id, err := messaging.OutboxEventID(command.CommandID, messaging.EventMessagePersisted)
		if err != nil {
			return err
		}
		hint := messaging.MessagePersistedEvent{SchemaVersion: messaging.SchemaVersion, EventID: id, CommandID: command.CommandID,
			ConversationID: message.ConversationID, MessageID: message.MessageID, Seq: message.Seq,
			SenderID: message.SenderID, TargetID: message.TargetID, TargetType: message.TargetType}
		if err := insertOutboxEvent(ctx, tx, command, messaging.EventMessagePersisted, id, hint, message); err != nil {
			return err
		}
	}
	id, err := messaging.OutboxEventID(command.CommandID, messaging.EventCommandResult)
	if err != nil {
		return err
	}
	result := messaging.CommandResultEvent{SchemaVersion: messaging.SchemaVersion, EventID: id, CommandID: command.CommandID,
		SenderID: command.SenderID, ClientMessageID: command.ClientMessageID, PayloadHash: command.PayloadHash,
		State: string(decision.State), ReasonCode: decision.ReasonCode, Message: message}
	return insertOutboxEvent(ctx, tx, command, messaging.EventCommandResult, id, result, message)
}

func insertOutboxEvent(ctx context.Context, tx CommandTransaction, command messaging.Command, eventType messaging.OutboxEventType, eventID string, payload any, message *messaging.MessageDTO) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode outbox event: %w", err)
	}
	var conversationID, messageID any
	if message != nil {
		conversationID, messageID = message.ConversationID, message.MessageID
	}
	// Plain INSERT: uniqueness collisions are invariant/SQL errors, not an
	// excuse to silently overwrite or omit part of a command's event set.
	if _, err := tx.ExecContext(ctx, insertMessageOutboxSQL, eventID, eventType, command.CommandID, conversationID, messageID, string(body)); err != nil {
		return fmt.Errorf("insert %s outbox: %w", eventType, err)
	}
	return nil
}
