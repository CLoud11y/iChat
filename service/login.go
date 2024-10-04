package service

import (
	"fmt"
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

func LoginUser(c *gin.Context) {
	info := &LoginInfo{}
	// 将注册信息绑定至结构体
	if err := c.ShouldBind(info); err != nil {
		fmt.Println("login info binding err", err)
	}
	user, err := database.Umanager.GetUserByPhone(info.PhoneNum)
	// 用户不存在或发生其他错误
	if err != nil {
		utils.RespFail(c.Writer, err.Error())
		return
	}
	// 生成token
	token, err := utils.GenerateToken(user.ID)
	if err != nil {
		utils.RespFail(c.Writer, err.Error())
		return
	}
	utils.RespOK(c.Writer, LoginResp{token, "0", *user}, "ok")
}
