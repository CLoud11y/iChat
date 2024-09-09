package service

import (
	"fmt"
	"iChat/database"
	"iChat/utils"
	"strconv"

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
	users, err := database.Rmanager.SearchFriends2(userId)
	if err != nil {
		fmt.Println(1)
		utils.RespFail(c.Writer, err.Error())
		return
	}
	utils.RespOKList(c.Writer, users, len(users))
}

type CreateGroupInfo struct {
	Name string `json:"name"`
	Desc string `json:"desc"`
}

func CreateGroup(c *gin.Context) {
	userId := c.GetUint("uid")
	group := CreateGroupInfo{}
	if err := c.ShouldBind(&group); err != nil {
		fmt.Println("group info binding err", err)
	}
	err := database.Gmanager.CreateGroup(group.Name, userId, group.Desc)
	if err != nil {
		utils.RespFail(c.Writer, err.Error())
		return
	}
	utils.RespOK(c.Writer, struct{}{}, "ok")
}

type GroupIdInfo struct {
	GroupId string `json:"groupId"`
}

func DeleteGroup(c *gin.Context) {
	userId := c.GetUint("uid")
	group := GroupIdInfo{}
	if err := c.ShouldBind(&group); err != nil {
		fmt.Println("group info binding err", err)
	}
	groupId, _ := strconv.Atoi(group.GroupId)
	err := database.Gmanager.DeleteGroup(userId, uint(groupId))
	if err != nil {
		utils.RespFail(c.Writer, err.Error())
		return
	}
	utils.RespOK(c.Writer, struct{}{}, "ok")
}

func JoinGroup(c *gin.Context) {
	userId := c.GetUint("uid")
	group := GroupIdInfo{}
	if err := c.ShouldBind(&group); err != nil {
		fmt.Println("group info binding err", err)
		utils.RespFail(c.Writer, err.Error())
		return
	}
	groupId, _ := strconv.Atoi(group.GroupId)
	err := database.Gmanager.JoinGroup(userId, uint(groupId))
	if err != nil {
		utils.RespFail(c.Writer, err.Error())
		return
	}
	utils.RespOK(c.Writer, struct{}{}, "ok")
}

func LoadGroups(c *gin.Context) {
	userId := c.GetUint("uid")
	groups, err := database.Gmanager.GetGroupsByUid2(userId)
	if err != nil {
		utils.RespFail(c.Writer, err.Error())
		return
	}
	utils.RespOKList(c.Writer, groups, len(groups))
}
