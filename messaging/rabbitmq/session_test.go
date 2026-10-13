package rabbitmq

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"iChat/messaging"
)

type observedSocket struct {
	net.Conn
	once        sync.Once
	closed      chan struct{}
	count       atomic.Int32
	trackWrites atomic.Bool
	writeEvents chan struct{}
}

func (c *observedSocket) Write(data []byte) (int, error) {
	if c.trackWrites.Load() {
		select {
		case c.writeEvents <- struct{}{}:
		default:
		}
	}
	return c.Conn.Write(data)
}

func (c *observedSocket) Close() error {
	c.once.Do(func() { c.count.Add(1); close(c.closed) })
	return c.Conn.Close()
}

type wireFrame struct {
	kind    byte
	channel uint16
	payload []byte
}
type wirePublish struct {
	channel uint16
	tag     uint64
	body    []byte
}
type sessionPeer struct {
	socket    *observedSocket
	done      chan struct{}
	received  chan wirePublish
	ready     chan struct{}
	dialCalls atomic.Int32
	modes     string
	slots     int
	server    net.Conn
}

func testSessionOptions() SessionOptions {
	o := DefaultSessionOptions()
	o.Publisher.Slots = 1
	o.ConnectTimeout = 200 * time.Millisecond
	o.OperationTimeout = 500 * time.Millisecond
	o.WriteTimeout = 80 * time.Millisecond
	o.CloseTimeout = 100 * time.Millisecond
	return o
}

func newSessionPeer(t *testing.T, mode string, slots int) *sessionPeer {
	t.Helper()
	client, server := net.Pipe()
	p := &sessionPeer{socket: &observedSocket{Conn: client, closed: make(chan struct{}), writeEvents: make(chan struct{}, 1)}, done: make(chan struct{}), received: make(chan wirePublish, 16), ready: make(chan struct{}), modes: mode, slots: slots, server: server}
	go func() { defer close(p.done); defer server.Close(); p.serve(server) }()
	t.Cleanup(func() {
		p.socket.Close()
		select {
		case <-p.done:
		case <-time.After(2 * time.Second):
			t.Error("AMQP peer did not exit")
		}
	})
	return p
}
func (p *sessionPeer) dial(ctx context.Context, network, address string) (net.Conn, error) {
	p.dialCalls.Add(1)
	return p.socket, nil
}

func (p *sessionPeer) serve(conn net.Conn) {
	if p.modes == "unread_handshake" {
		<-p.socket.closed
		return
	}
	header := make([]byte, 8)
	if _, err := io.ReadFull(conn, header); err != nil {
		return
	}
	if !bytes.Equal(header, []byte{'A', 'M', 'Q', 'P', 0, 0, 9, 1}) {
		return
	}
	if p.modes == "silent_handshake" {
		<-p.socket.closed
		return
	}
	start := []byte{0, 9, 0, 0, 0, 0}
	start = append(start, wireLong("PLAIN")...)
	start = append(start, wireLong("en_US")...)
	if wireMethod(conn, 0, 10, 10, start) != nil {
		return
	}
	if _, err := wireRead(conn); err != nil {
		return
	} // start-ok
	tune := make([]byte, 8)
	binary.BigEndian.PutUint16(tune, 64)
	binary.BigEndian.PutUint32(tune[2:], 131072)
	binary.BigEndian.PutUint16(tune[6:], 2)
	if wireMethod(conn, 0, 10, 30, tune) != nil {
		return
	}
	if _, err := wireRead(conn); err != nil {
		return
	} // tune-ok
	if _, err := wireRead(conn); err != nil {
		return
	} // connection.open
	if wireMethod(conn, 0, 10, 41, []byte{0}) != nil {
		return
	}
	tags := map[uint16]uint64{}
	headers := map[uint16][]byte{}
	bodies := map[uint16][]byte{}
	sizes := map[uint16]uint64{}
	configured := 0
	for {
		f, err := wireRead(conn)
		if err != nil {
			return
		}
		if f.kind == 8 {
			continue
		}
		if f.kind == 1 {
			if len(f.payload) < 4 {
				return
			}
			class, method := binary.BigEndian.Uint16(f.payload), binary.BigEndian.Uint16(f.payload[2:])
			switch {
			case class == 20 && method == 10:
				if p.modes == "silent_channel_open" {
					<-p.socket.closed
					return
				}
				if wireMethod(conn, f.channel, 20, 11, []byte{0, 0, 0, 0}) != nil {
					return
				}
			case class == 85 && method == 10:
				if p.modes == "silent_confirm_select" {
					<-p.socket.closed
					return
				}
				if wireMethod(conn, f.channel, 85, 11, nil) != nil {
					return
				}
				configured++
				if configured == p.slots {
					close(p.ready)
					if p.modes == "blocked_write" {
						<-p.socket.closed
						return
					}
				}
			case class == 60 && method == 40:
				tags[f.channel]++
				bodies[f.channel] = nil
			case class == 20 && method == 40:
				if p.modes == "silent_close" || p.modes == "return_silent_close" {
					continue
				}
				if wireMethod(conn, f.channel, 20, 41, nil) != nil {
					return
				}
			case class == 10 && method == 50:
				if p.modes == "silent_connection_close" {
					continue
				}
				wireMethod(conn, 0, 10, 51, nil)
				return
			default:
				return
			}
		} else if f.kind == 2 {
			if len(f.payload) < 12 {
				return
			}
			headers[f.channel] = f.payload
			sizes[f.channel] = binary.BigEndian.Uint64(f.payload[4:])
		} else if f.kind == 3 {
			bodies[f.channel] = append(bodies[f.channel], f.payload...)
			if uint64(len(bodies[f.channel])) != sizes[f.channel] {
				continue
			}
			p.received <- wirePublish{f.channel, tags[f.channel], append([]byte(nil), bodies[f.channel]...)}
			if p.modes == "lost_confirm" {
				continue
			}
			if p.modes == "return_silent_close" {
				ret := make([]byte, 2)
				binary.BigEndian.PutUint16(ret, 312)
				ret = append(ret, wireShort("NO_ROUTE")...)
				ret = append(ret, wireShort(CommandsExchange)...)
				ret = append(ret, wireShort(SubmitRoutingKey)...)
				if wireMethod(conn, f.channel, 60, 50, ret) != nil {
					return
				}
				if wireWrite(conn, wireFrame{2, f.channel, headers[f.channel]}) != nil {
					return
				}
				if wireWrite(conn, wireFrame{3, f.channel, bodies[f.channel]}) != nil {
					return
				}
			}
			ack := make([]byte, 9)
			binary.BigEndian.PutUint64(ack, tags[f.channel])
			if wireMethod(conn, f.channel, 60, 80, ack) != nil {
				return
			}
		}
	}
}

func wireShort(s string) []byte { return append([]byte{byte(len(s))}, []byte(s)...) }
func wireLong(s string) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(len(s)))
	return append(b, []byte(s)...)
}
func wireMethod(c net.Conn, ch, class, method uint16, data []byte) error {
	b := make([]byte, 4)
	binary.BigEndian.PutUint16(b, class)
	binary.BigEndian.PutUint16(b[2:], method)
	return wireWrite(c, wireFrame{1, ch, append(b, data...)})
}
func wireWrite(c net.Conn, f wireFrame) error {
	b := make([]byte, 7)
	b[0] = f.kind
	binary.BigEndian.PutUint16(b[1:], f.channel)
	binary.BigEndian.PutUint32(b[3:], uint32(len(f.payload)))
	b = append(b, f.payload...)
	b = append(b, 0xce)
	for len(b) > 0 {
		n, err := c.Write(b)
		if err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}
func wireRead(c net.Conn) (wireFrame, error) {
	b := make([]byte, 7)
	if _, err := io.ReadFull(c, b); err != nil {
		return wireFrame{}, err
	}
	size := binary.BigEndian.Uint32(b[3:])
	if size > 1<<20 {
		return wireFrame{}, io.ErrUnexpectedEOF
	}
	payload := make([]byte, int(size)+1)
	if _, err := io.ReadFull(c, payload); err != nil {
		return wireFrame{}, err
	}
	if payload[size] != 0xce {
		return wireFrame{}, io.ErrUnexpectedEOF
	}
	return wireFrame{b[0], binary.BigEndian.Uint16(b[1:]), payload[:size]}, nil
}

const testSessionURI = "amqp://ichat_app:private-secret@127.0.0.1:5672/%2Fichat"

func dialPeerSession(t *testing.T, p *sessionPeer, o SessionOptions) *PublisherSession {
	t.Helper()
	s, err := dialPublisher(context.Background(), testSessionURI, o, p.dial)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(context.Background()) })
	return s
}

func TestSessionHandshakeAndSetupHardBounds(t *testing.T) {
	for _, mode := range []string{"unread_handshake", "silent_handshake", "silent_channel_open", "silent_confirm_select"} {
		t.Run(mode, func(t *testing.T) {
			p := newSessionPeer(t, mode, 1)
			o := testSessionOptions()
			started := time.Now()
			if s, err := dialPublisher(context.Background(), testSessionURI, o, p.dial); err == nil || s != nil || strings.Contains(err.Error(), "private-secret") {
				t.Fatal("stalled setup succeeded or leaked credentials")
			}
			if elapsed := time.Since(started); elapsed > 2*time.Second {
				t.Fatal("handshake/setup was unbounded", elapsed)
			}
			if p.socket.count.Load() != 1 {
				t.Fatal("failed setup did not close owned socket once")
			}
			select {
			case <-p.done:
			case <-time.After(time.Second):
				t.Fatal("failed setup left peer running")
			}
		})
	}
}

func TestSessionBlockedWriteAndLostConfirmAreUnknown(t *testing.T) {
	for _, mode := range []string{"blocked_write", "lost_confirm"} {
		t.Run(mode, func(t *testing.T) {
			p := newSessionPeer(t, mode, 1)
			o := testSessionOptions()
			s := dialPeerSession(t, p, o)
			started := time.Now()
			r, err := s.Publish(context.Background(), publisherCommand(t), time.Now())
			if err == nil || r.Outcome != PublishUnknown {
				t.Fatal("stalled publication was definite", r, err)
			}
			if time.Since(started) > 2*time.Second {
				t.Fatal("publish did not return within hard bound")
			}
			select {
			case <-s.Done():
			default:
				t.Fatal("failed transport stayed admissible")
			}
			if r, err := s.Publish(context.Background(), publisherCommand(t), time.Now()); !errors.Is(err, ErrPublisherUnavailable) || r.Outcome != PublishNotStarted {
				t.Fatal("aborted session reused", r, err)
			}
			if mode == "lost_confirm" {
				select {
				case w := <-p.received:
					e, err := messaging.DecodeEnvelope(w.body)
					if err != nil || e.PublishAttemptID != r.PublishAttemptID {
						t.Fatal("unknown did not preserve sent attempt", err)
					}
				default:
					t.Fatal("missing confirm test did not actually receive publication")
				}
			}
			if p.socket.count.Load() != 1 {
				t.Fatal("abort did not close socket once")
			}
		})
	}
}

func TestSessionCancelAbortsAllPendingSlots(t *testing.T) {
	p := newSessionPeer(t, "lost_confirm", 2)
	o := testSessionOptions()
	o.Publisher.Slots = 2
	o.OperationTimeout = 2 * time.Second
	s := dialPeerSession(t, p, o)
	c := publisherCommand(t)
	first, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan PublishResult, 2)
	failures := make(chan error, 2)
	for _, ctx := range []context.Context{first, context.Background()} {
		go func(ctx context.Context) { r, err := s.Publish(ctx, c, time.Now()); results <- r; failures <- err }(ctx)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-p.received:
		case <-time.After(time.Second):
			t.Fatal("two publications never reached peer")
		}
	}
	started := time.Now()
	cancel()
	for i := 0; i < 2; i++ {
		select {
		case r := <-results:
			if err := <-failures; r.Outcome != PublishUnknown || err == nil {
				t.Fatal("shared abort accepted a pending attempt", r, err)
			}
		case <-time.After(time.Second):
			t.Fatal("shared abort left pending call running")
		}
	}
	if time.Since(started) > time.Second || p.socket.count.Load() != 1 {
		t.Fatal("shared socket abort not bounded or idempotent")
	}
}

func TestSessionPrewriteFailuresDoNotAbortHealthyConnection(t *testing.T) {
	p := newSessionPeer(t, "normal", 1)
	s := dialPeerSession(t, p, testSessionOptions())
	c := publisherCommand(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r, err := s.Publish(ctx, c, time.Now()); !errors.Is(err, context.Canceled) || r.Outcome != PublishNotStarted {
		t.Fatal("precanceled publish wrote", r, err)
	}
	bad := c
	bad.Content = "tampered"
	if r, err := s.Publish(context.Background(), bad, time.Now()); err == nil || r.Outcome != PublishNotStarted {
		t.Fatal("invalid command wrote")
	}
	select {
	case <-s.Done():
		t.Fatal("prewrite failure aborted connection")
	default:
	}
	r, err := s.Publish(context.Background(), c, time.Now())
	if err != nil || r.Outcome != PublishAccepted {
		t.Fatal("healthy session unavailable", r, err)
	}
	if strings.Contains(fmt.Sprintf("%+v", s), "private-secret") {
		t.Fatal("session formatting leaked credentials")
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal("graceful close failed", err)
	}
}

func TestSessionRetirementAndShutdownHardBounds(t *testing.T) {
	for _, mode := range []string{"return_silent_close", "silent_close", "silent_connection_close"} {
		t.Run(mode, func(t *testing.T) {
			p := newSessionPeer(t, mode, 1)
			s := dialPeerSession(t, p, testSessionOptions())
			started := time.Now()
			if mode == "return_silent_close" {
				r, err := s.Publish(context.Background(), publisherCommand(t), time.Now())
				if !errors.Is(err, ErrPublisherReturn) || r.Outcome != PublishUnroutable {
					t.Fatal("verified return lost its outcome", r, err)
				}
			} else if err := s.Close(context.Background()); err == nil {
				t.Fatal("silent close reported graceful completion")
			}
			if time.Since(started) > time.Second {
				t.Fatal("retirement or shutdown waited forever")
			}
			if p.socket.count.Load() != 1 {
				t.Fatal("close did not force socket shutdown")
			}
			select {
			case <-p.done:
			case <-time.After(time.Second):
				t.Fatal("close left peer running")
			}
		})
	}
}

func TestSessionBusyRequestDoesNotCancelAnotherPublication(t *testing.T) {
	p := newSessionPeer(t, "lost_confirm", 1)
	o := testSessionOptions()
	s := dialPeerSession(t, p, o)
	c := publisherCommand(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := s.Publish(ctx, c, time.Now()); done <- err }()
	select {
	case <-p.received:
	case <-time.After(time.Second):
		t.Fatal("first publish never reached peer")
	}
	short, stop := context.WithTimeout(context.Background(), time.Nanosecond)
	defer stop()
	if r, err := s.Publish(short, c, time.Now()); err == nil || r.Outcome != PublishNotStarted {
		t.Fatal("busy expired request wrote")
	}
	if r, err := s.Publish(context.Background(), c, time.Now()); !errors.Is(err, ErrPublisherBusy) || r.Outcome != PublishNotStarted {
		t.Fatal("busy pool wrote", r, err)
	}
	select {
	case <-s.Done():
		t.Fatal("busy request aborted another attempt")
	default:
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("pending attempt did not stop")
	}
}

func TestSessionConfigurationAndTLSHandshakeBounds(t *testing.T) {
	o := testSessionOptions()
	for _, address := range []string{"amqp://private-secret", "amqp://u:private-secret@example.com", "amqp://u:private-secret@localhost?heartbeat=0", "http://u:private-secret@localhost"} {
		if _, err := dialPublisher(context.Background(), address, o, func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("invalid endpoint dialed")
			return nil, nil
		}); !errors.Is(err, ErrSessionConfig) || strings.Contains(err.Error(), "private-secret") {
			t.Fatal("invalid endpoint accepted or leaked")
		}
	}
	bad := o
	bad.TLS = &tls.Config{InsecureSkipVerify: true}
	if validSessionOptions(testSessionURI, bad) {
		t.Fatal("insecure TLS accepted")
	}
	for _, duration := range []time.Duration{0, -time.Second, time.Hour} {
		bad := o
		bad.WriteTimeout = duration
		if validSessionOptions(testSessionURI, bad) {
			t.Fatal("invalid timeout accepted")
		}
	}
	p := newSessionPeer(t, "unread_handshake", 1)
	started := time.Now()
	if _, err := dialPublisher(context.Background(), "amqps://u:private-secret@localhost", o, p.dial); err == nil || strings.Contains(err.Error(), "private-secret") {
		t.Fatal("silent TLS handshake accepted or leaked")
	}
	if time.Since(started) > time.Second || p.socket.count.Load() != 1 {
		t.Fatal("TLS handshake unbounded or socket leaked")
	}
}

func TestSessionParentCancellationDuringTCPDial(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	s, err := dialPublisher(ctx, testSessionURI, testSessionOptions(), func(ctx context.Context, network, address string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if s != nil || !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatal("TCP dial did not honor caller deadline", err)
	}
}

func TestSessionIdlePeerEOFStopsAdmission(t *testing.T) {
	p := newSessionPeer(t, "normal", 1)
	s := dialPeerSession(t, p, testSessionOptions())
	p.server.Close()
	select {
	case <-s.Done():
	case <-time.After(time.Second):
		t.Fatal("idle reader EOF did not stop session admission")
	}
	if r, err := s.Publish(context.Background(), publisherCommand(t), time.Now()); !errors.Is(err, ErrPublisherUnavailable) || r.Outcome != PublishNotStarted {
		t.Fatal("idle closed connection admitted a new attempt", r, err)
	}
}

func TestSessionCompletedWatchdogsDoNotAbortLaterWork(t *testing.T) {
	p := newSessionPeer(t, "normal", 1)
	o := testSessionOptions()
	o.OperationTimeout = 100 * time.Millisecond
	parent, cancel := context.WithCancel(context.Background())
	s, err := dialPublisher(parent, testSessionURI, o, p.dial)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	t.Cleanup(func() { s.Close(context.Background()) })
	c := publisherCommand(t)
	for i := 0; i < 2; i++ {
		r, err := s.Publish(context.Background(), c, time.Now())
		if err != nil || r.Outcome != PublishAccepted {
			t.Fatal("completed watchdog affected a later request", r, err)
		}
		if i == 0 {
			time.Sleep(150 * time.Millisecond)
		}
	}
	select {
	case <-s.Done():
		t.Fatal("completed watchdog kept its cancellation callback")
	default:
	}
}

func TestSessionConcurrentCloseInterruptsBlockedPublish(t *testing.T) {
	p := newSessionPeer(t, "blocked_write", 1)
	o := testSessionOptions()
	o.WriteTimeout = time.Second
	o.OperationTimeout = 2 * time.Second
	s := dialPeerSession(t, p, o)
	c := publisherCommand(t)
	publication := make(chan PublishResult, 1)
	p.socket.trackWrites.Store(true)
	go func() { r, _ := s.Publish(context.Background(), c, time.Now()); publication <- r }()
	select {
	case <-p.socket.writeEvents:
	case <-time.After(time.Second):
		t.Fatal("publication did not enter the transport write")
	}
	// The session test peer has stopped reading after confirm.select-ok.
	// Closing concurrently must release the actual blocked net.Pipe write.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	_ = s.Close(ctx)
	if time.Since(started) > time.Second {
		t.Fatal("close waited for operation/write timeout instead of caller deadline")
	}
	select {
	case r := <-publication:
		if r.Outcome != PublishUnknown {
			t.Fatal("shutdown accepted unfinished publication", r)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown abandoned publication goroutine")
	}
	if p.socket.count.Load() != 1 {
		t.Fatal("concurrent close duplicated socket cleanup")
	}
}

func TestSessionRealTCPSilentTLSHandshake(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		io.Copy(io.Discard, conn)
	}()
	started := time.Now()
	address := "amqps://u:private-secret@" + listener.Addr().String() + "/%2Fichat"
	s, err := DialPublisher(context.Background(), address, testSessionOptions())
	if s != nil || err == nil || strings.Contains(err.Error(), "private-secret") || time.Since(started) > time.Second {
		t.Fatal("real TCP TLS blackhole was unbounded or leaked credentials", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("TLS failure left the TCP peer open")
	}
}
