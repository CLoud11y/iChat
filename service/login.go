package service

import (
	"iChat/database"
	"iChat/utils"
	"net/http"

	"github.com/gin-gonic/gin"
)

type LoginInfo struct {
	PhoneNum string `json:"phone_num"`
	Password string `json:"password"`
}

func LoginUser(c *gin.Context) {
	info := &RegisterInfo{}
	// 将注册信息绑定至结构体
	if err := c.ShouldBind(info); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"message": "bad request",
		})
		return
	}
	//
	user, err := database.UserManager.GetUserByPhone(info.PhoneNum)
	// 用户不存在或发生其他错误
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"message": err.Error(),
		})
		return
	}
	// 生成token
	token, err := utils.GenerateToken(user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"message": "ok",
		"token":   token,
	})
}
