package service

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func WebSocketHandler(c *gin.Context) {
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

	// 处理WebSocket消息
	for {
		messageType, p, err := ws.ReadMessage()
		if err != nil {
			break
		}

		fmt.Println("messageType:", messageType)
		fmt.Println("p:", string(p))

		// 输出WebSocket消息内容
		err = ws.WriteMessage(messageType, p)
		if err != nil {
			panic(err)
		}
	}
}
