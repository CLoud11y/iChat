package router

import (
	"iChat/middlewares"
	"iChat/service"

	"github.com/gin-gonic/gin"
)

func Router() *gin.Engine {
	r := gin.Default()
	r.Use(middlewares.Cors())

	public := r.Group("/")
	{
		public.POST("/getSystemInfo", service.GetSystemInfo)
		public.POST("/user/register", service.RegisterUser)
		public.POST("/user/login", service.LoginUser)
	}
	protected := r.Group("/auth")
	// 在路由组中使用中间件校验token
	protected.Use(middlewares.JwtAuth)
	{
		protected.GET("/getws", service.Chat)
		protected.POST("/getContacts", service.GetContacts)
		protected.POST("/files/index", service.GetFileList)
		protected.POST("/sendMessage", service.SendMsg)

		protected.GET("/test", service.Test)
		protected.GET("/chat", service.Chat)
		protected.POST("/getMessageList", service.GetMessageList)
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
