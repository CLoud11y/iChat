package middlewares

import (
	"fmt"
	"iChat/utils"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

func JwtAuth(c *gin.Context) {
	claims, err := utils.TokenValid(c)
	if err != nil {
		c.String(http.StatusUnauthorized, err.Error())
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	// 将user_id转换为浮点数字符串，然后再转换为 uint32
	uid, err := strconv.ParseUint(fmt.Sprintf("%.0f", claims["user_id"]), 10, 32)
	if err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	c.Set("uid", uint(uid))
	c.Next()
}
