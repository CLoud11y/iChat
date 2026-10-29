package clientsim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"iChat/messaging"
)

const fixtureBearer = "PRIVATE_SIM_TOKEN"

func simReply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}
func simHTTPRunner(t *testing.T, s *Sender, origin string) *HTTPRunner {
	t.Helper()
	options := DefaultHTTPOptions(origin, func(context.Context) (string, error) { return fixtureBearer, nil })
	r, err := NewHTTPRunner(s, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r
}
func simHTTPFixture(t *testing.T, handler http.HandlerFunc) (*HTTPRunner, *Sender, *checkpointStore, PendingEntry) {
	t.Helper()
	s, store := simSender(t)
	e := simQueue(t, s, 1)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return simHTTPRunner(t, s, server.URL), s, store, e
}
func acceptedReply(t *testing.T) messaging.SubmissionResponse {
	t.Helper()
	return messaging.SubmissionResponse{Code: 0, Msg: "accepted", Data: simSubmission(t, simRequest(1), "accepted")}
}
func statusReply(t *testing.T, state messaging.StatusState) messaging.CommandStatusResponse {
	t.Helper()
	code := 0
	if state == messaging.StatusConflict {
		code = -1
	}
	return messaging.CommandStatusResponse{Code: code, Msg: "command status", Data: simStatus(t, simRequest(1), state)}
}

func TestHTTPRunnerSubmissionAndStatusContracts(t *testing.T) {
	accepted := acceptedReply(t)
	var calls atomic.Int32
	r, s, _, e := simHTTPFixture(t, func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		if req.Method != "POST" || req.URL.Path != "/auth/sendMessage" || req.URL.RawQuery != "" || req.Header.Get("Authorization") != "Bearer "+fixtureBearer || req.Header.Get("Idempotency-Key") != "" || req.Header.Get("X-Idempotency-Key") != "" {
			t.Error("wrong submission route/headers")
		}
		body, err := io.ReadAll(req.Body)
		decoded, decodeErr := messaging.DecodeRequest(body)
		if err != nil || decodeErr != nil || decoded != simRequest(1) {
			t.Error("submission changed original request")
		}
		simReply(w, 202, accepted)
	})
	if err := r.Submit(context.Background(), e.ClientMessageID); err != nil || calls.Load() != 1 || simEntry(t, s).State != Accepted || simEntry(t, s).InFlight {
		t.Fatal(err)
	}
	for _, state := range []messaging.StatusState{messaging.StatusPersisted, messaging.StatusRejected, messaging.StatusPendingOrUnknown, messaging.StatusQuarantined, messaging.StatusConflict} {
		t.Run(string(state), func(t *testing.T) {
			reply := statusReply(t, state)
			code := 200
			if state == messaging.StatusConflict {
				code = 409
			}
			if state == messaging.StatusPersisted {
				reply.Data.Message.Seq = ^messaging.Sequence(0)
			}
			var seen atomic.Int32
			r, s, _, e := simHTTPFixture(t, func(w http.ResponseWriter, req *http.Request) {
				seen.Add(1)
				q := req.URL.Query()
				if req.Method != "GET" || req.URL.Path != "/auth/messages/status" || len(q) != 2 || q.Get("clientMessageId") != reply.Data.ClientMessageID || q.Get("payloadHash") != reply.Data.PayloadHash || req.Header.Get("Authorization") != "Bearer "+fixtureBearer || req.ContentLength != 0 {
					t.Error("wrong status request")
				}
				simReply(w, code, reply)
			})
			if err := r.Poll(context.Background(), e.ClientMessageID); err != nil || seen.Load() != 1 {
				t.Fatal(err)
			}
			got := simEntry(t, s)
			want := SendState(state)
			if state == messaging.StatusPendingOrUnknown {
				want = PublishUnknown
			}
			if got.State != want || !reflect.DeepEqual(got.Result, &reply.Data) {
				t.Fatal("wrong status checkpoint")
			}
		})
	}
}

func TestHTTPRunnerRejectsMalformedWireEvidence(t *testing.T) {
	accepted := acceptedReply(t)
	base, _ := json.Marshal(accepted)
	for _, mode := range []string{"legacy_200", "wrong_code", "missing_code", "duplicate_code", "case_alias", "duplicate_state", "escaped_duplicate", "null", "unknown", "wrong_identity", "missing_attempt", "unicode", "utf8", "trailing", "oversize", "media", "encoding", "cache"} {
		t.Run(mode, func(t *testing.T) {
			body := string(base)
			code := 202
			media, encoding, cache := "application/json", "", "no-store"
			switch mode {
			case "legacy_200":
				code = 200
			case "wrong_code":
				body = strings.Replace(body, `"code":0`, `"code":-1`, 1)
			case "missing_code":
				body = strings.Replace(body, `"code":0,`, "", 1)
			case "duplicate_code":
				body = strings.Replace(body, `"code":0`, `"code":0,"code":0`, 1)
			case "case_alias":
				body = strings.Replace(body, `"code":0`, `"Code":0`, 1)
			case "duplicate_state":
				body = strings.Replace(body, `"state":"accepted"`, `"state":"accepted","state":"accepted"`, 1)
			case "escaped_duplicate":
				body = strings.Replace(body, `"state":"accepted"`, `"state":"accepted","\u0073tate":"accepted"`, 1)
			case "null":
				body = strings.Replace(body, `"state":"accepted"`, `"state":null`, 1)
			case "unknown":
				body = strings.Replace(body, `"state":"accepted"`, `"state":"accepted","seq":"1"`, 1)
			case "wrong_identity":
				body = strings.Replace(body, accepted.Data.PayloadHash, strings.Repeat("f", 64), 1)
			case "missing_attempt":
				body = strings.Replace(body, accepted.Data.PublishAttemptID, "", 1)
			case "unicode":
				body = strings.Replace(body, `"msg":"accepted"`, `"msg":"\ud800"`, 1)
			case "utf8":
				body = string(append([]byte(body), 0xff))
			case "trailing":
				body += "{}"
			case "oversize":
				body += strings.Repeat(" ", messaging.MaxBodyBytes)
			case "media":
				media = "text/html"
			case "encoding":
				encoding = "gzip"
			case "cache":
				cache = "x-no-store"
			}
			r, s, _, e := simHTTPFixture(t, func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", media)
				w.Header().Set("Content-Encoding", encoding)
				w.Header().Set("Cache-Control", cache)
				w.WriteHeader(code)
				io.WriteString(w, body)
			})
			if err := r.Submit(context.Background(), e.ClientMessageID); !errors.Is(err, ErrProtocol) {
				t.Fatal("bad submission evidence accepted", mode, err)
			}
			if got := simEntry(t, s); got.State != PublishUnknown || got.InFlight || got.Request != e.Request {
				t.Fatal("unusable response lost original request")
			}
		})
	}
	for _, mode := range []string{"duplicate_message", "message_alias", "message_null", "number_seq", "wrong_hash", "bad_poll", "wrong_http", "missing_msg", "error_pending"} {
		t.Run("status_"+mode, func(t *testing.T) {
			value := statusReply(t, messaging.StatusPersisted)
			raw, _ := json.Marshal(value)
			body := string(raw)
			code := 200
			switch mode {
			case "duplicate_message":
				body = strings.Replace(body, `"seq":"1"`, `"seq":"1","seq":"1"`, 1)
			case "message_alias":
				body = strings.Replace(body, `"seq":"1"`, `"Seq":"1"`, 1)
			case "message_null":
				body = strings.Replace(body, `"seq":"1"`, `"seq":null`, 1)
			case "number_seq":
				body = strings.Replace(body, `"seq":"1"`, `"seq":1`, 1)
			case "wrong_hash":
				body = strings.Replace(body, value.Data.PayloadHash, strings.Repeat("f", 64), 1)
			case "bad_poll":
				body = strings.Replace(body, `"state":"persisted"`, `"state":"persisted","pollAfterMs":2000`, 1)
			case "wrong_http":
				code = 409
			case "missing_msg":
				body = strings.Replace(body, `"msg":"command status",`, "", 1)
			case "error_pending":
				code = 503
				value = statusReply(t, messaging.StatusPendingOrUnknown)
				raw, _ = json.Marshal(value)
				body = string(raw)
			}
			r, s, store, e := simHTTPFixture(t, func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				w.WriteHeader(code)
				io.WriteString(w, body)
			})
			before := store.counts()
			if err := r.Poll(context.Background(), e.ClientMessageID); !errors.Is(err, ErrProtocol) || store.counts() != before || simEntry(t, s).State != Queued {
				t.Fatal("bad query changed checkpoint", err)
			}
		})
	}
}

func TestHTTPRunnerErrorsRedirectDeadlineAndLostResponse(t *testing.T) {
	for _, tc := range []struct {
		code   int
		reason string
		want   error
	}{{400, "INVALID_REQUEST", ErrHTTPRejected}, {401, "UNAUTHENTICATED", ErrAuthentication}, {403, "FORBIDDEN", ErrHTTPRejected}, {503, "DEPENDENCY_UNAVAILABLE", ErrHTTPUnavailable}} {
		for _, method := range []string{"submit", "poll"} {
			r, s, _, e := simHTTPFixture(t, func(w http.ResponseWriter, req *http.Request) {
				simReply(w, tc.code, map[string]any{"code": -1, "msg": "unavailable", "data": map[string]string{"reasonCode": tc.reason}})
			})
			var err error
			if method == "submit" {
				err = r.Submit(context.Background(), e.ClientMessageID)
			} else {
				err = r.Poll(context.Background(), e.ClientMessageID)
			}
			want := Queued
			if method == "submit" {
				want = PublishUnknown
			}
			if !errors.Is(err, tc.want) || simEntry(t, s).State != want || simEntry(t, s).InFlight {
				t.Fatal(method, tc.code, err)
			}
		}
	}
	var redirects atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { redirects.Add(1) }))
	defer destination.Close()
	r, s, _, e := simHTTPFixture(t, func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Location", destination.URL+"/leak")
		simReply(w, 307, map[string]any{"code": -1, "msg": "redirect", "data": map[string]string{}})
	})
	if err := r.Submit(context.Background(), e.ClientMessageID); err == nil || redirects.Load() != 0 || simEntry(t, s).State != PublishUnknown {
		t.Fatal("redirect followed or accepted", err)
	}
	for _, mode := range []string{"lost", "body_deadline", "caller_cancel"} {
		t.Run(mode, func(t *testing.T) {
			entered := make(chan struct{})
			var posts atomic.Int32
			r, s, _, e := simHTTPFixture(t, func(w http.ResponseWriter, req *http.Request) {
				posts.Add(1)
				io.Copy(io.Discard, req.Body)
				close(entered)
				if mode == "lost" {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					conn.Close()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				w.WriteHeader(202)
				io.WriteString(w, `{"code":`)
				w.(http.Flusher).Flush()
				<-req.Context().Done()
			})
			r.options.RequestTimeout = 30 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- r.Submit(ctx, e.ClientMessageID) }()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("request not started")
			}
			if mode == "caller_cancel" {
				cancel()
			}
			select {
			case err := <-done:
				if err == nil || posts.Load() != 1 || simEntry(t, s).State != PublishUnknown || simEntry(t, s).InFlight || strings.Contains(fmt.Sprintf("%+v %#v", err, err), fixtureBearer) {
					t.Fatal("lost response mishandled", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("request did not honor deadline")
			}
		})
	}
}

func TestHTTPRunnerConcurrencyCloseAndTokenOwnership(t *testing.T) {
	s, _ := simSender(t)
	for i := 1; i <= 3; i++ {
		simQueue(t, s, i)
	}
	entered := make(chan struct{}, 3)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var active, peak atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		n := active.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		defer active.Add(-1)
		entered <- struct{}{}
		<-release
		body, _ := io.ReadAll(req.Body)
		request, err := messaging.DecodeRequest(body)
		if err != nil {
			t.Error(err)
			return
		}
		c, err := messaging.Normalize(12, request)
		if err != nil {
			t.Error(err)
			return
		}
		simReply(w, 202, messaging.SubmissionResponse{Msg: "accepted", Data: messaging.Submission{State: "accepted", ClientMessageID: c.ClientMessageID, CommandID: c.CommandID, PayloadHash: c.PayloadHash, PublishAttemptID: "11111111-1234-4234-8234-123456789012"}})
	}))
	defer func() { unblock(); server.Close() }()
	r := simHTTPRunner(t, s, server.URL)
	entries, err := s.Entries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 3)
	for _, e := range entries {
		go func(key string) { done <- r.Submit(context.Background(), key) }(e.ClientMessageID)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("concurrency slots not filled")
		}
	}
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		r.mu.Lock()
		count := len(r.keys)
		r.mu.Unlock()
		if count == 3 {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("third key not admitted to slot wait")
		case <-tick.C:
		}
	}
	if err := r.Poll(context.Background(), entries[0].ClientMessageID); !errors.Is(err, ErrInFlight) {
		t.Fatal("duplicate key task admitted", err)
	}
	unblock()
	for i := 0; i < 3; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if peak.Load() > 2 {
		t.Fatal("HTTP concurrency unbounded", peak.Load())
	}

	// A callback that ignores cancellation remains owned until real return.
	blocked := make(chan struct{})
	resume := make(chan struct{})
	var resumeOnce sync.Once
	defer resumeOnce.Do(func() { close(resume) })
	r.options.Token = func(context.Context) (string, error) { close(blocked); <-resume; return fixtureBearer, nil }
	op := make(chan error, 1)
	go func() { op <- r.Submit(context.Background(), entries[0].ClientMessageID) }()
	<-blocked
	closed := make(chan struct{})
	go func() { r.Close(); close(closed) }()
	select {
	case <-r.work.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not cancel runner")
	}
	select {
	case <-closed:
		t.Fatal("Close abandoned token callback")
	default:
	}
	resumeOnce.Do(func() { close(resume) })
	if err := <-op; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not join callback")
	}
	if err := r.Poll(context.Background(), entries[0].ClientMessageID); !errors.Is(err, ErrHTTPClosed) {
		t.Fatal(err)
	}
}

func TestHTTPRunnerValidationAndTokenFailures(t *testing.T) {
	s, _ := simSender(t)
	e := simQueue(t, s, 1)
	for _, mode := range []string{"nil_sender", "nil_token", "zero_timeout", "large_timeout", "zero_store_timeout", "large_store_timeout", "zero_concurrency", "large_concurrency"} {
		o := DefaultHTTPOptions("http://127.0.0.1:1", func(context.Context) (string, error) { t.Error("constructor called token provider"); return "", nil })
		owner := s
		switch mode {
		case "nil_sender":
			owner = nil
		case "nil_token":
			o.Token = nil
		case "zero_timeout":
			o.RequestTimeout = 0
		case "large_timeout":
			o.RequestTimeout = time.Minute + 1
		case "zero_store_timeout":
			o.StoreTimeout = 0
		case "large_store_timeout":
			o.StoreTimeout = time.Minute + 1
		case "zero_concurrency":
			o.MaxConcurrent = 0
		case "large_concurrency":
			o.MaxConcurrent = 9
		}
		if r, err := NewHTTPRunner(owner, o); r != nil || !errors.Is(err, ErrHTTPOptions) {
			t.Fatal(mode, err)
		}
	}
	for _, origin := range []string{"", "file:///tmp/x", "http://user:secret@localhost", "http://localhost/path", "http://localhost?x=1", "http://localhost#x", "http://:80"} {
		if r, err := NewHTTPRunner(s, DefaultHTTPOptions(origin, func(context.Context) (string, error) { return "token", nil })); r != nil || !errors.Is(err, ErrHTTPOptions) {
			t.Fatal(origin, err)
		}
	}
	for _, mode := range []string{"empty", "newline", "too_long", "callback_error", "late"} {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { calls.Add(1) }))
		o := DefaultHTTPOptions(server.URL, func(c context.Context) (string, error) {
			switch mode {
			case "empty":
				return "", nil
			case "newline":
				return "a\nb", nil
			case "too_long":
				return strings.Repeat("x", 8193), nil
			case "callback_error":
				return "", errors.New("PRIVATE_TOKEN_ERROR")
			case "late":
				<-c.Done()
				return fixtureBearer, nil
			}
			return "", nil
		})
		o.RequestTimeout = 5 * time.Millisecond
		r, err := NewHTTPRunner(s, o)
		if err != nil {
			t.Fatal(err)
		}
		err = r.Submit(context.Background(), e.ClientMessageID)
		r.Close()
		server.Close()
		if err == nil || calls.Load() != 0 || simEntry(t, s).Attempts != 0 || strings.Contains(fmt.Sprintf("%+v %#v", err, err), "PRIVATE") {
			t.Fatal("invalid token reached transport", mode, err)
		}
	}
}

func TestHTTPRunnerStoreSettlementRemainsOwned(t *testing.T) {
	accepted := acceptedReply(t)
	r, s, store, e := simHTTPFixture(t, func(w http.ResponseWriter, req *http.Request) { simReply(w, 202, accepted) })
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(resume) }) }
	defer unblock()
	store.onSave = func(context.Context) {
		body, _ := store.Load(context.Background())
		entries, err := decodeSnapshot(12, body)
		if err != nil {
			return
		}
		if entries[e.ClientMessageID].State == Accepted {
			close(entered)
			<-resume
		}
	}
	done := make(chan error, 1)
	go func() { done <- r.Submit(context.Background(), e.ClientMessageID) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("settlement not entered")
	}
	closed := make(chan struct{})
	go func() { r.Close(); close(closed) }()
	select {
	case <-r.work.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not cancel runner")
	}
	select {
	case <-closed:
		t.Fatal("Close abandoned storage callback")
	default:
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("settlement not joined")
	}
	if simEntry(t, s).State != Accepted || simEntry(t, s).InFlight {
		t.Fatal("known response lost during Close")
	}
}

func TestHTTPRunnerTypedErrorResponses(t *testing.T) {
	for _, state := range []string{"not_published", "unroutable", "nacked", "publish_unknown"} {
		reply := simSubmission(t, simRequest(1), state)
		r, s, _, e := simHTTPFixture(t, func(w http.ResponseWriter, req *http.Request) {
			simReply(w, 503, messaging.SubmissionResponse{Code: -1, Msg: "unavailable", Data: reply})
		})
		if err := r.Submit(context.Background(), e.ClientMessageID); !errors.Is(err, ErrHTTPUnavailable) {
			t.Fatal(err)
		}
		want := NotPublished
		if state == "publish_unknown" {
			want = PublishUnknown
		}
		if simEntry(t, s).State != want || simEntry(t, s).InFlight {
			t.Fatal("valid failure evidence lost", state)
		}
	}
}
