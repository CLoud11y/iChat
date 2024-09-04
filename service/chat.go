package service

import (
	"context"
	"encoding/json"
	"fmt"
	"iChat/database"
	"iChat/models"
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
	fmt.Println("sender: ", senderId)
	ctx, canceller := context.WithCancel(context.Background())
	go recvProc(ctx, canceller, senderId, ws)
	go sendProc(ctx, canceller, senderId, ws)
	<-ctx.Done()
}

// 接收goroutine如果挂了 发送goroutine也挂掉
func recvProc(ctx context.Context, cancel context.CancelFunc, senderId uint, ws *websocket.Conn) {
	defer cancel()
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
			fmt.Println("reader channel for userId: ", senderId, "closed")
			return
		case msg = <-subChan:
			fmt.Println("receive msg")
			// TODO: 将msg解绑至message结构体 获取信息后再展示
			err = ws.WriteMessage(1, []byte(msg.Payload))
			if err != nil {
				panic(err)
			}
		case msg = <-groupChan:
			fmt.Println("receive group msg")
			err = ws.WriteMessage(1, []byte(msg.Payload))
			if err != nil {
				panic(err)
			}
		}
	}
}

// 发送goroutine挂了 接收goroutine也挂掉
func sendProc(ctx context.Context, cancel context.CancelFunc, senderId uint, ws *websocket.Conn) {
	defer cancel()
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
				// ws.WriteMessage() // TODO告诉客户端错误
				return
			}
			fmt.Println("p:", string(p))
			msg := &models.Message{}
			err = json.Unmarshal(p, msg)
			if msg.SenderId != senderId {
				fmt.Println("msg.SenderId与token携带id不匹配")
				// ws.WriteMessage() // TODO告诉客户端错误
				continue
			}
			if err != nil {
				fmt.Println("json unmarshal msg failed: ", err)
				// ws.WriteMessage() // TODO告诉客户端错误
				continue
			}
			// 处理待发送消息
			err = handleSendMsg(msg)
			if err != nil {
				fmt.Println("publishAndSave msg failed: ", err)
				// ws.WriteMessage() // TODO告诉客户端错误
				continue
			}
		}
	}
}

// 根据消息类型对消息进行处理
func handleSendMsg(msg *models.Message) error {
	switch msg.Type {
	case models.InvalidType:
		fmt.Println("InvalidType")
	case models.HeartBeatType:
		fmt.Println("HeartBeatmsg: ", msg)
	case models.PrivateType:
		err := database.Mmanager.PublishAndSave(msg)
		if err != nil {
			return err
		}
	case models.GroupType:
		// TODO: 群消息的存储与加载
		err := database.Mmanager.PublishAndSave(msg)
		if err != nil {
			return err
		}
	default:
		fmt.Println("not support msg type: ", msg.Type)
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
