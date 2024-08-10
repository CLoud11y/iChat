package router

import (
	"iChat/middlewares"
	"iChat/service"

	"github.com/gin-gonic/gin"
)

func Router() *gin.Engine {
	r := gin.Default()
	public := r.Group("/api")
	{
		public.GET("/index", service.Index)
		public.POST("/user/register", service.RegisterUser)
		public.POST("/user/login", service.LoginUser)
	}
	protected := r.Group("/api/auth")
	{
		// 在路由组中使用中间件校验token
		protected.Use(middlewares.JwtAuth)
		protected.GET("/test", service.Test)
	}
	r.GET("/send_msg", service.SendMsg)
	return r
}
