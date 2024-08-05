package router

import (
	"iChat/service"

	"github.com/gin-gonic/gin"
)

func Router() *gin.Engine {
	r := gin.Default()
	r.GET("/index", service.Index)
	return r
}
