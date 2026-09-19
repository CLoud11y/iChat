package service

import (
	"errors"
	"iChat/database"
	"iChat/models"
	"iChat/utils"

	"github.com/gin-gonic/gin"
)

type LoginInfo struct {
	PhoneNum string `json:"account"`
	Password string `json:"password"`
}

type LoginResp struct {
	Token     string      `json:"token"`
	SessionId string      `json:"sessionId"`
	UserInfo  models.User `json:"userInfo"`
}

func LoginUser(users AccountUsers, tokens *utils.TokenService) gin.HandlerFunc {
	return func(c *gin.Context) {
		info := &LoginInfo{}
		// 将注册信息绑定至结构体
		if err := c.ShouldBind(info); err != nil {
			utils.RespFail(c.Writer, "invalid login request")
			return
		}
		if users == nil {
			storageUnavailable(c)
			return
		}
		user, err := users.GetUserByPhone(c.Request.Context(), info.PhoneNum)
		if errors.Is(err, database.ErrUserNotFound) {
			utils.RespFail(c.Writer, "account or password is incorrect")
			return
		}
		if err != nil || user == nil || user.ID == 0 {
			storageUnavailable(c)
			return
		}
		if !utils.VerifyPassword(info.Password, user.Password) {
			utils.RespFail(c.Writer, "account or password is incorrect")
			return
		}
		// 生成token
		token, err := tokens.GenerateToken(user.ID)
		if err != nil {
			utils.RespondAuthError(c, err)
			return
		}
		users.Online(user)
		utils.RespOK(c.Writer, LoginResp{token, "0", *user}, "ok")
	}
}
