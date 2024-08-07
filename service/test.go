package service

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func Test(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"message": "test api success",
		"uid":     c.GetUint("uid"),
	})
}
