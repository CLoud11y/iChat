package service

import (
	"encoding/json"
	"fmt"
	"iChat/database"
	"iChat/models"
	"iChat/utils"

	"github.com/gin-gonic/gin"
)

type ReqMsgInfo struct {
	TocontactId uint           `json:"toContactId"`
	EarliestMsg models.MsgInfo `json:"earliestMsg"`
	Limit       int            `json:"limit"`
	IsGroup     int            `json:"is_group"`
}
type RespMsgInfo struct {
	Id          string              `json:"id"`
	Status      string              `json:"status"`
	Type        string              `json:"type"`
	SendTime    int64               `json:"sendTime"`
	Content     string              `json:"content"`
	TocontactId uint                `json:"toContactId"`
	FromUser    models.FromUserInfo `json:"fromUser"`
}

func GetMessageList(c *gin.Context) {
	userId := c.GetUint("uid")
	info := ReqMsgInfo{}
	if err := c.ShouldBind(&info); err != nil {
		fmt.Println("info binding err", err)
	}
	// 消息类型
	msgType := models.PrivateType
	if info.IsGroup == 1 {
		msgType = models.GroupType
	}
	earliestMsg := *info.EarliestMsg.Conv2Msg()
	if info.EarliestMsg.Type == "" {
		earliestMsg.Type = models.InvalidType
	}
	strMsgs, err := database.Mmanager.LoadMsgs(userId, info.TocontactId, uint(msgType), earliestMsg, info.Limit)
	if err != nil {
		utils.RespFail(c.Writer, err.Error())
		return
	}
	msgs := make([]RespMsgInfo, len(strMsgs))
	for i, strMsg := range strMsgs {
		msg := models.Message{}
		json.Unmarshal([]byte(strMsg), &msg)
		respMsg := RespMsgInfo{
			Id:          msg.Id,
			Status:      "succeed",
			Type:        "text",
			SendTime:    msg.TimeStamp,
			Content:     msg.Content,
			TocontactId: msg.ReceiverId,
			FromUser: models.FromUserInfo{
				Id: msg.SenderId,
			},
		}
		// 由于前端显示需要 这里逆序
		msgs[len(strMsgs)-i-1] = respMsg
	}
	utils.RespOKList(c.Writer, msgs, len(strMsgs))
}
