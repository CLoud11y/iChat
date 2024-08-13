package service

import (
	"context"
	"encoding/json"
	"fmt"
	"iChat/models"
	"iChat/utils"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/gorilla/websocket"
)

func SendMsg(c *gin.Context) {
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
	senderId := c.Query("sender")
	fmt.Println("sender: ", senderId)
	ctx, canceller := context.WithCancel(context.Background())
	go recvProc(ctx, canceller, senderId, ws)
	go sendProc(ctx, canceller, senderId, ws)
	<-ctx.Done()
}

func recvProc(ctx context.Context, cancel context.CancelFunc, channel string, ws *websocket.Conn) {
	defer cancel()
	sub := utils.RDS.Subscribe(ctx, channel)
	subChan := sub.Channel()
	var msg *redis.Message
	var err error
	for {
		select {
		case <-ctx.Done():
			fmt.Println("reader channel: ", channel, "closed")
			return
		case msg = <-subChan:
			fmt.Println("receive msg")
			// TODO: 将msg解绑至message结构体 获取信息后再展示
			err = ws.WriteMessage(1, []byte(msg.Payload))
			if err != nil {
				panic(err)
			}
		}
	}
}

func sendProc(ctx context.Context, cancel context.CancelFunc, senderId string, ws *websocket.Conn) {
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			fmt.Println("sender channel closed")
			return
		default:
			messageType, p, err := ws.ReadMessage()
			if err != nil {
				fmt.Println("ws read msg err: ", err)
				return
			}
			fmt.Println("messageType:", messageType)
			fmt.Println("p:", string(p))
			msg := &models.Message{}
			err = json.Unmarshal(p, msg)
			// 校验消息中的发送者字段是否确实是此发送者
			if err != nil || msg.SenderId != senderId {
				fmt.Println("json unmarshal msg failed")
				return
			}
			utils.RDS.Publish(ctx, msg.ReceiverId, msg.Content)
		}
	}
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
