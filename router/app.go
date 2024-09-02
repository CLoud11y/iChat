package router

import (
	"iChat/middlewares"
	"iChat/service"

	"github.com/gin-gonic/gin"
)

func Router() *gin.Engine {
	r := gin.Default()

	//静态资源
	r.Static("/asset", "asset/")
	r.StaticFile("/favicon.ico", "asset/images/favicon.ico")
	r.LoadHTMLGlob("views/**/*")

	public := r.Group("/")
	{
		public.GET("/index", service.Index)
		public.POST("/user/register", service.RegisterUser)
		public.POST("/user/login", service.LoginUser)
		public.GET("/toRegister", service.ToRegister)
	}
	protected := r.Group("/auth")
	// 在路由组中使用中间件校验token
	protected.Use(middlewares.JwtAuth)
	{
		protected.GET("/toChat", service.ToChat)
		protected.GET("/test", service.Test)
		protected.GET("/chat", service.Chat)
		protected.POST("/loadMsgs", service.LoadMsgs)
		contact := protected.Group("/contact")
		{
			contact.POST("/addFriend", service.AddFriend)
			contact.POST("/searchFriends", service.SearchFriends)
			contact.POST("/createGroup", service.CreateGroup)
			contact.POST("/deleteGroup", service.DeleteGroup)
			contact.POST("/joinGroup", service.JoinGroup)
			contact.POST("/loadGroups", service.LoadGroups)
		}
	}
	return r
}
