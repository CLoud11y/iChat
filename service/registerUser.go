package service

import (
	"errors"
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

func RegisterUser(users AccountUsers) gin.HandlerFunc {
	return func(c *gin.Context) {
		info := &RegisterInfo{}
		// 将注册信息绑定至结构体
		if err := c.ShouldBind(info); err != nil {
			utils.RespFail(c.Writer, "invalid register request")
			return
		}
		// 检查密码是否一致
		if info.Password != info.Password2 {
			utils.RespFail(c.Writer, "two passwords don't match")
			return
		}
		// 注册
		if users == nil {
			accountStorageUnavailable(c)
			return
		}
		user, err := users.Signup(c.Request.Context(), info.PhoneNum, info.UserName, utils.Encrypt(info.Password))
		if errors.Is(err, database.ErrPhoneRegistered) {
			utils.RespFail(c.Writer, "phone is already registered")
			return
		}
		if err != nil || user == nil || user.ID == 0 {
			accountStorageUnavailable(c)
			return
		}
		utils.RespOK(c.Writer, struct{}{}, "ok")
	}
}
