package scheduledtasks

import (
	"context"
	"fmt"
	"iChat/database"
)

type MessageStore interface {
	GetAllDirtyMsgs(context.Context) ([]string, int, error)
	SaveMsgs2DB(context.Context, []string) error
	RemDirtyMsgs(context.Context, int) error
}

// Database failure must never advance the queue. Acknowledgement failure is
// still unsafe to retry until P1 adds idempotent persistence and precise ACKs.
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
	if size < 0 || size != len(msgs) {
		return fmt.Errorf("dirty message batch size mismatch: count=%d length=%d", size, len(msgs))
	}
	if size == 0 {
		return nil
	}
	if err := store.SaveMsgs2DB(ctx, msgs); err != nil {
		return fmt.Errorf("persist dirty messages: %w", err)
	}
	if err := store.RemDirtyMsgs(ctx, size); err != nil {
		return fmt.Errorf("acknowledge dirty messages: %w", err)
	}
	return nil
}
