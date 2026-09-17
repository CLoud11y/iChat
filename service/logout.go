package service

import (
	"encoding/json"
	"iChat/models"
	"iChat/utils"

	"github.com/gin-gonic/gin"
)

// Offline is local bookkeeping; token revocation must succeed before invoking it
// or sending a disconnect frame to the active WebSocket.
func LogoutUser(tokens *utils.TokenService, offline func(uint)) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, err := utils.ExtractToken(c)
		if err != nil {
			utils.RespondAuthError(c, err)
			return
		}
		identity, err := tokens.RevokeToken(c.Request.Context(), token)
		if err != nil {
			utils.RespondAuthError(c, err)
			return
		}
		b, _ := json.Marshal(models.CtrlMsg{Type: "offline"})
		disconnectWebsocket(identity.UserID, b)
		if offline != nil {
			offline(identity.UserID)
		}
		utils.RespOK(c.Writer, nil, "ok")
	}
}
