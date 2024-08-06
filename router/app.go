package router

import (
	"iChat/docs"
	"iChat/service"

	"github.com/gin-gonic/gin"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
)

func Router() *gin.Engine {
	r := gin.Default()
	registerSwagger(r)
	r.GET("/api/index", service.Index)
	r.POST("/api/user/register", service.RegisterUser)
	return r
}

func registerSwagger(r *gin.Engine) {
	docs.SwaggerInfo.BasePath = "/"
	docs.SwaggerInfo.Title = "api manager"
	docs.SwaggerInfo.Version = "1.0"
	docs.SwaggerInfo.Host = "localhost:8080"
	r.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))
}
