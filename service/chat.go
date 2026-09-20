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

func Chat(subscriptions MessageSubscriptions, offline func(uint), logger *logrus.Logger) gin.HandlerFunc {
	if logger == nil {
		logger = logrus.StandardLogger()
	}
	return func(c *gin.Context) {
		senderId := c.GetUint("uid")
		if senderId == 0 {
			utils.RespondAuthError(c, utils.ErrUnauthenticated)
			return
		}
		if subscriptions == nil || offline == nil {
			storageUnavailable(c)
			return
		}
		ctx, cancel := context.WithCancel(c.Request.Context())
		defer cancel()
		owned, err := openSubscriptions(ctx, subscriptions, senderId)
		if err != nil {
			storageUnavailable(c)
			return
		}
		defer owned.Close()
		if ctx.Err() != nil {
			storageUnavailable(c)
			return
		}
		ws, err := getWebsocket(c)
		if err != nil {
			logger.WithError(err).WithField("user_id", senderId).Warn("websocket upgrade failed")
			return
		}
		session := &websocketSession{outbound: make(chan websocketOutbound, websocketSendQueueSize), cancel: cancel}
		registerWebsocketSession(senderId, session)
		var workers sync.WaitGroup
		run := func(fn func()) { workers.Add(1); go func() { defer workers.Done(); fn() }() }
		defer func() {
			cancel()
			_ = ws.Close() // Unblock ReadMessage before waiting for the reader.
			owned.Close()
			workers.Wait()
			finishWebsocketSession(senderId, session, offline)
		}()
		run(func() { websocketWriteProc(ctx, cancel, senderId, ws, session.outbound, logger) })
		run(func() {
			receiveMessages(ctx, cancel, senderId, owned.private.Channel(), owned.groups.Channel(), session.outbound, logger)
		})
		run(func() { sendProc(ctx, cancel, senderId, ws, logger) })
		run(func() { heatbeatProc(ctx, cancel, session.outbound) })
		<-ctx.Done()
	}
}

// Serialize the identity check and local Offline with new registrations so an
// older connection cannot clear the online state of a replacement connection.
func finishWebsocketSession(uid uint, session *websocketSession, offline func(uint)) {
	websocketSessions.Lock()
	defer websocketSessions.Unlock()
	if websocketSessions.items[uid] == session {
		delete(websocketSessions.items, uid)
		offline(uid)
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
	logger *logrus.Logger,
) {
	defer func() {
		cancel()
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case message, ok := <-outbound:
			if !ok {
				return
			}
			if err := ws.SetWriteDeadline(time.Now().Add(websocketWriteTimeout)); err != nil {
				logger.WithError(err).WithField("user_id", userID).Warn("set websocket write deadline failed")
				return
			}
			if err := ws.WriteMessage(message.messageType, message.data); err != nil {
				logger.WithError(err).WithField("user_id", userID).Warn("write websocket message failed")
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

type sessionSubscriptions struct {
	private, groups database.Subscription
	once            sync.Once
}

func (s *sessionSubscriptions) Close() {
	s.once.Do(func() {
		if s.groups != nil {
			_ = s.groups.Close()
		}
		if s.private != nil {
			_ = s.private.Close()
		}
	})
}

func openSubscriptions(ctx context.Context, provider MessageSubscriptions, uid uint) (_ *sessionSubscriptions, err error) {
	if provider == nil {
		return nil, database.ErrStorageUnavailable
	}
	owned := &sessionSubscriptions{}
	defer func() {
		if err != nil {
			owned.Close()
		}
	}()
	owned.private, err = provider.Subscribe(ctx, uid)
	if err != nil {
		return nil, err
	}
	if owned.private == nil || owned.private.Channel() == nil {
		return nil, database.ErrStorageUnavailable
	}
	owned.groups, err = provider.SubscribeGroups(ctx, uid)
	if err != nil {
		return nil, err
	}
	if owned.groups == nil {
		return nil, database.ErrStorageUnavailable
	}
	return owned, nil
}

func receiveMessages(ctx context.Context, cancel context.CancelFunc, senderId uint, subChan, groupChan <-chan *redis.Message, outbound chan<- websocketOutbound, logger *logrus.Logger) {
	defer cancel()
	var err error
	var msg *redis.Message
	var ok bool
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok = <-subChan:
			if !ok || msg == nil {
				return
			}
			// TODO: 将msg解绑至message结构体 获取信息后再展示
			jsonMsg := &models.Message{}
			err = json.Unmarshal(utils.String2Bytes(msg.Payload), jsonMsg)
			if err != nil {
				logger.WithError(err).WithField("user_id", senderId).Warn("decode private message failed")
				continue
			}
			logger.WithFields(logrus.Fields{
				"message_id":  jsonMsg.Id,
				"sender_id":   jsonMsg.SenderId,
				"receiver_id": jsonMsg.ReceiverId,
			}).Debug("private message received")
			b, _ := json.Marshal(models.CtrlMsg{Data: jsonMsg.Conv2MsgInfo(), Type: "simple"})
			if !enqueueWebsocketMessage(ctx, outbound, b) {
				return
			}
		case msg, ok = <-groupChan:
			if !ok || msg == nil {
				return
			}
			jsonMsg := &models.Message{}
			err = json.Unmarshal(utils.String2Bytes(msg.Payload), jsonMsg)
			if err != nil {
				logger.WithError(err).WithField("user_id", senderId).Warn("decode group message failed")
				continue
			}
			logger.WithFields(logrus.Fields{
				"message_id": jsonMsg.Id,
				"sender_id":  jsonMsg.SenderId,
				"group_id":   jsonMsg.ReceiverId,
			}).Debug("group message received")
			b, _ := json.Marshal(models.CtrlMsg{Data: jsonMsg.Conv2MsgInfo(), Type: "group"})
			if !enqueueWebsocketMessage(ctx, outbound, b) {
				return
			}
		}
	}
}

// 发送goroutine挂了 接收goroutine也挂掉
func sendProc(ctx context.Context, cancel context.CancelFunc, senderId uint, ws *websocket.Conn, logger *logrus.Logger) {
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
				entry := logger.WithError(err).WithField("user_id", senderId)
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
				logger.WithError(err).WithField("user_id", senderId).Warn("decode websocket control message failed")
				continue
			}
			// 处理待发送消息
			err = handleCtrlMsg(msg, logger)
			if err != nil {
				logger.WithError(err).WithField("user_id", senderId).Warn("handle websocket control message failed")
				continue
			}
		}
	}
}

func handleCtrlMsg(msg *models.CtrlMsg, logger *logrus.Logger) error {
	switch msg.Type {
	case "ping":
	case "pong":
	default:
		logger.WithField("message_type", msg.Type).Warn("unsupported websocket control message")
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
