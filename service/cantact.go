package service

import (
	"fmt"
	"iChat/database"
	"iChat/utils"

	"github.com/gin-gonic/gin"
)

type FriendInfo struct {
	Phone string `json:"targetPhone"`
}

func AddFriend(c *gin.Context) {
	userId := c.GetUint("uid")
	friend := FriendInfo{}
	if err := c.ShouldBind(&friend); err != nil {
		fmt.Println("friend info binding err", err)
	}
	err := database.Rmanager.AddFriendByPhone(userId, friend.Phone)
	if err != nil {
		utils.RespFail(c.Writer, err.Error())
		return
	}
	utils.RespOK(c.Writer, struct{}{}, "ok")
}

func SearchFriends(c *gin.Context) {
	userId := c.GetUint("uid")
	users, err := database.Rmanager.SearchFriends(userId)
	if err != nil {
		fmt.Println(1)
		utils.RespFail(c.Writer, err.Error())
		return
	}
	utils.RespOKList(c.Writer, users, len(users))
}
