package service

import (
	"fmt"
	"iChat/database"
	"iChat/utils"

	"github.com/gin-gonic/gin"
)

type LoadMsgsInfo struct {
	UidA  uint `json:"uidA"`
	UidB  uint `json:"uidB"`
	Start int  `json:"start"`
	End   int  `json:"end"`
}

func LoadMsgs(c *gin.Context) {
	userId := c.GetUint("uid")
	info := LoadMsgsInfo{}
	if err := c.ShouldBind(&info); err != nil {
		fmt.Println("info binding err", err)
	}
	if info.UidA != userId && info.UidB != userId {
		utils.RespFail(c.Writer, "token uid与请求信息不符")
		return
	}
	strMsgs, err := database.Mmanager.LoadMsgs(info.UidA, info.UidB, info.Start, info.End)
	if err != nil {
		utils.RespFail(c.Writer, err.Error())
		return
	}
	utils.RespOKList(c.Writer, strMsgs, len(strMsgs))
}
