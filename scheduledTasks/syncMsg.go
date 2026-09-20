package scheduledtasks

import (
	"context"
	"errors"
	"fmt"
	"iChat/database"
)

type MessageStore interface {
	GetAllDirtyMsgs(context.Context) ([]string, int, error)
	// Returns the durably persisted/isolated consecutive prefix, even on error.
	ProcessDirtyMsgs(context.Context, []string) (int, error)
	RemDirtyMsgs(context.Context, []string) error
}

// Only a durably persisted or isolated prefix may advance the queue. Both
// message and quarantine writes are idempotent across acknowledgement failures.
func SyncMsg(ctx context.Context, store MessageStore) error {
	if store == nil {
		return database.ErrStorageUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	msgs, size, err := store.GetAllDirtyMsgs(ctx)
	if err != nil {
		return fmt.Errorf("read dirty messages: %w", err)
	}
	if size < 0 || size > database.DirtyMessageBatchSize || size != len(msgs) {
		return fmt.Errorf("dirty message batch size mismatch: count=%d length=%d", size, len(msgs))
	}
	if size == 0 {
		return nil
	}
	completed, processErr := store.ProcessDirtyMsgs(ctx, msgs)
	if completed < 0 || completed > len(msgs) || (processErr == nil && completed != len(msgs)) {
		return errors.Join(fmt.Errorf("invalid processed prefix: count=%d length=%d", completed, len(msgs)), processErr)
	}
	if processErr != nil {
		processErr = fmt.Errorf("process dirty messages: %w", processErr)
	}
	if completed > 0 {
		if err := store.RemDirtyMsgs(ctx, msgs[:completed]); err != nil {
			return errors.Join(processErr, fmt.Errorf("acknowledge dirty messages: %w", err))
		}
	}
	return processErr
}
