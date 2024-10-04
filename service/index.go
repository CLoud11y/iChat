package service

import (
	"iChat/utils"

	"github.com/gin-gonic/gin"
)

func GetSystemInfo(c *gin.Context) {
	chatInfo := map[string]string{"online": "1"}
	sysInfo := map[string]any{"name": "iChat", "state": "1", "regtype": "1"}
	utils.RespOK(c.Writer, map[string]any{"sysInfo": sysInfo, "chatInfo": chatInfo}, "ok")
}
