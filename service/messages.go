package service

import (
	"encoding/json"
	"errors"
	"iChat/database"
	"iChat/models"
	"iChat/utils"
	"net/http"

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

func GetMessageList(store MessageStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		userId := c.GetUint("uid")
		if userId == 0 {
			utils.RespondAuthError(c, utils.ErrUnauthenticated)
			return
		}
		info := ReqMsgInfo{}
		if err := c.ShouldBind(&info); err != nil {
			utils.RespFail(c.Writer, "invalid message list request")
			return
		}
		if info.TocontactId == 0 || info.Limit < 1 || info.Limit > database.MaxMessagePageSize || (info.IsGroup != 0 && info.IsGroup != 1) {
			utils.RespFail(c.Writer, "invalid message list request")
			return
		}
		if store == nil {
			storageUnavailable(c)
			return
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
		strMsgs, err := store.LoadMsgs(c.Request.Context(), userId, info.TocontactId, uint(msgType), earliestMsg, info.Limit)
		if err != nil {
			if errors.Is(err, database.ErrMessageHistoryForbidden) {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
					"code": -1, "msg": "group history access denied", "reasonCode": "GROUP_HISTORY_FORBIDDEN",
				})
			} else if errors.Is(err, database.ErrMessageCursorNotFound) {
				utils.RespFail(c.Writer, "message cursor not found")
			} else if errors.Is(err, database.ErrInvalidMessage) {
				utils.RespFail(c.Writer, "invalid message list request")
			} else {
				storageUnavailable(c)
			}
			return
		}
		msgs := make([]RespMsgInfo, len(strMsgs))
		for i, strMsg := range strMsgs {
			msg := models.Message{}
			if err := json.Unmarshal([]byte(strMsg), &msg); err != nil {
				storageUnavailable(c)
				return
			}
			mediaType := msg.Conv2MsgInfo().Type
			if mediaType == "" {
				storageUnavailable(c)
				return
			}
			respMsg := RespMsgInfo{
				Id:          msg.Id,
				Status:      "succeed",
				Type:        mediaType,
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
}
