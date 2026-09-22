package scheduledtasks

import (
	"context"
	"fmt"
)

// Ownership is checked before reading and before each durable transaction.
// Redis ownership cannot fence an in-flight MySQL transaction: recovery must
// first stop the old process, including when Redis has lost its lock data.
type OwnedMessages struct {
	MessageStore
	Owner interface {
		Check(context.Context) error
		Acknowledge(context.Context, []string) error
	}
}

func (s OwnedMessages) GetAllDirtyMsgs(ctx context.Context) ([]string, int, error) {
	if err := s.Owner.Check(ctx); err != nil {
		return nil, 0, err
	}
	return s.MessageStore.GetAllDirtyMsgs(ctx)
}

func (s OwnedMessages) ProcessDirtyMsgs(ctx context.Context, msgs []string) (int, error) {
	for i, raw := range msgs {
		if err := s.Owner.Check(ctx); err != nil {
			return i, err
		}
		n, err := s.MessageStore.ProcessDirtyMsgs(ctx, []string{raw})
		if n < 0 || n > 1 || (err == nil && n != 1) {
			return i, fmt.Errorf("invalid single-record completion: %d", n)
		}
		if err != nil {
			return i + n, err
		}
	}
	return len(msgs), nil
}

func (s OwnedMessages) RemDirtyMsgs(ctx context.Context, msgs []string) error {
	return s.Owner.Acknowledge(ctx, msgs)
}
