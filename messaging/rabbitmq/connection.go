package rabbitmq

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/url"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// ConnectionOptions bound an independently owned AMQP connection. There are
// no publishing slots or consumer subscriptions in these transport settings.
type ConnectionOptions struct {
	ConnectTimeout   time.Duration
	OperationTimeout time.Duration
	WriteTimeout     time.Duration
	CloseTimeout     time.Duration
	Heartbeat        time.Duration
	TLS              *tls.Config
}

func DefaultConnectionOptions() ConnectionOptions {
	return ConnectionOptions{ConnectTimeout: 5 * time.Second, OperationTimeout: 5 * time.Second, WriteTimeout: 3 * time.Second, CloseTimeout: 3 * time.Second, Heartbeat: 2 * time.Second}
}

// connectionOwner is instantiated per dial; no socket, write lock, shutdown
// signal or cancellation callback is shared between producer and consumer.
type connectionOwner struct {
	mu         sync.Mutex
	closed     bool
	transport  *boundedTransport
	connection *amqp.Connection
	options    ConnectionOptions
	done       chan struct{}
	calls      int
	idle       chan struct{}
	closing    bool
	closeDone  chan struct{}
	closeErr   error
	stopWork   func() // Must not perform RPC or acquire this owner's mutex.
	closeWork  func() // Runs under the owner's close deadline/socket watchdog.
}

func dialOwnedConnection(ctx context.Context, address string, options ConnectionOptions, dial func(context.Context, string, string) (net.Conn, error), prepare func(context.Context, *connectionOwner) error) (*connectionOwner, error) {
	if ctx == nil || dial == nil || !validConnectionOptions(address, options) {
		return nil, ErrSessionConfig
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.TLS != nil {
		options.TLS = options.TLS.Clone()
	}
	idle := make(chan struct{})
	close(idle)
	s := &connectionOwner{options: options, done: make(chan struct{}), idle: idle, closeDone: make(chan struct{})}
	connectCtx, cancel := context.WithTimeout(ctx, options.ConnectTimeout)
	stop := watchCancellation(connectCtx, s.abort)
	config := amqp.Config{Recovery: nil, Heartbeat: options.Heartbeat, Dial: func(network, address string) (net.Conn, error) {
		conn, err := dial(connectCtx, network, address)
		if err != nil {
			return nil, err
		}
		transport := &boundedTransport{Conn: conn, timeout: options.WriteTimeout, onFailure: s.abort, onClose: s.stopAdmission}
		s.mu.Lock()
		s.transport = transport
		closed := s.closed
		s.mu.Unlock()
		if closed {
			transport.Close()
			return nil, net.ErrClosed
		}
		return transport, nil
	}}
	if options.TLS != nil {
		config.TLSClientConfig = options.TLS.Clone()
		if config.TLSClientConfig.MinVersion == 0 {
			config.TLSClientConfig.MinVersion = tls.VersionTLS12
		}
	} else {
		config.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	conn, err := amqp.DialConfig(address, config)
	stop()
	connectErr := connectCtx.Err()
	cancel()
	s.mu.Lock()
	s.connection = conn
	closed := s.closed
	s.mu.Unlock()
	if err != nil || closed || connectErr != nil {
		s.abort()
		// The client may return a partially-open connection after a failed handshake.
		if conn != nil {
			conn.CloseDeadline(time.Now().Add(options.CloseTimeout))
		}
		if connectErr != nil {
			return nil, connectErr
		}
		return nil, ErrSessionConnect
	}
	setupCtx, setupCancel := context.WithTimeout(ctx, options.OperationTimeout)
	setupStop := watchCancellation(setupCtx, s.abort)
	if prepare != nil {
		err = prepare(setupCtx, s)
	}
	setupStop()
	setupErr := setupCtx.Err()
	setupCancel()
	s.mu.Lock()
	closed = s.closed
	s.mu.Unlock()
	if err != nil || setupErr != nil || closed {
		s.abort()
		s.Close(context.Background())
		if setupErr != nil {
			return nil, setupErr
		}
		if errors.Is(err, ErrSupervisorTopology) {
			return nil, ErrSupervisorTopology
		}
		return nil, ErrSessionSetup
	}
	return s, nil
}

func validConnectionOptions(address string, o ConnectionOptions) bool {
	for _, d := range []time.Duration{o.ConnectTimeout, o.OperationTimeout, o.WriteTimeout, o.CloseTimeout} {
		if d <= 0 || d > time.Minute {
			return false
		}
	}
	if o.Heartbeat < time.Second || o.Heartbeat > time.Minute || o.Heartbeat%time.Second != 0 {
		return false
	}
	u, err := url.Parse(address)
	if err != nil || u.User == nil || u.User.Username() == "" || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if password, ok := u.User.Password(); !ok || password == "" {
		return false
	}
	uri, err := amqp.ParseURI(address)
	if err != nil || uri.Port < 1 || uri.Port > 65535 {
		return false
	}
	if u.Scheme == "amqp" {
		ip := net.ParseIP(u.Hostname())
		if u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return false
		}
	} else if u.Scheme != "amqps" {
		return false
	}
	return o.TLS == nil || (!o.TLS.InsecureSkipVerify && (o.TLS.MinVersion == 0 || o.TLS.MinVersion >= tls.VersionTLS12) && (o.TLS.MaxVersion == 0 || o.TLS.MaxVersion >= tls.VersionTLS12))
}

func (s *connectionOwner) beginCall() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	if s.calls == 0 {
		s.idle = make(chan struct{})
	}
	s.calls++
	return true
}

func (s *connectionOwner) endCall() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls--
	if s.calls == 0 {
		close(s.idle)
	}
}

func (s *connectionOwner) quiescent() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.idle
}

func (s *connectionOwner) stopAdmission() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.done)
	}
	if s.stopWork != nil {
		s.stopWork()
	}
	s.mu.Unlock()
}

// abort performs no RPC. Closing the socket releases real Read/Write and
// causes the client's reader shutdown to release blocked channel RPCs.
func (s *connectionOwner) abort() {
	s.stopAdmission()
	s.mu.Lock()
	t := s.transport
	s.mu.Unlock()
	if t != nil {
		t.Close()
	}
}

func (s *connectionOwner) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		return ErrSessionConfig
	}
	closeCtx, cancel := context.WithTimeout(ctx, s.options.CloseTimeout)
	defer cancel()
	s.mu.Lock()
	if s.closing {
		done := s.closeDone
		s.mu.Unlock()
		select {
		case <-done:
			s.mu.Lock()
			err := s.closeErr
			s.mu.Unlock()
			return err
		case <-closeCtx.Done():
			return closeCtx.Err()
		}
	}
	s.closing = true
	closeWork, conn := s.closeWork, s.connection
	s.mu.Unlock()
	s.stopAdmission()
	stop := watchCancellation(closeCtx, s.abort)
	if closeWork != nil {
		closeWork()
	}
	var err error
	if conn != nil {
		deadline, _ := closeCtx.Deadline()
		if e := conn.CloseDeadline(deadline); e != nil && !errors.Is(e, amqp.ErrClosed) {
			err = ErrSessionClose
		}
	}
	s.abort()
	stop()
	select {
	case <-s.quiescent():
	case <-closeCtx.Done():
		err = closeCtx.Err()
	}
	if closeCtx.Err() != nil {
		err = closeCtx.Err()
	}
	s.mu.Lock()
	s.closeErr = err
	close(s.closeDone)
	s.mu.Unlock()
	return err
}

// Cancellation callbacks only interrupt the owned socket and are joined.
// No blocked RPC is hidden in a goroutine abandoned by its caller.
func watchCancellation(ctx context.Context, interrupt func()) func() {
	finished := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(finished); interrupt() })
	return func() {
		if !stop() {
			<-finished
		}
	}
}

type boundedTransport struct {
	net.Conn
	timeout   time.Duration
	onFailure func()
	onClose   func()
	writeMu   sync.Mutex
	closeOnce sync.Once
	closeErr  error
}

func (t *boundedTransport) Write(data []byte) (int, error) {
	t.writeMu.Lock()
	err := t.Conn.SetWriteDeadline(time.Now().Add(t.timeout))
	var n int
	if err == nil {
		n, err = t.Conn.Write(data)
	}
	t.writeMu.Unlock()
	if err != nil {
		t.onFailure()
	}
	return n, err
}

func (t *boundedTransport) Close() error {
	t.closeOnce.Do(func() { t.closeErr = t.Conn.Close() })
	if t.onClose != nil {
		t.onClose()
	}
	return t.closeErr
}

func (t *boundedTransport) Read(data []byte) (int, error) {
	n, err := t.Conn.Read(data)
	if err != nil {
		t.onFailure()
	}
	return n, err
}
