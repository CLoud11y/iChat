package database

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"iChat/messaging"
	"strconv"
	"strings"
	"time"
)

var (
	ErrCommandRepositoryUnavailable = errors.New("command repository is not initialized")
	ErrInvalidCommandDecision       = errors.New("invalid command terminal decision")
	ErrCommandInvariant             = errors.New("command record invariant violated")
	// A failed Commit never proves rollback. The caller must retain the original
	// command identity and recover by replay; it must not acknowledge a delivery.
	ErrCommandCommitUnknown = errors.New("command commit outcome is unknown")
)

type CommandState string

const (
	CommandPersisted CommandState = "persisted"
	CommandRejected  CommandState = "rejected"
)

type CommandDisposition string

const (
	CommandApplied  CommandDisposition = "applied"
	CommandReplayed CommandDisposition = "replayed"
	CommandConflict CommandDisposition = "ID_CONFLICT"
)

// CommandDecision is a trusted transaction handler's final business result.
// Permission checks and message/Outbox writes are added in P2.4/P2.5.
type CommandDecision struct {
	State      CommandState
	ReasonCode string
}

type CommandResult struct {
	SenderID        uint64
	ClientMessageID string
	CommandID       string
	PayloadHash     string
	State           CommandState
	ReasonCode      string
	MessageID       string
	ConversationID  string
	CreatedAt       time.Time
	CompletedAt     time.Time
}

// Conflict has no Result: exposing the original successful result would
// incorrectly describe the conflicting request as persisted.
type CommandExecution struct {
	Disposition CommandDisposition
	Result      *CommandResult
}

// CommandTransaction limits handlers to SQL in the repository-owned transaction.
// They must not execute DDL, transaction control, network calls or use a separate
// connection. Only the repository decides whether to commit or roll back.
type CommandTransaction interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type commandTransaction struct{ tx *sql.Tx }

func (t commandTransaction) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, query, args...)
}

func (t commandTransaction) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(ctx, query, args...)
}

func (t commandTransaction) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(ctx, query, args...)
}

type CommandHandler func(context.Context, CommandTransaction, messaging.Command) (CommandDecision, error)

// MessageRepository has no global dependencies or implicit migration/startup I/O.
type MessageRepository struct{ db *sql.DB }

func NewMessageRepository(db *sql.DB) (*MessageRepository, error) {
	if db == nil {
		return nil, ErrCommandRepositoryUnavailable
	}
	return &MessageRepository{db: db}, nil
}

const commandClaimPrefix = "__claim:"

const claimCommandSQL = `INSERT INTO message_command
 (sender_id,client_message_id,command_id,payload_hash,state,reason_code,created_at,completed_at)
 VALUES (?,?,?,?,'rejected',?,UTC_TIMESTAMP(6),UTC_TIMESTAMP(6))
 ON DUPLICATE KEY UPDATE command_id=message_command.command_id`

const lockCommandSQL = `SELECT sender_id,client_message_id,command_id,payload_hash,state,
 reason_code,message_id,conversation_id,
 DATE_FORMAT(created_at,'%Y-%m-%d %H:%i:%s.%f'),DATE_FORMAT(completed_at,'%Y-%m-%d %H:%i:%s.%f')
 FROM message_command WHERE sender_id=? AND client_message_id=? FOR UPDATE`

const finishCommandSQL = `UPDATE message_command SET state=?,reason_code=?,message_id=?,
 conversation_id=?,completed_at=UTC_TIMESTAMP(6)
 WHERE sender_id=? AND client_message_id=? AND command_id=? AND payload_hash=?
 AND state='rejected' AND reason_code=?`

// ExecuteCommand serializes a logical command by database uniqueness and a
// locking read. The handler runs only for a newly claimed key. Same-key replays
// never recheck permissions or change the original terminal result.
//
// v1's CHECK rejects processing even within a transaction. A provisional rejected
// row with a private random reason code satisfies that CHECK. It is uncommitted
// and must be replaced by a validated terminal result before Commit. Any error,
// cancellation or panic rolls it back. A committed claim marker is corruption.
//
// The duplicate branch is a no-op: it never overwrites identity, hash or result.
// Ownership uses the private marker, not RowsAffected (CLIENT_FOUND_ROWS changes
// its meaning). ON DUPLICATE KEY UPDATE takes an exclusive lock on duplicates:
// https://dev.mysql.com/doc/refman/8.0/en/innodb-locks-set.html
//
// SQL failures, including deadlocks, propagate without automatic retries; the
// future consumer owns bounded retries. A Commit error is explicitly unknown.
func (r *MessageRepository) ExecuteCommand(ctx context.Context, command messaging.Command, handler CommandHandler) (execution CommandExecution, err error) {
	if r == nil || r.db == nil {
		return CommandExecution{}, ErrCommandRepositoryUnavailable
	}
	if err := messaging.ValidateCommand(command); err != nil {
		return CommandExecution{}, err
	}
	if handler == nil {
		return CommandExecution{}, ErrInvalidCommandDecision
	}
	if err := ctx.Err(); err != nil {
		return CommandExecution{}, err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return CommandExecution{}, fmt.Errorf("generate command claim: %w", err)
	}
	claim := commandClaimPrefix + hex.EncodeToString(nonce[:])
	sender, _ := strconv.ParseUint(command.SenderID, 10, 64) // ValidateCommand checked this.
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return CommandExecution{}, fmt.Errorf("begin command transaction: %w", err)
	}
	defer func() {
		// Also runs during panic. Commit/automatic cancellation may already have
		// finished the transaction; sql.ErrTxDone adds no new information.
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rollback command transaction: %w", rollbackErr))
		}
		if err != nil {
			execution = CommandExecution{}
		}
	}()
	if _, err := tx.ExecContext(ctx, claimCommandSQL, sender, command.ClientMessageID, command.CommandID, command.PayloadHash, claim); err != nil {
		return CommandExecution{}, fmt.Errorf("claim command identity: %w", err)
	}
	record, err := readCommand(ctx, tx, sender, command.ClientMessageID)
	if err != nil {
		return CommandExecution{}, err
	}
	if record.CommandID != command.CommandID || record.SenderID != sender || record.ClientMessageID != command.ClientMessageID {
		return CommandExecution{}, ErrCommandInvariant
	}
	owned := record.State == CommandRejected && record.ReasonCode == claim
	if !owned {
		if err := validateStoredCommand(record); err != nil {
			return CommandExecution{}, err
		}
		if record.PayloadHash != command.PayloadHash {
			return CommandExecution{Disposition: CommandConflict}, nil
		}
		if record.State == CommandPersisted && record.ConversationID != command.ConversationID {
			return CommandExecution{}, ErrCommandInvariant
		}
		// No changes occurred. Release the no-op reservation with Rollback, not
		// a needless COMMIT that could introduce an unknown commit response.
		return CommandExecution{Disposition: CommandReplayed, Result: &record}, nil
	}
	if record.PayloadHash != command.PayloadHash || record.MessageID != "" || record.ConversationID != "" {
		return CommandExecution{}, ErrCommandInvariant
	}
	decision, err := handler(ctx, commandTransaction{tx: tx}, command)
	if err != nil {
		return CommandExecution{}, fmt.Errorf("apply command: %w", err)
	}
	if err := validateCommandDecision(decision); err != nil {
		return CommandExecution{}, err
	}
	var reasonCode, messageID, conversationID any
	if decision.State == CommandRejected {
		reasonCode = decision.ReasonCode
	} else {
		messageID, conversationID = command.CommandID, command.ConversationID
	}
	write, err := tx.ExecContext(ctx, finishCommandSQL, decision.State, reasonCode, messageID, conversationID,
		sender, command.ClientMessageID, command.CommandID, command.PayloadHash, claim)
	if err != nil {
		return CommandExecution{}, fmt.Errorf("complete command: %w", err)
	}
	count, err := write.RowsAffected()
	if err != nil {
		return CommandExecution{}, fmt.Errorf("verify completed command: %w", err)
	}
	if count != 1 {
		return CommandExecution{}, ErrCommandInvariant
	}
	record, err = readCommand(ctx, tx, sender, command.ClientMessageID)
	if err != nil {
		return CommandExecution{}, err
	}
	if err := validateStoredCommand(record); err != nil {
		return CommandExecution{}, err
	}
	if record.CommandID != command.CommandID || record.PayloadHash != command.PayloadHash ||
		record.State != decision.State || record.ReasonCode != decision.ReasonCode ||
		(decision.State == CommandPersisted && record.ConversationID != command.ConversationID) {
		return CommandExecution{}, ErrCommandInvariant
	}
	if err := tx.Commit(); err != nil {
		return CommandExecution{}, fmt.Errorf("%w: %w", ErrCommandCommitUnknown, err)
	}
	return CommandExecution{Disposition: CommandApplied, Result: &record}, nil
}

func readCommand(ctx context.Context, tx *sql.Tx, sender uint64, clientID string) (CommandResult, error) {
	var record CommandResult
	var reasonCode, messageID, conversationID sql.NullString
	var createdAt, completedAt string
	err := tx.QueryRowContext(ctx, lockCommandSQL, sender, clientID).Scan(
		&record.SenderID, &record.ClientMessageID, &record.CommandID, &record.PayloadHash, &record.State,
		&reasonCode, &messageID, &conversationID, &createdAt, &completedAt)
	if errors.Is(err, sql.ErrNoRows) {
		// The other unique key may belong to a different logical command.
		return CommandResult{}, ErrCommandInvariant
	}
	if err != nil {
		return CommandResult{}, fmt.Errorf("lock command result: %w", err)
	}
	record.ReasonCode, record.MessageID, record.ConversationID = reasonCode.String, messageID.String, conversationID.String
	// DATETIME has no timezone. Read explicit UTC clock text so the existing
	// legacy pool's loc=Local cannot reinterpret UTC_TIMESTAMP as local time.
	record.CreatedAt, err = time.Parse("2006-01-02 15:04:05.000000", createdAt)
	if err != nil {
		return CommandResult{}, fmt.Errorf("%w: created_at", ErrCommandInvariant)
	}
	record.CompletedAt, err = time.Parse("2006-01-02 15:04:05.000000", completedAt)
	if err != nil {
		return CommandResult{}, fmt.Errorf("%w: completed_at", ErrCommandInvariant)
	}
	return record, nil
}

func validateCommandDecision(decision CommandDecision) error {
	if decision.State == CommandPersisted && decision.ReasonCode == "" {
		return nil
	}
	if decision.State == CommandRejected && validCommandReason(decision.ReasonCode) {
		return nil
	}
	return ErrInvalidCommandDecision
}

func validCommandReason(reason string) bool {
	if len(reason) == 0 || len(reason) > 64 || reason[0] < 'A' || reason[0] > 'Z' {
		return false
	}
	for _, c := range reason {
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

func validateStoredCommand(record CommandResult) error {
	if record.CreatedAt.IsZero() || record.CompletedAt.IsZero() || strings.HasPrefix(record.ReasonCode, commandClaimPrefix) {
		return ErrCommandInvariant
	}
	switch record.State {
	case CommandPersisted:
		if record.MessageID == record.CommandID && record.ConversationID != "" && record.ReasonCode == "" {
			return nil
		}
	case CommandRejected:
		if record.MessageID == "" && record.ConversationID == "" && validCommandReason(record.ReasonCode) {
			return nil
		}
	}
	return ErrCommandInvariant
}
