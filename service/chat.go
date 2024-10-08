package service

import (
	"context"
	"encoding/json"
	"fmt"
	"iChat/database"
	"iChat/models"
	"iChat/utils"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/gorilla/websocket"
)

func Chat(c *gin.Context) {
	// 获取WebSocket连接
	ws, err := getWebsocket(c)
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := ws.Close(); err != nil {
			panic(err)
		}
	}()
	senderId := c.GetUint("uid")
	database.Umanager.UpdateWs(senderId, ws)
	ctx, canceller := context.WithCancel(context.Background())
	go recvProc(ctx, canceller, senderId, ws)
	go sendProc(ctx, canceller, senderId, ws)
	go heatbeatProc(ctx, canceller, ws)
	<-ctx.Done()
	database.Umanager.Offline(senderId)
}

// 心跳检测goroutine
func heatbeatProc(ctx context.Context, cancel context.CancelFunc, ws *websocket.Conn) {
	defer func() {
		cancel()
		fmt.Println("heatbeatProc closed")
	}()
	ticker := time.NewTicker(time.Second * 30)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b, _ := json.Marshal(models.CtrlMsg{Type: "ping", Data: nil})
			err := ws.WriteMessage(websocket.TextMessage, b)
			if err != nil {
				return
			}
		}
	}
}

// 接收goroutine如果挂了 发送goroutine也挂掉
func recvProc(ctx context.Context, cancel context.CancelFunc, senderId uint, ws *websocket.Conn) {
	defer func() {
		cancel()
		fmt.Println("recvProc closed")
	}()
	subChan, err := database.Mmanager.Subscribe(senderId)
	if err != nil {
		fmt.Println("Mmanager.Subscribe failed: ", err)
		// ws.WriteMessage() // TODO告诉客户端错误
		return
	}
	groupChan, err := database.Mmanager.SubscribeGroups(senderId)
	if err != nil {
		fmt.Println("Mmanager.SubscribeGroups failed: ", err)
	}
	var msg *redis.Message
	for {
		select {
		case <-ctx.Done():
			return
		case msg = <-subChan:
			fmt.Println("receive msg: ", msg.Payload)
			// TODO: 将msg解绑至message结构体 获取信息后再展示
			jsonMsg := &models.Message{}
			err = json.Unmarshal(utils.String2Bytes(msg.Payload), jsonMsg)
			if err != nil {
				fmt.Println("json unmarshal msg failed: ", err)
				continue
			}
			b, _ := json.Marshal(models.CtrlMsg{Data: jsonMsg.Conv2MsgInfo(), Type: "simple"})
			err = ws.WriteMessage(websocket.TextMessage, b)
			if err != nil {
				panic(err)
			}
		case msg = <-groupChan:
			fmt.Println("receive group msg", msg.Payload)
			err = ws.WriteMessage(websocket.TextMessage, utils.String2Bytes(msg.Payload))
			if err != nil {
				panic(err)
			}
		}
	}
}

// 发送goroutine挂了 接收goroutine也挂掉
func sendProc(ctx context.Context, cancel context.CancelFunc, senderId uint, ws *websocket.Conn) {
	defer func() {
		cancel()
		fmt.Println("sendProc closed")
	}()
	for {
		select {
		case <-ctx.Done():
			fmt.Println("sender channel closed")
			return
		default:
			_, p, err := ws.ReadMessage()
			// websocket 发生错误 结束此sendProc
			if err != nil {
				fmt.Println("ws read msg err: ", err)
				utils.Logger().Error("ws read msg err: ", err)
				return
			}
			fmt.Println("p:", string(p))
			msg := &models.CtrlMsg{}
			err = json.Unmarshal(p, msg)
			if err != nil {
				fmt.Println("json unmarshal msg failed: ", err)
				utils.Logger().Error("json unmarshal msg failed: ", err)
				continue
			}
			// 处理待发送消息
			err = handleCtrlMsg(msg)
			if err != nil {
				fmt.Println("handleCtrlMsg failed: ", err)
				utils.Logger().Error("handleCtrlMsg failed: ", err)
				continue
			}
		}
	}
}

func handleCtrlMsg(msg *models.CtrlMsg) error {
	switch msg.Type {
	case "ping":
		fmt.Println("rcv ping msg: ", msg)
	case "pong":
		fmt.Println("rcv pong msg: ", msg)
	default:
		fmt.Println("not support ctrlmsg type: ", msg.Type)
	}
	return nil
}

func getWebsocket(c *gin.Context) (*websocket.Conn, error) {
	wsUpgrader := websocket.Upgrader{
		HandshakeTimeout: time.Second * 10,
		ReadBufferSize:   1024,
		WriteBufferSize:  1024,
		// 解决跨域
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}
	return wsUpgrader.Upgrade(c.Writer, c.Request, nil)
}

func SendMsg(c *gin.Context) {
	msgInfo := &models.MsgInfo{}
	if err := c.ShouldBind(msgInfo); err != nil {
		fmt.Println("msg info binding err", err)
	}
	msg := msgInfo.Conv2Msg()
	fmt.Println("msgInfo: ", msgInfo)
	fmt.Println("msg: ", msg)
	err := database.Mmanager.PublishAndSave(msg)
	if err != nil {
		utils.RespFail(c.Writer, "publishAndSave msg failed: "+err.Error())
		return
	}
	utils.RespOK(c.Writer, "ok", "ok")
}
