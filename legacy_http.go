package main

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
)

var (
	errLegacyHTTPInput    = errors.New("invalid legacy HTTP lifecycle input")
	errLegacyHTTPStopped  = errors.New("legacy HTTP admission stopped")
	errLegacyHTTPShutdown = errors.New("legacy HTTP shutdown failed")
)

// legacyHTTPServer owns every admitted handler, including hijacked WebSockets
// and replaced sessions still finishing cleanup. Handlers must return only
// after joining their work (as service.Chat does), and honor cancellation.
// Construction is idle; the caller supplies and transfers an owned listener.
type legacyHTTPServer struct {
	server   *http.Server
	requests *legacyHTTPRequests
}

func newLegacyHTTPServer(handler http.Handler) (*legacyHTTPServer, error) {
	if handler == nil {
		return nil, errLegacyHTTPInput
	}
	requests := &legacyHTTPRequests{next: handler, active: make(map[*legacyHTTPRequest]struct{}), done: make(chan struct{})}
	return &legacyHTTPServer{server: &http.Server{Handler: requests}, requests: requests}, nil
}

// Serve blocks until the accept loop ends, not until handlers finish. Its
// caller must join this call AND Drain/Close before releasing shared storage.
// The listener is owned by http.Server after this call, including on failure.
func (s *legacyHTTPServer) Serve(listener net.Listener) error {
	if listener == nil {
		return errLegacyHTTPInput
	}
	return s.server.Serve(listener)
}

// StopAdmission rejects new handlers and cancels hijacked requests. Ordinary
// admitted HTTP work keeps its context and can finish during the drain phase.
// It does not wait for handlers, subscriptions, or socket I/O.
func (s *legacyHTTPServer) StopAdmission() {
	s.requests.stop(false)
}

// RequestsDone is distinct from Serve returning: hijacked handlers can still
// be joining workers after the accept loop has stopped.
func (s *legacyHTTPServer) RequestsDone() <-chan struct{} { return s.requests.done }

// Drain shares the caller's single phase budget across net/http shutdown and
// the explicit handler join. net/http alone does not wait for hijacked sockets.
func (s *legacyHTTPServer) Drain(ctx context.Context) error {
	if ctx == nil {
		return errLegacyHTTPInput
	}
	s.StopAdmission()
	err := s.server.Shutdown(ctx)
	return legacyHTTPShutdownError(errors.Join(err, s.requests.wait(ctx)))
}

// Close also cancels ordinary requests and interrupts owned sockets. Even an
// expired observer requests the stop; a later call can wait again. A timeout
// does not permit the caller to close storage still used by active handlers.
func (s *legacyHTTPServer) Close(ctx context.Context) error {
	if ctx == nil {
		return errLegacyHTTPInput
	}
	s.requests.stop(true)
	err := s.server.Close()
	return legacyHTTPShutdownError(errors.Join(err, s.requests.wait(ctx)))
}

type legacyHTTPFailure struct{ cause error }

func (*legacyHTTPFailure) Error() string      { return errLegacyHTTPShutdown.Error() }
func (e *legacyHTTPFailure) String() string   { return e.Error() }
func (e *legacyHTTPFailure) GoString() string { return e.Error() }
func (e *legacyHTTPFailure) Unwrap() []error {
	return []error{errLegacyHTTPShutdown, e.cause}
}
func legacyHTTPShutdownError(cause error) error {
	if cause == nil {
		return nil
	}
	return &legacyHTTPFailure{cause: cause}
}

type legacyHTTPRequest struct {
	cancel context.CancelFunc
	conn   net.Conn // protected by the owning requests.mu
}

type legacyHTTPRequests struct {
	mu      sync.Mutex
	next    http.Handler
	stopped bool
	active  map[*legacyHTTPRequest]struct{}
	done    chan struct{}
}

func (s *legacyHTTPRequests) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	request := &legacyHTTPRequest{cancel: cancel}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		cancel()
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	s.active[request] = struct{}{}
	s.mu.Unlock()
	defer s.finish(request)
	writer := &legacyHTTPWriter{ResponseWriter: w, owner: s, request: request}
	var response http.ResponseWriter = writer
	if notifier, ok := w.(http.CloseNotifier); ok {
		// Gin's Stream helper still uses this optional, deprecated interface.
		// Advertise it only when the underlying server actually supports it.
		response = &legacyHTTPNotifyWriter{legacyHTTPWriter: writer, notifier: notifier}
	}
	s.next.ServeHTTP(response, r.WithContext(ctx))
}

func (s *legacyHTTPRequests) finish(request *legacyHTTPRequest) {
	request.cancel()
	s.mu.Lock()
	conn := request.conn
	s.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	s.mu.Lock()
	delete(s.active, request)
	s.completeLocked()
	s.mu.Unlock()
}

func (s *legacyHTTPRequests) completeLocked() {
	if s.stopped && len(s.active) == 0 {
		select {
		case <-s.done:
		default:
			close(s.done)
		}
	}
}

func (s *legacyHTTPRequests) stop(force bool) {
	s.mu.Lock()
	s.stopped = true
	var cancel []context.CancelFunc
	var sockets []net.Conn
	for request := range s.active {
		if force || request.conn != nil {
			cancel = append(cancel, request.cancel)
		}
		if force && request.conn != nil {
			sockets = append(sockets, request.conn)
		}
	}
	s.completeLocked()
	s.mu.Unlock()
	for _, stop := range cancel {
		stop()
	}
	for _, conn := range sockets {
		_ = conn.Close()
	}
}

func (s *legacyHTTPRequests) wait(ctx context.Context) error {
	select {
	case <-s.done:
		return nil
	default:
	}
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Preserve the interfaces used by Gin/Gorilla and ResponseController, while
// recording successful hijacks in the same request owner as ordinary HTTP.
type legacyHTTPWriter struct {
	http.ResponseWriter
	owner   *legacyHTTPRequests
	request *legacyHTTPRequest
}

func (w *legacyHTTPWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *legacyHTTPWriter) Flush()                      { _ = w.FlushError() }
func (w *legacyHTTPWriter) FlushError() error {
	return http.NewResponseController(w.ResponseWriter).Flush()
}
func (w *legacyHTTPWriter) Push(target string, options *http.PushOptions) error {
	if pusher, ok := w.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, options)
	}
	return http.ErrNotSupported
}

type legacyHTTPNotifyWriter struct {
	*legacyHTTPWriter
	notifier http.CloseNotifier
}

func (w *legacyHTTPNotifyWriter) CloseNotify() <-chan bool { return w.notifier.CloseNotify() }

func (w *legacyHTTPWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.owner.mu.Lock()
	stopped := w.owner.stopped
	w.owner.mu.Unlock()
	if stopped {
		return nil, nil, errLegacyHTTPStopped
	}
	conn, buffer, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		if conn != nil {
			_ = conn.Close()
		}
		return nil, nil, err
	}
	if conn == nil || buffer == nil {
		if conn != nil {
			_ = conn.Close()
		}
		return nil, nil, errLegacyHTTPInput
	}
	w.owner.mu.Lock()
	w.request.conn = conn
	stopped = w.owner.stopped
	w.owner.mu.Unlock()
	if stopped {
		// StopAdmission may have raced with the underlying Hijack. Do not
		// hand a new live socket to a handler after the stop boundary.
		w.request.cancel()
		_ = conn.Close()
		return nil, nil, errLegacyHTTPStopped
	}
	return conn, buffer, nil
}
