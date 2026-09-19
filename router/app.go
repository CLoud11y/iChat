package router

import (
	"iChat/config"
	"iChat/middlewares"
	"iChat/service"
	"iChat/utils"

	"github.com/gin-gonic/gin"
)

func Router(conf config.Config, tokens *utils.TokenService, users service.AccountUsers, contacts service.ContactQueries, writes service.ContactWrites) *gin.Engine {
	r := gin.New()
	// 日志中间件 跨域中间件
	r.Use(gin.RecoveryWithWriter(utils.Logger().Out))
	if conf.LOG.AccessEnabled {
		r.Use(middlewares.Logger2File())
	}
	r.Use(middlewares.Cors())

	public := r.Group("/")
	{
		public.POST("/getSystemInfo", service.GetSystemInfo)
		public.POST("/user/register", service.RegisterUser(users))
		public.POST("/user/login", service.LoginUser(users, tokens))
		var offline func(uint)
		if users != nil {
			offline = users.Offline
		}
		public.POST("/user/logout", service.LogoutUser(tokens, offline))
	}
	protected := r.Group("/auth")
	// 在此路由组中使用中间件校验token
	protected.Use(middlewares.JwtAuth(tokens))
	{
		protected.GET("/getws", service.Chat)
		protected.POST("/getContacts", service.GetContacts(contacts))
		protected.POST("/files/index", service.GetFileList)
		protected.POST("/sendMessage", service.SendMsg)

		protected.GET("/test", service.Test)
		protected.GET("/chat", service.Chat)
		protected.POST("/getMessageList", service.GetMessageList)
		contact := protected.Group("/contact")
		{
			contact.POST("/addFriend", service.AddFriend(writes.Friends))
			contact.POST("/searchFriends", service.SearchFriends(contacts.Friends))
			contact.POST("/createGroup", service.CreateGroup(writes.Groups))
			contact.POST("/deleteGroup", service.DeleteGroup(writes.Groups))
			contact.POST("/joinGroup", service.JoinGroup(writes.Groups))
			contact.POST("/loadGroups", service.LoadGroups(contacts.Groups))
			contact.POST("/loadGroupUsers", service.LoadGroupUsers(contacts.Groups))
		}
	}
	return r
}
