package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iChat/messaging"
	"iChat/models"
	"strconv"
)

const (
	ReasonSenderNotFound = "SENDER_NOT_FOUND"
	ReasonTargetNotFound = "TARGET_NOT_FOUND"
	ReasonSelfSend       = "SELF_SEND_NOT_ALLOWED"
	ReasonNotFriends     = "NOT_FRIENDS"
	ReasonNotGroupMember = "NOT_GROUP_MEMBER"
)

// ExecuteAuthorizedCommand checks permissions only after winning a new command
// identity. A replay/conflict never rechecks current permissions. The handler
// receives the same transaction while permission locks remain held.
// Message/seq/Outbox production writes are supplied in P2.5.
func (r *MessageRepository) ExecuteAuthorizedCommand(ctx context.Context, command messaging.Command, handler CommandHandler) (CommandExecution, error) {
	if handler == nil {
		return CommandExecution{}, ErrInvalidCommandDecision
	}
	return r.ExecuteCommand(ctx, command, func(ctx context.Context, tx CommandTransaction, command messaging.Command) (CommandDecision, error) {
		reason, err := authorizeCommand(ctx, tx, command)
		if err != nil {
			return CommandDecision{}, err
		}
		if reason != "" {
			return CommandDecision{State: CommandRejected, ReasonCode: reason}, nil
		}
		return handler(ctx, tx, command)
	})
}

// Lock order: command -> users by numeric ID -> group (if any) -> relations by
// (owner_id,target_id,type) -> conversation (P2.5). Current locking reads see
// committed changes after waiting, rather than an earlier snapshot.
// https://dev.mysql.com/doc/refman/8.0/en/innodb-locking-reads.html
func authorizeCommand(ctx context.Context, tx CommandTransaction, command messaging.Command) (string, error) {
	sender, _ := strconv.ParseUint(command.SenderID, 10, 64) // ExecuteCommand validated both IDs.
	target, _ := strconv.ParseUint(command.TargetID, 10, 64)
	if command.TargetType == messaging.Private {
		if sender == target {
			return ReasonSelfSend, nil
		}
		low, high := sender, target
		if low > high {
			low, high = high, low
		}
		// Lock even soft-deleted rows by identity, then inspect their state.
		// This also serializes concurrent restore/delete with the decision.
		for _, id := range []uint64{low, high} {
			active, err := lockActiveIdentity(ctx, tx, "SELECT id,deleted_at IS NULL FROM `user` WHERE id=? FOR SHARE", id)
			if err != nil {
				return "", err
			}
			if !active {
				if id == sender {
					return ReasonSenderNotFound, nil
				}
				return ReasonTargetNotFound, nil
			}
		}
		for _, pair := range [][2]uint64{{low, high}, {high, low}} {
			active, err := lockActiveIdentity(ctx, tx,
				"SELECT id,deleted_at IS NULL FROM relation WHERE owner_id=? AND target_id=? AND type=? FOR SHARE",
				pair[0], pair[1], models.FriendRelation)
			if err != nil {
				return "", err
			}
			if !active {
				return ReasonNotFriends, nil
			}
		}
		return "", nil
	}
	active, err := lockActiveIdentity(ctx, tx, "SELECT id,deleted_at IS NULL FROM `user` WHERE id=? FOR SHARE", sender)
	if err != nil {
		return "", err
	}
	if !active {
		return ReasonSenderNotFound, nil
	}
	active, err = lockActiveIdentity(ctx, tx, "SELECT id,deleted_at IS NULL FROM `group` WHERE id=? FOR SHARE", target)
	if err != nil {
		return "", err
	}
	if !active {
		return ReasonTargetNotFound, nil
	}
	active, err = lockActiveIdentity(ctx, tx,
		"SELECT id,deleted_at IS NULL FROM relation WHERE owner_id=? AND target_id=? AND type=? FOR SHARE",
		sender, target, models.GroupRelation)
	if err != nil {
		return "", err
	}
	if !active {
		return ReasonNotGroupMember, nil
	}
	return "", nil
}

func lockActiveIdentity(ctx context.Context, tx CommandTransaction, query string, args ...any) (bool, error) {
	var id uint64
	var active bool
	err := tx.QueryRowContext(ctx, query, args...).Scan(&id, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock message permission: %w", err)
	}
	return active, nil
}
