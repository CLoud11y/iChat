package service

import (
	"fmt"
	"iChat/database"
	"iChat/utils"
	"net/http"

	"github.com/gin-gonic/gin"
)

type RegisterInfo struct {
	PhoneNum  string `json:"phone_num"`
	UserName  string `json:"user_name"`
	Password  string `json:"password"`
	Password2 string `json:"password2"`
}

func RegisterUser(c *gin.Context) {
	info := &RegisterInfo{}
	// 将注册信息绑定至结构体
	if err := c.ShouldBind(info); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"message": "bad request",
		})
		return
	}
	fmt.Println(info)
	// 检查密码是否一致
	if info.Password != info.Password2 {
		c.JSON(http.StatusBadRequest, gin.H{
			"message": "two passwords don't match",
		})
		return
	}
	// 注册
	_, err := database.UserManager.Signup(info.PhoneNum, info.UserName, utils.Encrypt(info.Password))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"message": "ok",
	})
}
