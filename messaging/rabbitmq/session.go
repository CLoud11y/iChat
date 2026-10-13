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
	"iChat/messaging"
)

var (
	ErrSessionConfig  = errors.New("invalid RabbitMQ session options or endpoint")
	ErrSessionConnect = errors.New("RabbitMQ TCP, TLS or AMQP handshake failed")
	ErrSessionSetup   = errors.New("RabbitMQ publisher channel setup failed")
	ErrSessionClose   = errors.New("RabbitMQ session graceful close failed")
)

type SessionOptions struct {
	Publisher        PublisherOptions
	ConnectTimeout   time.Duration
	OperationTimeout time.Duration
	WriteTimeout     time.Duration
	CloseTimeout     time.Duration
	Heartbeat        time.Duration
	TLS              *tls.Config
}

func DefaultSessionOptions() SessionOptions {
	return SessionOptions{Publisher: PublisherOptions{Slots: 8, ConnectionEpoch: 1}, ConnectTimeout: 5 * time.Second, OperationTimeout: 5 * time.Second, WriteTimeout: 3 * time.Second, CloseTimeout: 3 * time.Second, Heartbeat: 2 * time.Second}
}

// PublisherSession owns one socket, AMQP connection and publisher. A transport
// failure/cancel aborts that connection's pending work. It never redials, retries
// messages or exposes its private channels/connection to additional listeners.
type PublisherSession struct {
	mu         sync.Mutex
	closed     bool
	transport  *boundedTransport
	connection *amqp.Connection
	publisher  *Publisher
	options    SessionOptions
	done       chan struct{}
	calls      int
	idle       chan struct{}
	closing    bool
	closeDone  chan struct{}
	closeErr   error
}

func (s *PublisherSession) String() string { return "RabbitMQ owned publisher session" }

func DialPublisher(ctx context.Context, address string, options SessionOptions) (*PublisherSession, error) {
	return dialPublisher(ctx, address, options, (&net.Dialer{}).DialContext)
}

func dialPublisher(ctx context.Context, address string, options SessionOptions, dial func(context.Context, string, string) (net.Conn, error)) (*PublisherSession, error) {
	return dialPreparedPublisher(ctx, address, options, dial, nil)
}

// prepare runs against the new connection before creating publishing channels,
// under the same setup deadline and socket-interruption watchdog.
func dialPreparedPublisher(ctx context.Context, address string, options SessionOptions, dial func(context.Context, string, string) (net.Conn, error), prepare func(context.Context, *amqp.Connection) error) (*PublisherSession, error) {
	if ctx == nil || !validSessionOptions(address, options) {
		return nil, ErrSessionConfig
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	idle := make(chan struct{})
	close(idle)
	s := &PublisherSession{options: options, done: make(chan struct{}), idle: idle, closeDone: make(chan struct{})}
	connectCtx, cancel := context.WithTimeout(ctx, options.ConnectTimeout)
	stop := watchCancellation(connectCtx, s.abort)
	var transport *boundedTransport
	config := amqp.Config{Recovery: nil, Heartbeat: options.Heartbeat, Dial: func(network, address string) (net.Conn, error) {
		conn, err := dial(connectCtx, network, address)
		if err != nil {
			return nil, err
		}
		transport = &boundedTransport{Conn: conn, timeout: options.WriteTimeout, onFailure: s.abort, onClose: s.stopAdmission}
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
		// DialConfig can return a partially-open connection on handshake failure.
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
	var p *Publisher
	if prepare != nil {
		err = prepare(setupCtx, conn)
	}
	if err == nil {
		p, err = newPublisher(func() (publishChannel, error) { return conn.Channel() }, options.Publisher)
	}
	setupStop()
	setupErr := setupCtx.Err()
	setupCancel()
	if err != nil || setupErr != nil {
		s.abort()
		if p != nil {
			p.Close()
		}
		conn.CloseDeadline(time.Now().Add(options.CloseTimeout))
		if setupErr != nil {
			return nil, setupErr
		}
		if errors.Is(err, ErrSupervisorTopology) {
			return nil, ErrSupervisorTopology
		}
		return nil, ErrSessionSetup
	}
	p.interrupt, p.closeTimeout = s.abort, options.CloseTimeout
	s.mu.Lock()
	s.publisher = p
	closed = s.closed
	s.mu.Unlock()
	if closed {
		p.Close()
		conn.CloseDeadline(time.Now().Add(options.CloseTimeout))
		return nil, ErrSessionSetup
	}
	return s, nil
}

func validSessionOptions(address string, o SessionOptions) bool {
	if o.Publisher.Slots < 1 || o.Publisher.Slots > MaxPublisherSlots || o.Publisher.ConnectionEpoch == 0 {
		return false
	}
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

func (s *PublisherSession) Publish(ctx context.Context, command messaging.Command, receivedAt time.Time) (PublishResult, error) {
	r := PublishResult{Outcome: PublishNotStarted}
	if s == nil || ctx == nil {
		return publishFailure(r, ErrPublisherUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return publishFailure(r, err)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return publishFailure(r, ErrPublisherUnavailable)
	}
	if s.calls == 0 {
		s.idle = make(chan struct{})
	}
	s.calls++
	p := s.publisher
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.calls--
		if s.calls == 0 {
			close(s.idle)
		}
		s.mu.Unlock()
	}()
	operationCtx, cancel := context.WithTimeout(ctx, s.options.OperationTimeout)
	defer cancel()
	return p.Publish(operationCtx, command, receivedAt)
}

// Done signals permanently stopped admission, not completed client teardown.
func (s *PublisherSession) Done() <-chan struct{} { return s.done }

func (s *PublisherSession) quiescent() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.idle
}

func (s *PublisherSession) stopAdmission() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.done)
	}
	p := s.publisher
	if p != nil {
		p.stopAdmission()
	}
	s.mu.Unlock()
}

// abort never calls an AMQP RPC. net.Conn.Close releases pending Read/Write;
// the fixed client's reader shutdown then releases blocked channel RPCs.
func (s *PublisherSession) abort() {
	s.stopAdmission()
	s.mu.Lock()
	t := s.transport
	s.mu.Unlock()
	if t != nil {
		t.Close()
	}
}

func (s *PublisherSession) Close(ctx context.Context) error {
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
	p, conn := s.publisher, s.connection
	s.mu.Unlock()
	s.stopAdmission()
	stop := watchCancellation(closeCtx, s.abort)
	if p != nil {
		p.Close()
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
	s.mu.Lock()
	idle := s.idle
	s.mu.Unlock()
	select {
	case <-idle:
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

// No worker goroutine is abandoned to turn an uninterruptible RPC into a
// timeout. At most one cancellation callback per active operation/slot close
// exists; it closes the socket, and its completion is joined before return.
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
