package service

import (
	"context"
	"fmt"
	"iChat/utils"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/gorilla/websocket"
)

func SendMsg(c *gin.Context) {
	// 获取WebSocket连接
	wsUpgrader := websocket.Upgrader{
		HandshakeTimeout: time.Second * 10,
		ReadBufferSize:   1024,
		WriteBufferSize:  1024,
		// 解决跨域
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}
	ws, err := wsUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := ws.Close(); err != nil {
			panic(err)
		}
	}()
	senderId := c.Query("sender")
	receiverId := c.Query("receiver")
	fmt.Println("sender: ", senderId)
	fmt.Println("receiver: ", receiverId)
	ctx, canceller := context.WithCancel(context.Background())
	go reader(ctx, canceller, senderId, ws)
	go sender(ctx, canceller, receiverId, ws)
	<-ctx.Done()
}

func reader(ctx context.Context, cancel context.CancelFunc, channel string, ws *websocket.Conn) {
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
			err = ws.WriteMessage(1, []byte(msg.Payload))
			if err != nil {
				panic(err)
			}
		}
	}
}

func sender(ctx context.Context, cancel context.CancelFunc, channel string, ws *websocket.Conn) {
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			fmt.Println("sender channel: ", channel, "closed")
			return
		default:
			messageType, p, err := ws.ReadMessage()
			if err != nil {
				fmt.Println("ws read msg err: ", err)
				return
			}
			fmt.Println("messageType:", messageType)
			fmt.Println("p:", string(p))

			utils.RDS.Publish(ctx, channel, string(p))
		}
	}
}
