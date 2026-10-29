package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"iChat/database"
	"iChat/messaging"
	"iChat/utils"
)

var ErrReliableChatInput = errors.New("invalid reliable chat dependencies or options")

type NotificationSubscriptions interface {
	Channels(context.Context, uint64, int) ([]string, error)
	Subscribe(context.Context, []string) (database.Subscription, error)
}
type DeliveryAcknowledger interface {
	AcknowledgeDelivery(context.Context, uint64, string, messaging.Sequence) (messaging.Sequence, error)
}
type ReliableChatOptions struct {
	SetupTimeout, ResolveTimeout, WriteTimeout, RefreshInterval, HeartbeatInterval, ReadTimeout time.Duration
	QueueSize, MaxGroups, MaxSessions                                                           int
}

func DefaultReliableChatOptions() ReliableChatOptions {
	return ReliableChatOptions{SetupTimeout: 3 * time.Second, ResolveTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second, RefreshInterval: 10 * time.Second, HeartbeatInterval: 20 * time.Second, ReadTimeout: 60 * time.Second, QueueSize: 64, MaxGroups: 1024, MaxSessions: 1024}
}
func (o ReliableChatOptions) Validate() error {
	for _, d := range []time.Duration{o.SetupTimeout, o.ResolveTimeout, o.WriteTimeout, o.RefreshInterval, o.HeartbeatInterval} {
		if d <= 0 || d > time.Minute {
			return ErrReliableChatInput
		}
	}
	if o.ReadTimeout <= o.HeartbeatInterval || o.ReadTimeout > 2*time.Minute || o.QueueSize < 1 || o.QueueSize > 1024 || o.MaxGroups < 1 || o.MaxGroups > 1024 || o.MaxSessions < 1 || o.MaxSessions > 10000 {
		return ErrReliableChatInput
	}
	return nil
}

type reliableSlot struct {
	gate    chan struct{}
	mu      sync.Mutex
	current *reliableSession
	refs    int
}
type reliableSession struct {
	ctx    context.Context
	cancel context.CancelFunc
	id     string
	user   uint64
	slot   *reliableSlot
	conn   *websocket.Conn
	queue  chan reliableOutbound
}
type reliableOutbound struct {
	channel string
	body    []byte
	control int
}
type ReliableChat struct {
	mu           sync.Mutex
	source       NotificationSubscriptions
	dispatcher   *NotificationDispatcher
	acknowledger DeliveryAcknowledger
	options      ReliableChatOptions
	slots        map[uint64]*reliableSlot
	count        int
}

func NewReliableChat(source NotificationSubscriptions, reader NotificationReader, acknowledger DeliveryAcknowledger, options ReliableChatOptions) (*ReliableChat, error) {
	if source == nil || nilChatValue(source) || acknowledger == nil || nilChatValue(acknowledger) || options.Validate() != nil {
		return nil, ErrReliableChatInput
	}
	d, err := NewNotificationDispatcher(reader)
	if err != nil {
		return nil, err
	}
	return &ReliableChat{source: source, dispatcher: d, acknowledger: acknowledger, options: options, slots: make(map[uint64]*reliableSlot)}, nil
}
func nilChatValue(value any) bool {
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Pointer, reflect.Func, reflect.Interface, reflect.Chan, reflect.Map, reflect.Slice:
		return v.IsNil()
	}
	return false
}
func (*ReliableChat) String() string     { return "reliable chat" }
func (c *ReliableChat) GoString() string { return c.String() }
func (c *ReliableChat) acquire(user uint64) *reliableSlot {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.count >= c.options.MaxSessions {
		return nil
	}
	slot := c.slots[user]
	if slot == nil {
		slot = &reliableSlot{gate: make(chan struct{}, 1)}
		c.slots[user] = slot
	}
	slot.refs++
	c.count++
	return slot
}
func (c *ReliableChat) release(user uint64, slot *reliableSlot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	slot.refs--
	c.count--
	if slot.refs == 0 {
		delete(c.slots, user)
	}
}
func (s *reliableSession) active() bool {
	s.slot.mu.Lock()
	defer s.slot.mu.Unlock()
	return s.slot.current == s && s.ctx.Err() == nil
}
func (s *reliableSession) enqueue(item reliableOutbound) bool {
	if s.ctx.Err() != nil {
		return false
	}
	select {
	case s.queue <- item:
		return true
	default:
		s.cancel()
		return false
	}
}

type reliableSubscription struct {
	database.Subscription
	cancel context.CancelFunc
}

func (s *reliableSubscription) Close() error { s.cancel(); return s.Subscription.Close() }
func (c *ReliableChat) open(lifetime, setup context.Context, channels []string) (owned *reliableSubscription, err error) {
	ctx, cancel := context.WithCancel(lifetime)
	unlink := context.AfterFunc(setup, cancel)
	sub, err := c.source.Subscribe(ctx, channels)
	unlinked := unlink()
	if sub != nil && !nilChatValue(sub) {
		owned = &reliableSubscription{sub, cancel}
	}
	if err == nil {
		err = errors.Join(setup.Err(), ctx.Err())
		if !unlinked && err == nil {
			err = context.Canceled
		}
	}
	if err == nil && (owned == nil || owned.Channel() == nil) {
		err = ErrReliableChatInput
	}
	if err != nil {
		cancel()
		if owned != nil {
			owned.Close()
		}
		return nil, err
	}
	return owned, nil
}

func (c *ReliableChat) validChannels(user uint64, channels []string) bool {
	if len(channels) < 1 || len(channels) > c.options.MaxGroups+1 || channels[0] != "ichat:notify:v1:user:"+strconv.FormatUint(user, 10) {
		return false
	}
	seen := map[string]bool{}
	for _, channel := range channels[1:] {
		id := strings.TrimPrefix(channel, "ichat:notify:v1:group:")
		n, e := strconv.ParseUint(id, 10, 64)
		if id == channel || e != nil || n == 0 || strconv.FormatUint(n, 10) != id || seen[channel] {
			return false
		}
		seen[channel] = true
	}
	return true
}

// Serve keeps its HTTP handler alive until every old/replaced subscription,
// SQL callback and reader/writer actually joins. API request ownership therefore
// retains borrowed clients even after a shutdown observer returns pending.
func (c *ReliableChat) Serve(g *gin.Context, authenticate func(context.Context) (uint64, error)) {
	if c == nil || authenticate == nil {
		g.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	setup, stopSetup := context.WithTimeout(g.Request.Context(), c.options.SetupTimeout)
	defer stopSetup()
	user, err := authenticate(setup)
	if err != nil || user == 0 {
		if err == nil {
			err = utils.ErrUnauthenticated
		}
		utils.RespondAuthError(g, err)
		return
	}
	slot := c.acquire(user)
	if slot == nil {
		g.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	defer c.release(user, slot)
	ctx, cancel := context.WithCancel(g.Request.Context())
	defer cancel()
	channels, err := c.source.Channels(setup, user, c.options.MaxGroups)
	if err != nil || !c.validChannels(user, channels) {
		g.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	sub, err := c.open(ctx, setup, channels)
	if err != nil {
		g.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	defer sub.Close()
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		g.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	conn, err := (&websocket.Upgrader{HandshakeTimeout: c.options.SetupTimeout, Subprotocols: []string{"ichat.v1"}}).Upgrade(g.Writer, g.Request, nil)
	if err != nil {
		return
	}
	s := &reliableSession{ctx: ctx, cancel: cancel, id: hex.EncodeToString(nonce[:]), user: user, slot: slot, conn: conn, queue: make(chan reliableOutbound, c.options.QueueSize)}
	defer conn.Close()
	select {
	case slot.gate <- struct{}{}:
	case <-setup.Done():
		return
	}
	slot.mu.Lock()
	old := slot.current
	slot.current = s
	slot.mu.Unlock()
	if old != nil {
		old.cancel()
		old.conn.Close()
	}
	<-slot.gate
	defer func() {
		slot.mu.Lock()
		if slot.current == s {
			slot.current = nil
		}
		slot.mu.Unlock()
	}()
	// Subscriptions retain lifetime; successful activation never retains setup.
	stopSetup()
	var tasks sync.WaitGroup
	run := func(fn func()) {
		tasks.Add(1)
		go func() { defer tasks.Done(); defer cancel(); defer func() { _ = recover() }(); fn() }()
	}
	run(func() { c.write(s) })
	run(func() { c.read(s) })
	run(func() { c.receive(s, sub, channels, authenticate) })
	<-ctx.Done()
	conn.Close()
	sub.Close()
	tasks.Wait()
}
func (c *ReliableChat) writeData(s *reliableSession, kind int, body []byte) error {
	return c.writeContext(s.ctx, s, kind, body)
}
func (c *ReliableChat) writeContext(parent context.Context, s *reliableSession, kind int, body []byte) error {
	ctx, cancel := context.WithTimeout(parent, c.options.WriteTimeout)
	defer cancel()
	select {
	case s.slot.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.slot.gate }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.active() {
		return context.Canceled
	}
	deadline, _ := ctx.Deadline()
	if err := s.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	return s.conn.WriteMessage(kind, body)
}
func chatControl(kind string, data any) []byte {
	body, _ := json.Marshal(struct {
		Type string `json:"type"`
		Data any    `json:"data"`
	}{kind, data})
	return body
}
func (c *ReliableChat) write(s *reliableSession) {
	if err := c.writeData(s, websocket.TextMessage, chatControl("ready", struct {
		ProtocolVersion     int    `json:"protocolVersion"`
		SessionID           string `json:"sessionId"`
		SyncIntervalSeconds int    `json:"syncIntervalSeconds"`
	}{1, s.id, max(1, int(c.options.RefreshInterval/time.Second))})); err != nil {
		return
	}
	ticker := time.NewTicker(c.options.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			if c.writeData(s, websocket.TextMessage, chatControl("ping", struct{}{})) != nil {
				return
			}
		case item := <-s.queue:
			if s.ctx.Err() != nil {
				return
			}
			if item.channel != "" {
				ctx, cancel := context.WithTimeout(s.ctx, c.options.ResolveTimeout)
				_, err := c.dispatcher.Dispatch(ctx, s.user, item.channel, item.body, func(ctx context.Context, f messaging.ServerFrame) error {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					return c.writeContext(ctx, s, websocket.TextMessage, f.Bytes())
				})
				cancel()
				if err != nil {
					return
				}
			} else if item.control != 0 {
				if c.writeData(s, item.control, item.body) != nil {
					return
				}
			} else {
				if c.writeData(s, websocket.TextMessage, item.body) != nil {
					return
				}
			}
		}
	}
}
func (c *ReliableChat) read(s *reliableSession) {
	s.conn.SetReadLimit(messaging.MaxBodyBytes)
	s.conn.SetReadDeadline(time.Now().Add(c.options.ReadTimeout))
	s.conn.SetPingHandler(func(body string) error {
		if err := s.conn.SetReadDeadline(time.Now().Add(c.options.ReadTimeout)); err != nil {
			return err
		}
		if !s.enqueue(reliableOutbound{control: websocket.PongMessage, body: []byte(body)}) {
			return context.Canceled
		}
		return nil
	})
	s.conn.SetPongHandler(func(string) error { return s.conn.SetReadDeadline(time.Now().Add(c.options.ReadTimeout)) })
	s.conn.SetCloseHandler(func(int, string) error { s.cancel(); return nil })
	for {
		kind, body, err := s.conn.ReadMessage()
		if err != nil || kind != websocket.TextMessage {
			return
		}
		frame, ack, err := messaging.DecodeClientFrame(body)
		if err != nil {
			return
		}
		if s.conn.SetReadDeadline(time.Now().Add(c.options.ReadTimeout)) != nil {
			return
		}
		switch frame {
		case "ping":
			if !s.enqueue(reliableOutbound{body: chatControl("pong", struct{}{})}) {
				return
			}
		case "pong":
			// Heartbeat activity only; business ACK is a separate protocol.
		case "ack":
			if c.acknowledge(s, s.id, ack) != nil {
				return
			}
		default:
			return
		}
	}
}

// The same gate serializes activation, socket writes and the entire ACK SQL
// callback, including COMMIT. A check followed by releasing the gate before
// SQL would allow a replaced session to advance the new session's cursor.
func (c *ReliableChat) acknowledge(s *reliableSession, sessionID string, ack messaging.DeliveryACK) error {
	ctx, cancel := context.WithTimeout(s.ctx, c.options.ResolveTimeout)
	defer cancel()
	select {
	case s.slot.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.slot.gate }()
	if ctx.Err() != nil || sessionID == "" || sessionID != s.id || !s.active() {
		return context.Canceled
	}
	seq, err := c.acknowledger.AcknowledgeDelivery(ctx, s.user, ack.ConversationID, ack.DeliveredSeq)
	if err != nil {
		return err
	}
	if ctx.Err() != nil || !s.active() {
		return context.Canceled
	}
	if seq < ack.DeliveredSeq {
		return ErrReliableChatInput
	}
	if !s.enqueue(reliableOutbound{body: chatControl("ack.confirmed", messaging.DeliveryACK{ConversationID: ack.ConversationID, DeliveredSeq: seq})}) {
		return context.Canceled
	}
	return nil
}
func (c *ReliableChat) receive(s *reliableSession, sub *reliableSubscription, channels []string, authenticate func(context.Context) (uint64, error)) {
	current := sub
	defer func() { current.Close() }()
	ticker := time.NewTicker(c.options.RefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case msg, ok := <-current.Channel():
			if !ok || msg == nil {
				return
			}
			h, err := messaging.DecodeNotificationHint(msg.Channel, []byte(msg.Payload))
			if err != nil || !h.AllowsUser(s.user) {
				return
			}
			if !s.enqueue(reliableOutbound{channel: msg.Channel, body: []byte(msg.Payload)}) {
				return
			}
		case <-ticker.C:
			setup, stop := context.WithTimeout(s.ctx, c.options.SetupTimeout)
			uid, err := authenticate(setup)
			if err != nil || uid != s.user {
				stop()
				return
			}
			next, err := c.source.Channels(setup, s.user, c.options.MaxGroups)
			if err != nil || !c.validChannels(s.user, next) {
				stop()
				return
			}
			if !reflect.DeepEqual(next, channels) {
				fresh, err := c.open(s.ctx, setup, next)
				if err != nil {
					stop()
					return
				}
				old := current
				current = fresh
				channels = append([]string(nil), next...)
				old.Close()
			}
			stop()
			if !s.enqueue(reliableOutbound{body: chatControl("sync.required", struct {
				Scope string `json:"scope"`
			}{"all"})}) {
				return
			}
		}
	}
}
