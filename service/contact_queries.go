package service

import (
	"context"
	"iChat/models"
)

type FriendQueries interface {
	SearchFriends2(context.Context, uint) ([]models.User, error)
}

type GroupQueries interface {
	GetGroupsByUid2(context.Context, uint) ([]models.Group, error)
	GetGroupUsers(context.Context, uint) ([]models.User, error)
}

type ContactQueries struct {
	Friends FriendQueries
	Groups  GroupQueries
}
