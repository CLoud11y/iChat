package service

import (
	"errors"
	"iChat/database"
	"iChat/models"
	"iChat/utils"
	"net/http"

	"github.com/gin-gonic/gin"
)

func SendMsg(store MessageStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Only JwtAuth's typed identity may authorize a send. Check it before
		// binding so a missing identity is never treated as a payload error.
		uid := c.GetUint("uid")
		if uid == 0 {
			utils.RespondAuthError(c, utils.ErrUnauthenticated)
			return
		}
		info := &models.MsgInfo{}
		if err := c.ShouldBind(info); err != nil {
			utils.RespFail(c.Writer, "invalid send message request")
			return
		}
		if info.ToContactId == 0 || (info.IsGroup != 0 && info.IsGroup != 1) || (info.Type != "text" && info.Type != "picture" && info.Type != "audio") {
			utils.RespFail(c.Writer, "invalid send message request")
			return
		}
		if store == nil {
			storageUnavailable(c)
			return
		}
		msg := info.Conv2Msg()
		// fromUser remains a legacy wire field, never an authority for sending.
		msg.SenderId = uid
		if err := store.PublishAndSave(c.Request.Context(), msg); err != nil {
			if errors.Is(err, database.ErrMessageEnqueueUnknown) {
				c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
					"code": -1, "msg": "message enqueue outcome unknown",
					"data": gin.H{"reasonCode": "MESSAGE_ENQUEUE_UNKNOWN", "status": "enqueue_unknown", "messageId": msg.Id},
				})
			} else if errors.Is(err, database.ErrInvalidMessage) {
				utils.RespFail(c.Writer, "invalid send message request")
			} else {
				storageUnavailable(c)
			}
			return
		}
		utils.RespOK(c.Writer, "ok", "ok")
	}
}
