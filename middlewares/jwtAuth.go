package middlewares

import (
	"iChat/utils"
	"net/http"

	"github.com/gin-gonic/gin"
)

func JwtAuth(c *gin.Context) {
	uid, err := utils.TokenValid(c)
	if err != nil {
		c.String(http.StatusUnauthorized, err.Error())
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	c.Set("uid", uid)
	c.Next()
}
