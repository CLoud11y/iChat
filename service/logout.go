package service

import (
	"encoding/json"
	"fmt"
	"iChat/database"
	"iChat/models"
	"iChat/utils"
	"strconv"

	"github.com/gin-gonic/gin"
)

func LogoutUser(c *gin.Context) {
	token, err := utils.ExtractToken(c)
	if err != nil {
		utils.Logger().Warn("LogoutUser extractToken error: ", err)
	}
	claims, err := utils.TokenValid(c)
	if err != nil {
		utils.Logger().Warn("LogoutUser tokenValid error: ", err)
	}
	err = utils.BanToken(token, claims)
	if err != nil {
		utils.Logger().Error("LogoutUser BanToken error: ", err)
	}

	uid, _ := strconv.ParseUint(fmt.Sprintf("%.0f", claims["user_id"]), 10, 32)
	b, _ := json.Marshal(models.CtrlMsg{Type: "offline"})
	disconnectWebsocket(uint(uid), b)
	database.Umanager.Offline(uint(uid))
}
