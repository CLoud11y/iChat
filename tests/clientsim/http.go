package clientsim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"iChat/messaging"
)

var (
	ErrHTTPOptions       = errors.New("invalid simulator HTTP options")
	ErrHTTPClosed        = errors.New("simulator HTTP runner closed")
	ErrHTTPUnavailable   = errors.New("simulator HTTP unavailable")
	ErrAuthentication    = errors.New("simulator authentication required")
	ErrHTTPRejected      = errors.New("simulator HTTP request rejected")
	ErrRecoveryExhausted = errors.New("simulator recovery budget exhausted")
)

type HTTPOptions struct {
	Origin         string
	Token          func(context.Context) (string, error)
	RequestTimeout time.Duration
	StoreTimeout   time.Duration
	MaxConcurrent  int
}

func DefaultHTTPOptions(origin string, token func(context.Context) (string, error)) HTTPOptions {
	return HTTPOptions{Origin: origin, Token: token, RequestTimeout: 3 * time.Second, StoreTimeout: 3 * time.Second, MaxConcurrent: 2}
}
func (HTTPOptions) String() string     { return "simulator HTTP options (origin and credentials redacted)" }
func (o HTTPOptions) GoString() string { return o.String() }

type HTTPRunner struct {
	sender    *Sender
	options   HTTPOptions
	client    *http.Client
	transport *http.Transport
	work      context.Context
	cancel    context.CancelFunc
	slots     chan struct{}
	mu        sync.Mutex
	closed    bool
	keys      map[string]bool
	active    sync.WaitGroup
	closeOnce sync.Once
	wait      func(context.Context, time.Duration) error
}

func (*HTTPRunner) String() string     { return "simulator HTTP runner (origin and credentials redacted)" }
func (r *HTTPRunner) GoString() string { return r.String() }

// NewHTTPRunner owns its client/transport, with no ambient proxy, cookies,
// redirects, compression, idempotency headers or replayable POST body.
// Token is borrowed and must honor ctx; callbacks are never abandoned.
func NewHTTPRunner(sender *Sender, options HTTPOptions) (*HTTPRunner, error) {
	u, err := url.Parse(options.Origin)
	valid := func(d time.Duration) bool { return d > 0 && d <= time.Minute }
	if sender == nil || sender.user == 0 || nilStore(sender.store) || err != nil || u.Host == "" || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || options.Token == nil || !valid(options.RequestTimeout) || !valid(options.StoreTimeout) || options.MaxConcurrent < 1 || options.MaxConcurrent > 8 {
		return nil, ErrHTTPOptions
	}
	options.Origin = strings.TrimSuffix(u.String(), "/")
	dialer := &net.Dialer{Timeout: options.RequestTimeout, KeepAlive: 30 * time.Second}
	transport := &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: options.RequestTimeout, ResponseHeaderTimeout: options.RequestTimeout, ExpectContinueTimeout: time.Second, MaxConnsPerHost: options.MaxConcurrent, MaxIdleConnsPerHost: options.MaxConcurrent, MaxIdleConns: options.MaxConcurrent, IdleConnTimeout: 30 * time.Second, DisableCompression: true}
	work, cancel := context.WithCancel(context.Background())
	r := &HTTPRunner{sender: sender, options: options, transport: transport, work: work, cancel: cancel, slots: make(chan struct{}, options.MaxConcurrent), keys: make(map[string]bool), wait: waitRecovery}
	r.client = &http.Client{Transport: transport, Timeout: options.RequestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return r, nil
}

func (r *HTTPRunner) enter(ctx context.Context, key string) (context.Context, func(), error) {
	if ctx == nil {
		return nil, nil, ErrInput
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, nil, ErrHTTPClosed
	}
	if r.keys[key] {
		r.mu.Unlock()
		return nil, nil, ErrInFlight
	}
	r.keys[key] = true
	r.active.Add(1)
	r.mu.Unlock()
	work, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(r.work, cancel)
	return work, func() {
		stop()
		cancel()
		r.mu.Lock()
		delete(r.keys, key)
		r.mu.Unlock()
		r.active.Done()
	}, nil
}

// Close cancels requests, token callbacks, slot waiters and backoffs, then joins
// actual calls before releasing idle HTTP connections. It never closes Sender
// or its borrowed disk store. Concurrent Close callers join the same cleanup.
func (r *HTTPRunner) Close() {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		r.cancel()
		r.mu.Unlock()
		r.active.Wait()
		r.transport.CloseIdleConnections()
	})
}

func (r *HTTPRunner) Submit(ctx context.Context, key string) error {
	work, done, err := r.enter(ctx, key)
	if err != nil {
		return err
	}
	defer done()
	_, err = r.submit(work, key)
	return err
}
func (r *HTTPRunner) Poll(ctx context.Context, key string) error {
	work, done, err := r.enter(ctx, key)
	if err != nil {
		return err
	}
	defer done()
	_, err = r.poll(work, key)
	return err
}

type httpAdvice struct {
	after   time.Duration
	pending bool
}

func (r *HTTPRunner) requestContext(ctx context.Context) (context.Context, func(), string, error) {
	work, cancel := context.WithTimeout(ctx, r.options.RequestTimeout)
	select {
	case r.slots <- struct{}{}:
	case <-work.Done():
		cancel()
		return nil, nil, "", work.Err()
	}
	done := func() { cancel(); <-r.slots }
	token, err := r.options.Token(work)
	if err != nil {
		done()
		return nil, nil, "", safeError(ErrAuthentication, err)
	}
	if err := work.Err(); err != nil {
		done()
		return nil, nil, "", err
	}
	if token == "" || len(token) > 8192 || strings.ContainsFunc(token, func(c rune) bool { return c <= 32 || c >= 127 }) {
		done()
		return nil, nil, "", ErrAuthentication
	}
	return work, done, token, nil
}

func (r *HTTPRunner) entry(ctx context.Context, key string) (PendingEntry, error) {
	entries, err := r.sender.Entries(ctx)
	if err != nil {
		return PendingEntry{}, err
	}
	for _, e := range entries {
		if e.ClientMessageID == key {
			return e, nil
		}
	}
	return PendingEntry{}, ErrNotFound
}

func (r *HTTPRunner) settle(ctx context.Context, fn func(context.Context) error) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.options.StoreTimeout)
	defer cancel()
	return fn(cleanup)
}

func (r *HTTPRunner) submit(ctx context.Context, key string) (httpAdvice, error) {
	work, done, token, err := r.requestContext(ctx)
	if err != nil {
		return httpAdvice{}, err
	}
	defer done()
	a, err := r.sender.BeginSubmit(work, key)
	if err != nil {
		return httpAdvice{}, err
	}
	unknown := func(cause error, advice httpAdvice) (httpAdvice, error) {
		err := r.settle(ctx, func(c context.Context) error { return r.sender.CompleteUnknown(c, a) })
		return advice, errors.Join(cause, err)
	}
	body, err := json.Marshal(a.Request)
	if err != nil {
		return unknown(ErrProtocol, httpAdvice{})
	}
	if err := work.Err(); err != nil {
		return unknown(err, httpAdvice{})
	}
	req, err := http.NewRequestWithContext(work, http.MethodPost, r.options.Origin+"/auth/sendMessage", bytes.NewReader(body))
	if err != nil {
		return unknown(safeError(ErrHTTPUnavailable, err), httpAdvice{})
	}
	req.GetBody = nil // net/http must never replay this POST.
	req.Header.Set("Content-Type", "application/json")
	status, response, advice, err := r.exchange(req, token)
	if err != nil {
		return unknown(err, advice)
	}
	value, err := messaging.DecodeSubmissionResponse(response)
	if err != nil {
		return unknown(ErrProtocol, advice)
	}
	if value.Code != -1 && value.Code != 0 {
		return unknown(ErrProtocol, advice)
	}
	if status == 401 || status == 400 || status == 403 {
		if !validHTTPFailure(value.Code, value.Data.ReasonCode, status) {
			return unknown(ErrProtocol, advice)
		}
		if status == 401 {
			return unknown(ErrAuthentication, advice)
		}
		return unknown(ErrHTTPRejected, advice)
	}
	if status == 503 && value.Data.State == "" && value.Data.CommandID == "" && value.Data.ClientMessageID == "" && value.Data.PayloadHash == "" && value.Data.PublishAttemptID == "" && validHTTPFailure(value.Code, value.Data.ReasonCode, status) {
		return unknown(ErrHTTPUnavailable, advice)
	}
	c, _ := messaging.Normalize(r.sender.user, a.Request)
	if !validSubmission(value.Data, c) || !validSubmissionHTTP(status, value) {
		return unknown(ErrProtocol, advice)
	}
	err = r.settle(ctx, func(c context.Context) error { return r.sender.CompleteSubmission(c, a, value.Data) })
	if err != nil {
		return advice, err
	}
	if status != 202 {
		return advice, ErrHTTPUnavailable
	}
	return advice, nil
}

func validSubmissionHTTP(status int, r messaging.SubmissionResponse) bool {
	switch status {
	case 202:
		return r.Code == 0 && r.Data.State == "accepted"
	case 429:
		return r.Code == -1 && r.Data.State == "not_published" && r.Data.ReasonCode == "OVERLOADED"
	case 503:
		return r.Code == -1 && ((r.Data.State == "not_published" && r.Data.ReasonCode == "DEPENDENCY_UNAVAILABLE") || r.Data.State == "publish_unknown" || r.Data.State == "unroutable" || r.Data.State == "nacked")
	}
	return false
}

func (r *HTTPRunner) poll(ctx context.Context, key string) (httpAdvice, error) {
	e, err := r.entry(ctx, key)
	if err != nil {
		return httpAdvice{}, err
	}
	if e.State == Persisted || e.State == Rejected {
		return httpAdvice{}, nil
	}
	work, done, token, err := r.requestContext(ctx)
	if err != nil {
		return httpAdvice{}, err
	}
	defer done()
	q, _ := messaging.NormalizeStatusQuery(r.sender.user, key, e.PayloadHash)
	params := url.Values{"clientMessageId": {key}, "payloadHash": {e.PayloadHash}}
	req, err := http.NewRequestWithContext(work, http.MethodGet, r.options.Origin+"/auth/messages/status?"+params.Encode(), nil)
	if err != nil {
		return httpAdvice{}, safeError(ErrHTTPUnavailable, err)
	}
	status, body, advice, err := r.exchange(req, token)
	if err != nil {
		return advice, err
	}
	value, err := messaging.DecodeCommandStatusResponse(body)
	if err != nil {
		return advice, ErrProtocol
	}
	if status == 400 || status == 401 || status == 403 || status == 503 {
		if !validHTTPFailure(value.Code, value.Data.ReasonCode, status) || value.Data.State != "" || value.Data.ClientMessageID != "" || value.Data.PayloadHash != "" || value.Data.Message != nil || value.Data.PollAfterMs != 0 {
			return advice, ErrProtocol
		}
		if status == 401 {
			return advice, ErrAuthentication
		}
		if status != 503 {
			return advice, ErrHTTPRejected
		}
		return advice, ErrHTTPUnavailable
	}
	if !((status == 200 && value.Code == 0 && value.Data.State != messaging.StatusConflict) || (status == 409 && value.Code == -1 && value.Data.State == messaging.StatusConflict)) || value.Data.ValidateFor(q) != nil {
		return advice, ErrProtocol
	}
	err = r.settle(ctx, func(c context.Context) error { return r.sender.ApplyStatus(c, value.Data) })
	if err != nil {
		return advice, err
	}
	advice.pending = value.Data.State == messaging.StatusPendingOrUnknown
	advice.after = max(advice.after, time.Duration(value.Data.PollAfterMs)*time.Millisecond)
	return advice, nil
}

func validHTTPFailure(code int, reason string, status int) bool {
	if code != -1 {
		return false
	}
	switch status {
	case 400:
		return reason == "INVALID_REQUEST"
	case 401:
		return reason == "UNAUTHENTICATED"
	case 403:
		return reason == "FORBIDDEN"
	case 503:
		return reason == "DEPENDENCY_UNAVAILABLE"
	}
	return false
}

func (r *HTTPRunner) exchange(req *http.Request, token string) (int, []byte, httpAdvice, error) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, nil, httpAdvice{}, safeError(ErrHTTPUnavailable, err)
	}
	advice := httpAdvice{after: retryAfter(resp.Header.Get("Retry-After"), time.Now())}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, messaging.MaxBodyBytes+1))
	closeErr := resp.Body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return resp.StatusCode, nil, advice, safeError(ErrHTTPUnavailable, err)
	}
	media, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || media != "application/json" || (params["charset"] != "" && !strings.EqualFold(params["charset"], "utf-8")) || resp.Header.Get("Content-Encoding") != "" || len(body) > messaging.MaxBodyBytes || !hasNoStore(resp.Header.Get("Cache-Control")) {
		return resp.StatusCode, nil, advice, ErrProtocol
	}
	return resp.StatusCode, body, advice, nil
}

func hasNoStore(value string) bool {
	for _, part := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(part), "no-store") {
			return true
		}
	}
	return false
}

func retryAfter(value string, now time.Time) time.Duration {
	if n, err := strconv.ParseUint(value, 10, 32); err == nil {
		return time.Duration(min(n, 60)) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil {
		return min(max(date.Sub(now), 0), time.Minute)
	}
	return 0
}
