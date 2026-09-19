package service

import (
	"context"
	"iChat/models"
)

type MessageStore interface {
	PublishAndSave(context.Context, *models.Message) error
	LoadMsgs(context.Context, uint, uint, uint, models.Message, int) ([]string, error)
}
