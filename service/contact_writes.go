package service

import (
	"context"
	"errors"
	"iChat/database"
	"iChat/utils"

	"github.com/gin-gonic/gin"
)

type FriendWrites interface {
	AddFriendByPhone(context.Context, uint, string) error
}

type GroupWrites interface {
	CreateGroup(context.Context, string, uint, string) error
	JoinGroup(context.Context, uint, uint) error
	DeleteGroup(context.Context, uint, uint) error
}

type ContactWrites struct {
	Friends FriendWrites
	Groups  GroupWrites
}

func contactWriteResult(c *gin.Context, err error) {
	if err == nil {
		utils.RespOK(c.Writer, struct{}{}, "ok")
		return
	}
	// Only known business errors are safe to expose; unknown errors fail closed.
	for _, business := range []error{database.ErrEmptyFriendPhone, database.ErrSelfFriend, database.ErrRelationExists, database.ErrGroupNotOwned, database.ErrGroupNotFound} {
		if errors.Is(err, business) {
			utils.RespFail(c.Writer, business.Error())
			return
		}
	}
	if errors.Is(err, database.ErrUserNotFound) {
		utils.RespFail(c.Writer, "目标用户不存在")
		return
	}
	storageUnavailable(c)
}
