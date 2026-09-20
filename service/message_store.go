package service

import (
	"context"
	"iChat/models"
)

type MessageStore interface {
	// nil confirms the legacy enqueue only, not database persistence or delivery.
	// ErrMessageEnqueueUnknown must not be interpreted as a definite rejection.
	PublishAndSave(context.Context, *models.Message) error
	LoadMsgs(context.Context, uint, uint, uint, models.Message, int) ([]string, error)
}
