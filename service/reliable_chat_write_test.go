package service

import (
	"context"
	"net"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"iChat/messaging"
)

// Keep the real HTTP/WebSocket transport, withholding writes only after the
// handshake/ready. The controlled socket honors the supplied write deadline.
type chatWriteListener struct {
	net.Listener
	armed   atomic.Bool
	blocked atomic.Int32
}
type chatWriteConn struct {
	net.Conn
	owner    *chatWriteListener
	mu       sync.Mutex
	deadline time.Time
	closed   chan struct{}
	once     sync.Once
}

func (l *chatWriteListener) Accept() (net.Conn, error) {
	c, e := l.Listener.Accept()
	if e != nil {
		return nil, e
	}
	return &chatWriteConn{Conn: c, owner: l, closed: make(chan struct{})}, nil
}
func (c *chatWriteConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	return c.Conn.SetWriteDeadline(t)
}
func (c *chatWriteConn) Write(body []byte) (int, error) {
	if !c.owner.armed.Load() {
		return c.Conn.Write(body)
	}
	c.owner.blocked.Add(1)
	c.mu.Lock()
	deadline := c.deadline
	c.mu.Unlock()
	if deadline.IsZero() {
		return 0, ErrReliableChatInput
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-timer.C:
		return 0, &net.OpError{Op: "write", Net: "tcp", Err: context.DeadlineExceeded}
	case <-c.closed:
		return 0, net.ErrClosed
	}
}
func (c *chatWriteConn) Close() error { c.once.Do(func() { close(c.closed) }); return c.Conn.Close() }

func TestReliableChatBlockedSocketHonorsWriteDeadline(t *testing.T) {
	source := &reliableTestSource{channels: []string{"ichat:notify:v1:user:1"}}
	n, f := chatTestFrame(t)
	c, _ := NewReliableChat(source, notificationReaderFunc(func(context.Context, uint64, messaging.NotificationHint) (messaging.ServerFrame, error) {
		return f, nil
	}), deliveryAckTestFunc(unusedDeliveryACK), chatTestOptions())
	var finished atomic.Bool
	r := gin.New()
	r.GET("/chat", func(g *gin.Context) {
		defer finished.Store(true)
		c.Serve(g, func(context.Context) (uint64, error) { return 1, nil })
	})
	server := httptest.NewUnstartedServer(r)
	listener := &chatWriteListener{Listener: server.Listener}
	server.Listener = listener
	server.Start()
	defer server.Close()
	conn, _ := chatDial(t, server)
	listener.armed.Store(true)
	source.subscription(t, 0).messages <- &redis.Message{Channel: n.Channel(), Payload: string(n.Body())}
	chatEventually(t, func() bool { return finished.Load() })
	if listener.blocked.Load() == 0 {
		t.Fatal("socket write did not reach controlled deadline")
	}
	conn.Close()
}
