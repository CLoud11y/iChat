package service

import (
	"iChat/database"
	"iChat/utils"
	"strconv"

	"github.com/gin-gonic/gin"
)

type ContactInfo struct {
	Id           uint   `json:"id"`
	DisplayName  string `json:"displayName"`
	Avatar       string `json:"avatar"`
	Account      string `json:"account"`
	Index        string `json:"index"`
	Unread       uint   `json:"unread"`
	LastContent  string `json:"lastContent"`
	LastSendTime int64  `json:"lastSendTime"`
	IsNotice     int    `json:"is_notice"`
	IsGroup      int    `json:"is_group"`
	Setting      string `json:"setting"`
}

func GetContacts(c *gin.Context) {
	userId := c.GetUint("uid")
	friends, err := database.Rmanager.SearchFriends2(userId)
	if err != nil {
		utils.RespFail(c.Writer, err.Error())
		return
	}
	groups, err := database.Gmanager.GetGroupsByUid2(userId)
	resMsg := "ok"
	if err != nil {
		utils.Logger().WithError(err).WithField("user_id", userId).Error("get groups failed")
		resMsg = "get groups failed"
	}
	contacts := make([]ContactInfo, 0, len(friends)+len(groups))
	for i := 0; i < len(friends); i++ {
		contacts = append(contacts, ContactInfo{
			Id:           friends[i].ID,
			DisplayName:  friends[i].Name,
			Avatar:       "",
			Account:      friends[i].Phone,
			Index:        utils.GetFirstLetter(friends[i].Name),
			Unread:       0,
			LastContent:  "last content unimplemented",
			LastSendTime: 0,
			IsNotice:     0,
			IsGroup:      0,
			Setting:      "",
		})
	}
	for i := 0; i < len(groups); i++ {
		contacts = append(contacts, ContactInfo{
			Id:           groups[i].ID,
			DisplayName:  groups[i].Name,
			Avatar:       "",
			Account:      "",
			Index:        "群聊",
			Unread:       0,
			LastContent:  "",
			LastSendTime: 0,
			IsNotice:     0,
			IsGroup:      1,
			Setting:      "",
		})
	}
	utils.RespOK(c.Writer, contacts, resMsg)
}

type FriendInfo struct {
	Phone string `json:"targetPhone"`
}

func AddFriend(c *gin.Context) {
	userId := c.GetUint("uid")
	friend := FriendInfo{}
	if err := c.ShouldBind(&friend); err != nil {
		utils.Logger().WithError(err).Warn("bind add friend request failed")
		utils.RespFail(c.Writer, "invalid add friend request")
		return
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
		utils.Logger().WithError(err).Warn("bind create group request failed")
		utils.RespFail(c.Writer, "invalid create group request")
		return
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
		utils.Logger().WithError(err).Warn("bind delete group request failed")
		utils.RespFail(c.Writer, "invalid delete group request")
		return
	}
	groupId, err := strconv.Atoi(group.GroupId)
	if err != nil {
		utils.Logger().WithError(err).Warn("parse delete group id failed")
		utils.RespFail(c.Writer, "invalid group id")
		return
	}
	err = database.Gmanager.DeleteGroup(userId, uint(groupId))
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
		utils.Logger().WithError(err).Warn("bind join group request failed")
		utils.RespFail(c.Writer, err.Error())
		return
	}
	groupId, err := strconv.Atoi(group.GroupId)
	if err != nil {
		utils.Logger().WithError(err).Warn("parse join group id failed")
		utils.RespFail(c.Writer, "invalid group id")
		return
	}
	err = database.Gmanager.JoinGroup(userId, uint(groupId))
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

func LoadGroupUsers(c *gin.Context) {
	reqInfo := struct {
		GroupId uint `json:"group_id"`
	}{}
	if err := c.ShouldBind(&reqInfo); err != nil {
		utils.Logger().WithError(err).Warn("bind load group users request failed")
		utils.RespFail(c.Writer, err.Error())
		return
	}
	users, err := database.Gmanager.GetGroupUsers(reqInfo.GroupId)
	if err != nil {
		utils.Logger().WithError(err).WithField("group_id", reqInfo.GroupId).Error("load group users failed")
		utils.RespFail(c.Writer, err.Error())
		return
	}
	type RespItem struct {
		Role     int         `json:"role"`
		UserId   uint        `json:"user_id"`
		UserInfo ContactInfo `json:"userInfo"`
	}
	data := make([]RespItem, 0, len(users))
	for i := 0; i < len(users); i++ {
		data = append(data, RespItem{
			Role:   0, // not implement
			UserId: users[i].ID,
			UserInfo: ContactInfo{
				Id:           users[i].ID,
				DisplayName:  users[i].Name,
				Avatar:       "",
				Account:      users[i].Phone,
				Index:        utils.GetFirstLetter(users[i].Name),
				Unread:       0,
				LastContent:  "",
				LastSendTime: 0,
				IsNotice:     0,
				IsGroup:      0,
				Setting:      "",
			},
		})
	}
	utils.RespOKList(c.Writer, data, len(data))
}
