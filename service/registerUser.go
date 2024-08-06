package service

import (
	"iChat/database"
	"iChat/utils"
	"net/http"

	"github.com/gin-gonic/gin"
)

type RegisterInfo struct {
	PhoneNum  string
	UserName  string
	Password  string
	Password2 string
}

// @BasePath /api/user
// @Produce json
// @Param object body RegisterInfo true "register information"
// @Success 200 {string} string "ok"
// @Failure 400 {object} string "bad request"
// @Failure 500 {object} string "内部错误"
// @Router /api/user/register [post]
func RegisterUser(c *gin.Context) {
	info := &RegisterInfo{}
	// 将注册信息绑定至结构体
	if err := c.ShouldBind(info); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"message": "bad request",
		})
		return
	}
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
