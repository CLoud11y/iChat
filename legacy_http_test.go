package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"iChat/database"
	"iChat/models"
	"iChat/service"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
)

func legacyHTTPAwait(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("legacy HTTP lifecycle did not reach expected stage")
	}
}

func startLegacyHTTPFixture(t *testing.T, handler http.Handler) (*legacyHTTPServer, string, <-chan error) {
	t.Helper()
	s, err := newLegacyHTTPServer(handler)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := s.Close(ctx); err != nil {
			t.Error("fixture failed to join HTTP handlers", err)
		}
		select {
		case err := <-done:
			if !errors.Is(err, http.ErrServerClosed) {
				t.Error("fixture serve error", err)
			}
		case <-ctx.Done():
			t.Error("fixture failed to join accept loop")
		}
	})
	return s, "http://" + listener.Addr().String(), done
}

func TestLegacyHTTPAdmissionAndDrain(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	finish := sync.OnceFunc(func() { close(release) })
	defer finish()
	var requestContext context.Context
	var calls atomic.Int32
	s, url, _ := startLegacyHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		requestContext = r.Context()
		close(entered)
		<-release
		_, _ = io.WriteString(w, "finished")
	}))
	t.Cleanup(finish)
	client := &http.Client{Timeout: 3 * time.Second}
	response := make(chan error, 1)
	go func() {
		resp, err := client.Get(url)
		if err == nil {
			defer resp.Body.Close()
			body, readErr := io.ReadAll(resp.Body)
			if readErr != nil || resp.StatusCode != http.StatusOK || string(body) != "finished" {
				err = fmt.Errorf("HTTP response status=%d body=%q read=%v", resp.StatusCode, body, readErr)
			}
		}
		response <- err
	}()
	legacyHTTPAwait(t, entered)
	s.StopAdmission()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Cache-Control") != "no-store" || calls.Load() != 1 {
		t.Fatal("new HTTP work entered after admission stopped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	drained := make(chan error, 1)
	go func() { drained <- s.Drain(ctx) }()
	if requestContext.Err() != nil {
		t.Fatal("ordinary request was cancelled before drain")
	}
	select {
	case err := <-drained:
		t.Fatal("drain skipped an admitted request", err)
	default:
	}
	finish()
	if err := <-response; err != nil {
		t.Fatal(err)
	}
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	if requestContext.Err() != context.Canceled {
		t.Fatal("finished request context was not released")
	}
}

func TestLegacyHTTPForcedCloseAndRetry(t *testing.T) {
	entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	finish := sync.OnceFunc(func() { close(release) })
	defer finish()
	s, url, _ := startLegacyHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(cancelled)
		<-release // model cleanup which has not yet joined
	}))
	t.Cleanup(finish)
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		client := &http.Client{Timeout: 3 * time.Second}
		if resp, err := client.Get(url); err == nil {
			_ = resp.Body.Close()
		}
	}()
	legacyHTTPAwait(t, entered)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	err := s.Drain(ctx)
	cancel()
	if !errors.Is(err, errLegacyHTTPShutdown) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("drain timeout not reported", err)
	}
	select {
	case <-cancelled:
		t.Fatal("drain cancelled ordinary in-flight work")
	default:
	}
	expired, stop := context.WithCancel(context.Background())
	stop()
	if err := s.Close(expired); !errors.Is(err, context.Canceled) {
		t.Fatal("expired Close did not report pending handlers", err)
	}
	legacyHTTPAwait(t, cancelled)
	legacyHTTPAwait(t, requestDone)
	select {
	case <-s.requests.done:
		t.Fatal("owner reported completion before handler cleanup joined")
	default:
	}
	finish()
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var joins sync.WaitGroup
	for range 8 {
		joins.Add(1)
		go func() {
			defer joins.Done()
			if err := s.Close(ctx); err != nil {
				t.Error("later concurrent close failed", err)
			}
		}()
	}
	joins.Wait()
	legacyHTTPAwait(t, s.requests.done)
}

type legacyHTTPSubscription struct {
	messages chan *redis.Message
	closed   chan struct{}
	once     sync.Once
}

func (s *legacyHTTPSubscription) Channel() <-chan *redis.Message { return s.messages }
func (s *legacyHTTPSubscription) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

type legacyHTTPSubscriptions struct {
	mu      sync.Mutex
	pending map[context.Context]*legacyHTTPSubscription
	created chan [2]*legacyHTTPSubscription
}

func (s *legacyHTTPSubscriptions) Subscribe(ctx context.Context, _ uint) (database.Subscription, error) {
	sub := &legacyHTTPSubscription{messages: make(chan *redis.Message, 1), closed: make(chan struct{})}
	s.mu.Lock()
	s.pending[ctx] = sub
	s.mu.Unlock()
	return sub, nil
}
func (s *legacyHTTPSubscriptions) SubscribeGroups(ctx context.Context, _ uint) (database.Subscription, error) {
	s.mu.Lock()
	private := s.pending[ctx]
	delete(s.pending, ctx)
	s.mu.Unlock()
	groups := &legacyHTTPSubscription{messages: make(chan *redis.Message, 1), closed: make(chan struct{})}
	s.created <- [2]*legacyHTTPSubscription{private, groups}
	return groups, nil
}

type legacyHTTPLogBarrier struct {
	entered, release chan struct{}
	once             sync.Once
}

func (*legacyHTTPLogBarrier) Levels() []logrus.Level { return logrus.AllLevels }
func (b *legacyHTTPLogBarrier) Fire(entry *logrus.Entry) error {
	if entry.Message == "unsupported websocket control message" && entry.Data["message_type"] == "hold-reader" {
		b.once.Do(func() { close(b.entered) })
		<-b.release
	}
	return nil
}

func TestLegacyHTTPJoinsReplacedAndActiveWebSockets(t *testing.T) {
	provider := &legacyHTTPSubscriptions{pending: make(map[context.Context]*legacyHTTPSubscription), created: make(chan [2]*legacyHTTPSubscription, 3)}
	barrier := &legacyHTTPLogBarrier{entered: make(chan struct{}), release: make(chan struct{})}
	release := sync.OnceFunc(func() { close(barrier.release) })
	defer release()
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	logger.AddHook(barrier)
	var mu sync.Mutex
	offline := make(map[uint]int)
	chat := service.Chat(provider, func(uid uint) { mu.Lock(); offline[uid]++; mu.Unlock() }, logger)
	routes := gin.New()
	routes.GET("/:user", func(c *gin.Context) {
		uid := uint(42)
		if c.Param("user") == "other" {
			uid = 99
		}
		c.Set("uid", uid)
		chat(c)
	})
	s, url, _ := startLegacyHTTPFixture(t, routes)
	t.Cleanup(release)
	connect := func(user string) (*websocket.Conn, [2]*legacyHTTPSubscription) {
		t.Helper()
		conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(url, "http")+"/"+user, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		var pair [2]*legacyHTTPSubscription
		select {
		case pair = <-provider.created:
		case <-time.After(3 * time.Second):
			t.Fatal("chat did not subscribe")
		}
		// An actual push proves the handler has registered its session and
		// started its workers after the HTTP upgrade response.
		raw, _ := json.Marshal(models.Message{Id: "owner-fixture", SenderId: 7, ReceiverId: 42, Type: models.PrivateType, Content: "ready"})
		pair[0].messages <- &redis.Message{Payload: string(raw)}
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Fatal(err)
		}
		return conn, pair
	}
	old, oldPair := connect("same")
	if err := old.WriteJSON(models.CtrlMsg{Type: "hold-reader"}); err != nil {
		t.Fatal(err)
	}
	legacyHTTPAwait(t, barrier.entered)
	current, currentPair := connect("same")
	other, otherPair := connect("other")
	for _, sub := range oldPair {
		legacyHTTPAwait(t, sub.closed)
	}
	s.StopAdmission()
	for _, pair := range [][2]*legacyHTTPSubscription{currentPair, otherPair} {
		for _, sub := range pair {
			legacyHTTPAwait(t, sub.closed)
		}
	}
	for _, conn := range []*websocket.Conn{current, other} {
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, _, err := conn.ReadMessage(); err == nil {
			t.Fatal("websocket transport remained open after cancellation")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	err := s.Drain(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("drain ignored the replaced reader still in cleanup", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Millisecond)
	err = s.Close(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("forced socket close was mistaken for a completed handler", err)
	}
	release()
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if offline[42] != 1 || offline[99] != 1 {
		t.Fatal("replacement or shutdown corrupted online state", offline)
	}
}

type legacyHTTPHijackWriter struct {
	*httptest.ResponseRecorder
	hijack func() (net.Conn, *bufio.ReadWriter, error)
}

func (w *legacyHTTPHijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) { return w.hijack() }

func TestLegacyHTTPHijackStopRace(t *testing.T) {
	for _, when := range []string{"before", "during"} {
		t.Run(when, func(t *testing.T) {
			entered, allowHijack, handlerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(allowHijack) })
			defer release()
			serverConn, clientConn := net.Pipe()
			defer serverConn.Close()
			defer clientConn.Close()
			var calls atomic.Int32
			writer := &legacyHTTPHijackWriter{ResponseRecorder: httptest.NewRecorder(), hijack: func() (net.Conn, *bufio.ReadWriter, error) {
				calls.Add(1)
				close(entered)
				<-allowHijack
				return serverConn, bufio.NewReadWriter(bufio.NewReader(serverConn), bufio.NewWriter(serverConn)), nil
			}}
			var h *legacyHTTPServer
			h, _ = newLegacyHTTPServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if when == "before" {
					h.StopAdmission()
				}
				conn, _, err := w.(http.Hijacker).Hijack()
				if conn != nil || !errors.Is(err, errLegacyHTTPStopped) {
					t.Error("stop race handed out a live socket", err)
				}
				if when == "during" && r.Context().Err() != context.Canceled {
					t.Error("late hijack request context was not cancelled")
				}
			}))
			go func() {
				defer close(handlerDone)
				h.requests.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/", nil))
			}()
			if when == "during" {
				legacyHTTPAwait(t, entered)
				h.StopAdmission()
				release()
			}
			legacyHTTPAwait(t, handlerDone)
			if want := map[string]int32{"before": 0, "during": 1}[when]; calls.Load() != want {
				t.Fatal("unexpected underlying hijack count", calls.Load())
			}
			if when == "during" {
				_ = clientConn.SetReadDeadline(time.Now().Add(time.Second))
				if _, err := clientConn.Read(make([]byte, 1)); err != io.EOF {
					t.Fatal("late hijack transport not closed", err)
				}
			}
			if err := h.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLegacyHTTPBoundariesAndWriterInterfaces(t *testing.T) {
	if _, err := newLegacyHTTPServer(nil); !errors.Is(err, errLegacyHTTPInput) {
		t.Fatal("nil handler accepted")
	}
	h, _ := newLegacyHTTPServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Second)); !errors.Is(err, http.ErrNotSupported) {
			t.Error("ResponseController did not unwrap writer", err)
		}
		if err := w.(http.Pusher).Push("/asset", nil); !errors.Is(err, http.ErrNotSupported) {
			t.Error("unsupported push error", err)
		}
		if _, _, err := w.(http.Hijacker).Hijack(); !errors.Is(err, http.ErrNotSupported) {
			t.Error("unsupported hijack error", err)
		}
		w.(http.Flusher).Flush()
	}))
	if err := h.Serve(nil); !errors.Is(err, errLegacyHTTPInput) {
		t.Fatal("nil listener accepted")
	}
	for _, err := range []error{h.Drain(nil), h.Close(nil)} {
		if !errors.Is(err, errLegacyHTTPInput) {
			t.Fatal("nil context accepted", err)
		}
	}
	writer := httptest.NewRecorder()
	h.requests.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/", nil))
	if !writer.Flushed {
		t.Fatal("Flush was not forwarded")
	}
	if err := h.Drain(context.Background()); err != nil {
		t.Fatal("idle drain failed", err)
	}
	if err := h.Close(context.Background()); err != nil {
		t.Fatal("idle close failed", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := h.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		t.Fatal("stopped server accepted a new Serve", err)
	}
	_ = listener.(*net.TCPListener).SetDeadline(time.Now().Add(time.Second))
	if conn, err := listener.Accept(); !errors.Is(err, net.ErrClosed) {
		if conn != nil {
			_ = conn.Close()
		}
		t.Fatal("refused Serve did not close transferred listener", err)
	}
	secret := errors.New("amqp://user:SECRET_TOKEN@internal")
	err = legacyHTTPShutdownError(errors.Join(secret, context.Canceled))
	if !errors.Is(err, secret) || !errors.Is(err, errLegacyHTTPShutdown) || !errors.Is(err, context.Canceled) {
		t.Fatal("shutdown causes lost")
	}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		if strings.Contains(fmt.Sprintf(format, err), "SECRET_TOKEN") {
			t.Fatal("formatted shutdown error leaked cause")
		}
	}
}

type legacyHTTPNotifyRecorder struct {
	*httptest.ResponseRecorder
	notified <-chan bool
}

func (w *legacyHTTPNotifyRecorder) CloseNotify() <-chan bool { return w.notified }

type legacyHTTPPlainWriter struct{ http.ResponseWriter }

func TestLegacyHTTPOptionalWriterCapabilities(t *testing.T) {
	notified := make(chan bool)
	notifier := &legacyHTTPNotifyRecorder{ResponseRecorder: httptest.NewRecorder(), notified: notified}
	h, _ := newLegacyHTTPServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, ok := w.(http.CloseNotifier); !ok || got.CloseNotify() != notified {
			t.Error("Gin CloseNotify was not preserved")
		}
	}))
	h.requests.ServeHTTP(notifier, httptest.NewRequest(http.MethodGet, "/", nil))
	h, _ = newLegacyHTTPServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(http.CloseNotifier); ok {
			t.Error("wrapper invented CloseNotify capability")
		}
		if err := http.NewResponseController(w).Flush(); !errors.Is(err, http.ErrNotSupported) {
			t.Error("ResponseController lost flush failure", err)
		}
	}))
	h.requests.ServeHTTP(legacyHTTPPlainWriter{httptest.NewRecorder()}, httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestLegacyHTTPFailedHijackOwnsPartialSocket(t *testing.T) {
	for _, failure := range []string{"error_with_handle", "no_buffer", "no_connection"} {
		t.Run(failure, func(t *testing.T) {
			serverConn, clientConn := net.Pipe()
			defer serverConn.Close()
			defer clientConn.Close()
			cause := errors.New("hijack failed")
			writer := &legacyHTTPHijackWriter{ResponseRecorder: httptest.NewRecorder(), hijack: func() (net.Conn, *bufio.ReadWriter, error) {
				switch failure {
				case "error_with_handle":
					return serverConn, nil, cause
				case "no_buffer":
					return serverConn, nil, nil
				default:
					return nil, nil, nil
				}
			}}
			h, _ := newLegacyHTTPServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, _, err := w.(http.Hijacker).Hijack()
				want := errLegacyHTTPInput
				if failure == "error_with_handle" {
					want = cause
				}
				if conn != nil || !errors.Is(err, want) {
					t.Error("bad hijack result accepted", err)
				}
			}))
			h.requests.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/", nil))
			if failure != "no_connection" {
				_ = clientConn.SetReadDeadline(time.Now().Add(time.Second))
				if _, err := clientConn.Read(make([]byte, 1)); err != io.EOF {
					t.Fatal("partial hijack socket was not closed", err)
				}
			}
			if err := h.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
