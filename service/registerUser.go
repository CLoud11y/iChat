package service

import (
	"fmt"
	"iChat/database"
	"iChat/utils"

	"github.com/gin-gonic/gin"
)

type RegisterInfo struct {
	PhoneNum  string `json:"phone"`
	UserName  string `json:"name"`
	Password  string `json:"password"`
	Password2 string `json:"confirmPsw"`
}

func RegisterUser(c *gin.Context) {
	info := &RegisterInfo{}
	// 将注册信息绑定至结构体
	if err := c.ShouldBind(info); err != nil {
		utils.RespFail(c.Writer, err.Error())
		return
	}
	fmt.Println(info)
	// 检查密码是否一致
	if info.Password != info.Password2 {
		utils.RespFail(c.Writer, "two passwords don't match")
		return
	}
	// 注册
	_, err := database.UserManager.Signup(info.PhoneNum, info.UserName, utils.Encrypt(info.Password))
	if err != nil {
		utils.RespFail(c.Writer, err.Error())
		return
	}
	utils.RespOK(c.Writer, struct{}{}, "ok")
}
