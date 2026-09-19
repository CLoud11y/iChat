package service

import (
	"context"
	"encoding/json"
	"iChat/database"
	"iChat/models"
	"iChat/utils"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
)

const (
	websocketSendQueueSize = 256
	websocketWriteTimeout  = 10 * time.Second
)

type websocketOutbound struct {
	messageType int
	data        []byte
	closeAfter  bool
}

type websocketSession struct {
	outbound chan websocketOutbound
	cancel   context.CancelFunc
}

var websocketSessions = struct {
	sync.RWMutex
	items map[uint]*websocketSession
}{items: make(map[uint]*websocketSession)}

func Chat(c *gin.Context) {
	senderId := c.GetUint("uid")
	ws, err := getWebsocket(c)
	if err != nil {
		utils.Logger().WithError(err).WithField("user_id", senderId).Warn("websocket upgrade failed")
		return
	}
	defer ws.Close()

	ctx, canceller := context.WithCancel(context.Background())
	defer canceller()

	session := &websocketSession{
		outbound: make(chan websocketOutbound, websocketSendQueueSize),
		cancel:   canceller,
	}
	registerWebsocketSession(senderId, session)

	utils.Logger().WithField("user_id", senderId).Info("websocket connected")
	defer utils.Logger().WithField("user_id", senderId).Info("websocket disconnected")

	go websocketWriteProc(ctx, canceller, senderId, ws, session.outbound)
	go func() {
		if err := recvProc(ctx, canceller, database.Mmanager, senderId, session.outbound); err != nil {
			utils.Logger().WithError(err).WithField("user_id", senderId).Error("receive websocket messages failed")
		}
	}()
	go sendProc(ctx, canceller, senderId, ws)
	go heatbeatProc(ctx, canceller, session.outbound)
	<-ctx.Done()
	if unregisterWebsocketSession(senderId, session) {
		database.Umanager.Offline(senderId)
	}
}

func registerWebsocketSession(uid uint, session *websocketSession) {
	websocketSessions.Lock()
	oldSession := websocketSessions.items[uid]
	websocketSessions.items[uid] = session
	websocketSessions.Unlock()

	if oldSession != nil {
		oldSession.cancel()
	}
}

func unregisterWebsocketSession(uid uint, session *websocketSession) bool {
	websocketSessions.Lock()
	defer websocketSessions.Unlock()
	if websocketSessions.items[uid] == session {
		delete(websocketSessions.items, uid)
		return true
	}
	return false
}

func disconnectWebsocket(uid uint, data []byte) bool {
	websocketSessions.RLock()
	session := websocketSessions.items[uid]
	websocketSessions.RUnlock()
	if session == nil {
		return false
	}

	select {
	case session.outbound <- websocketOutbound{
		messageType: websocket.TextMessage,
		data:        data,
		closeAfter:  true,
	}:
		return true
	default:
		session.cancel()
		return false
	}
}

func websocketWriteProc(
	ctx context.Context,
	cancel context.CancelFunc,
	userID uint,
	ws *websocket.Conn,
	outbound <-chan websocketOutbound,
) {
	defer func() {
		cancel()
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case message := <-outbound:
			if err := ws.SetWriteDeadline(time.Now().Add(websocketWriteTimeout)); err != nil {
				utils.Logger().WithError(err).WithField("user_id", userID).Warn("set websocket write deadline failed")
				return
			}
			if err := ws.WriteMessage(message.messageType, message.data); err != nil {
				utils.Logger().WithError(err).WithField("user_id", userID).Warn("write websocket message failed")
				return
			}
			if message.closeAfter {
				return
			}
		}
	}
}

func enqueueWebsocketMessage(ctx context.Context, outbound chan<- websocketOutbound, data []byte) bool {
	select {
	case outbound <- websocketOutbound{messageType: websocket.TextMessage, data: data}:
		return true
	case <-ctx.Done():
		return false
	}
}

// 心跳检测goroutine
func heatbeatProc(ctx context.Context, cancel context.CancelFunc, outbound chan<- websocketOutbound) {
	defer func() {
		cancel()
	}()
	ticker := time.NewTicker(time.Second * 30)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b, _ := json.Marshal(models.CtrlMsg{Type: "ping", Data: nil})
			if !enqueueWebsocketMessage(ctx, outbound, b) {
				return
			}
		}
	}
}

// 接收goroutine如果挂了 发送goroutine也挂掉
type MessageSubscriptions interface {
	Subscribe(context.Context, uint) (database.Subscription, error)
	SubscribeGroups(context.Context, uint) (database.Subscription, error)
}

func recvProc(ctx context.Context, cancel context.CancelFunc, subscriptions MessageSubscriptions, senderId uint, outbound chan<- websocketOutbound) error {
	defer func() {
		cancel()
	}()
	if subscriptions == nil {
		return database.ErrStorageUnavailable
	}
	private, err := subscriptions.Subscribe(ctx, senderId)
	if private != nil {
		defer private.Close()
	}
	if err != nil {
		return err
	}
	if private == nil {
		return database.ErrStorageUnavailable
	}
	groups, err := subscriptions.SubscribeGroups(ctx, senderId)
	if groups != nil {
		defer groups.Close()
	}
	if err != nil {
		return err
	}
	if groups == nil {
		return database.ErrStorageUnavailable
	}
	subChan, groupChan := private.Channel(), groups.Channel()
	if subChan == nil {
		return database.ErrStorageUnavailable
	}
	var msg *redis.Message
	var ok bool
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok = <-subChan:
			if !ok || msg == nil {
				return nil
			}
			// TODO: 将msg解绑至message结构体 获取信息后再展示
			jsonMsg := &models.Message{}
			err = json.Unmarshal(utils.String2Bytes(msg.Payload), jsonMsg)
			if err != nil {
				utils.Logger().WithError(err).WithField("user_id", senderId).Warn("decode private message failed")
				continue
			}
			utils.Logger().WithFields(logrus.Fields{
				"message_id":  jsonMsg.Id,
				"sender_id":   jsonMsg.SenderId,
				"receiver_id": jsonMsg.ReceiverId,
			}).Debug("private message received")
			b, _ := json.Marshal(models.CtrlMsg{Data: jsonMsg.Conv2MsgInfo(), Type: "simple"})
			if !enqueueWebsocketMessage(ctx, outbound, b) {
				return nil
			}
		case msg, ok = <-groupChan:
			if !ok || msg == nil {
				return nil
			}
			jsonMsg := &models.Message{}
			err = json.Unmarshal(utils.String2Bytes(msg.Payload), jsonMsg)
			if err != nil {
				utils.Logger().WithError(err).WithField("user_id", senderId).Warn("decode group message failed")
				continue
			}
			utils.Logger().WithFields(logrus.Fields{
				"message_id": jsonMsg.Id,
				"sender_id":  jsonMsg.SenderId,
				"group_id":   jsonMsg.ReceiverId,
			}).Debug("group message received")
			b, _ := json.Marshal(models.CtrlMsg{Data: jsonMsg.Conv2MsgInfo(), Type: "group"})
			if !enqueueWebsocketMessage(ctx, outbound, b) {
				return nil
			}
		}
	}
}

// 发送goroutine挂了 接收goroutine也挂掉
func sendProc(ctx context.Context, cancel context.CancelFunc, senderId uint, ws *websocket.Conn) {
	defer func() {
		cancel()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		default:
			_, p, err := ws.ReadMessage()
			// websocket 发生错误 结束此sendProc
			if err != nil {
				entry := utils.Logger().WithError(err).WithField("user_id", senderId)
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
					entry.Warn("read websocket message failed")
				} else {
					entry.Debug("websocket closed by client")
				}
				return
			}
			msg := &models.CtrlMsg{}
			err = json.Unmarshal(p, msg)
			if err != nil {
				utils.Logger().WithError(err).WithField("user_id", senderId).Warn("decode websocket control message failed")
				continue
			}
			// 处理待发送消息
			err = handleCtrlMsg(msg)
			if err != nil {
				utils.Logger().WithError(err).WithField("user_id", senderId).Warn("handle websocket control message failed")
				continue
			}
		}
	}
}

func handleCtrlMsg(msg *models.CtrlMsg) error {
	switch msg.Type {
	case "ping":
	case "pong":
	default:
		utils.Logger().WithField("message_type", msg.Type).Warn("unsupported websocket control message")
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
