package clientsim

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"iChat/messaging"
)

func fastRecovery() RecoveryOptions {
	return RecoveryOptions{MaxRounds: 8, MaxSubmissions: 2, BaseBackoff: time.Millisecond, MaxBackoff: 4 * time.Millisecond}
}

func TestHTTPRecoveryDiskRestartAndOriginalRequest(t *testing.T) {
	dir := t.TempDir()
	store := diskStore(t, dir)
	sender := diskSender(t, store)
	entry := simQueue(t, sender, 1)
	accepted, pending, terminal := acceptedReply(t), statusReply(t, messaging.StatusPendingOrUnknown), statusReply(t, messaging.StatusPersisted)
	var mu sync.Mutex
	var bodies [][]byte
	var sequence []string
	var polls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if req.Method == "POST" {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Error(err)
				return
			}
			bodies = append(bodies, body)
			sequence = append(sequence, "submit")
			if len(bodies) == 1 {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				conn.Close()
				return
			}
			simReply(w, 202, accepted)
			return
		}
		sequence = append(sequence, "poll")
		polls++
		if req.URL.Query().Get("payloadHash") != entry.PayloadHash {
			t.Error("query changed local hash")
		}
		if polls == 1 {
			simReply(w, 200, pending)
		} else {
			simReply(w, 200, terminal)
		}
	}))
	defer server.Close()
	r := simHTTPRunner(t, sender, server.URL)
	if err := r.Submit(context.Background(), entry.ClientMessageID); err == nil || simEntry(t, sender).State != PublishUnknown {
		t.Fatal("lost response not retained", err)
	}
	r.Close()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopenedStore := diskStore(t, dir)
	reopened := diskSender(t, reopenedStore)
	r = simHTTPRunner(t, reopened, server.URL)
	var waits []time.Duration
	r.wait = func(ctx context.Context, d time.Duration) error { waits = append(waits, d); return ctx.Err() }
	o := fastRecovery()
	o.MaxSubmissions = 1
	if err := r.Recover(context.Background(), entry.ClientMessageID, o); err != nil {
		t.Fatal(err)
	}
	got := simEntry(t, reopened)
	if got.State != Persisted || got.Attempts != 2 || got.Request != entry.Request || !reflect.DeepEqual(got.Result, &terminal.Data) {
		t.Fatal("HTTP recovery lost durable state")
	}
	mu.Lock()
	if len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) || !reflect.DeepEqual(sequence, []string{"submit", "poll", "submit", "poll"}) {
		t.Error("HTTP recovery changed request or order", sequence)
	}
	mu.Unlock()
	if !reflect.DeepEqual(waits, []time.Duration{2 * time.Second, 2 * time.Millisecond}) {
		t.Fatal("server poll floor/backoff ignored", waits)
	}
	r.Close()
	if err := reopenedStore.Close(); err != nil {
		t.Fatal(err)
	}
	final := diskSender(t, diskStore(t, dir))
	if got := simEntry(t, final); got.State != Persisted || got.Attempts != 2 || !reflect.DeepEqual(got.Result, &terminal.Data) {
		t.Fatal("HTTP terminal did not survive disk reopen")
	}
}

func TestHTTPRecoveryBudgetsAndEvidenceGates(t *testing.T) {
	for _, mode := range []string{"pending", "SQL_error", "malformed", "quarantine", "conflict", "rejected", "auth", "overloaded"} {
		t.Run(mode, func(t *testing.T) {
			accepted := acceptedReply(t)
			pending := statusReply(t, messaging.StatusPendingOrUnknown)
			state := messaging.StatusQuarantined
			if mode == "conflict" {
				state = messaging.StatusConflict
			}
			if mode == "rejected" {
				state = messaging.StatusRejected
			}
			status := statusReply(t, state)
			var posts, queries atomic.Int32
			r, s, _, e := simHTTPFixture(t, func(w http.ResponseWriter, req *http.Request) {
				if req.Method == "POST" {
					posts.Add(1)
					if mode == "overloaded" {
						reply := accepted
						reply.Code = -1
						reply.Data.State = "not_published"
						reply.Data.ReasonCode = "OVERLOADED"
						w.Header().Set("Retry-After", "3")
						simReply(w, 429, reply)
					} else {
						simReply(w, 202, accepted)
					}
					return
				}
				queries.Add(1)
				switch mode {
				case "pending", "overloaded":
					simReply(w, 200, pending)
				case "SQL_error":
					simReply(w, 503, map[string]any{"code": -1, "msg": "unavailable", "data": map[string]string{"reasonCode": "DEPENDENCY_UNAVAILABLE"}})
				case "malformed":
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Cache-Control", "no-store")
					io.WriteString(w, `{"code":0,"msg":"bad","data":{"state":"pending_or_unknown"}}`)
				case "auth":
					simReply(w, 401, map[string]any{"code": -1, "msg": "auth", "data": map[string]string{"reasonCode": "UNAUTHENTICATED"}})
				default:
					code := 200
					if mode == "conflict" {
						code = 409
					}
					simReply(w, code, status)
				}
			})
			if mode != "overloaded" {
				a := simBegin(t, s, e.ClientMessageID)
				if err := s.CompleteUnknown(context.Background(), a); err != nil {
					t.Fatal(err)
				}
			}
			var waits []time.Duration
			r.wait = func(ctx context.Context, d time.Duration) error { waits = append(waits, d); return ctx.Err() }
			o := fastRecovery()
			o.MaxRounds = 5
			o.MaxSubmissions = 1
			err := r.Recover(context.Background(), e.ClientMessageID, o)
			switch mode {
			case "rejected":
				if err != nil || posts.Load() != 0 || queries.Load() != 1 || simEntry(t, s).State != Rejected {
					t.Fatal(err)
				}
			case "conflict":
				if !errors.Is(err, ErrBlocked) || posts.Load() != 0 || queries.Load() != 1 || simEntry(t, s).State != Conflict {
					t.Fatal(err)
				}
			case "auth":
				if !errors.Is(err, ErrAuthentication) || posts.Load() != 0 || queries.Load() != 1 {
					t.Fatal(err)
				}
			default:
				wantPosts := int32(0)
				if mode == "pending" || mode == "overloaded" {
					wantPosts = 1
				}
				if !errors.Is(err, ErrRecoveryExhausted) || posts.Load() != wantPosts || int(posts.Load()+queries.Load()) != o.MaxRounds {
					t.Fatal("unbounded recovery or unauthorized repost", mode, err, posts.Load(), queries.Load())
				}
				if mode == "SQL_error" || mode == "malformed" {
					if simEntry(t, s).State != PublishUnknown {
						t.Fatal("query failure fabricated pending")
					}
				}
				if mode == "quarantine" && simEntry(t, s).State != Quarantined {
					t.Fatal("quarantine lost")
				}
				if mode == "overloaded" && waits[0] != 3*time.Second {
					t.Fatal("Retry-After ignored", waits)
				}
			}
		})
	}
}

func TestHTTPRecoveryBackoffCancellationAndClose(t *testing.T) {
	for _, mode := range []string{"cancel", "close"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			r, s, _, e := simHTTPFixture(t, func(w http.ResponseWriter, req *http.Request) {
				calls.Add(1)
				simReply(w, 503, map[string]any{"code": -1, "msg": "unavailable", "data": map[string]string{"reasonCode": "DEPENDENCY_UNAVAILABLE"}})
			})
			a := simBegin(t, s, e.ClientMessageID)
			if err := s.CompleteUnknown(context.Background(), a); err != nil {
				t.Fatal(err)
			}
			entered := make(chan struct{})
			r.wait = func(c context.Context, d time.Duration) error { close(entered); return waitRecovery(c, d) }
			o := fastRecovery()
			o.BaseBackoff = time.Minute
			o.MaxBackoff = time.Minute
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- r.Recover(ctx, e.ClientMessageID, o) }()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("backoff not entered")
			}
			if mode == "close" {
				r.Close()
			} else {
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) || calls.Load() != 1 || simEntry(t, s).InFlight {
					t.Fatal("backoff ignored cancellation", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("backoff did not stop")
			}
		})
	}
}

func TestHTTPRecoveryPolicyAndRetryAfterBounds(t *testing.T) {
	r, _, _, e := simHTTPFixture(t, func(w http.ResponseWriter, req *http.Request) { t.Error("invalid policy called HTTP") })
	for _, mode := range []string{"rounds_zero", "rounds_large", "submissions_negative", "submissions_large", "base_zero", "backoff_order", "backoff_large"} {
		o := fastRecovery()
		switch mode {
		case "rounds_zero":
			o.MaxRounds = 0
		case "rounds_large":
			o.MaxRounds = 101
		case "submissions_negative":
			o.MaxSubmissions = -1
		case "submissions_large":
			o.MaxSubmissions = o.MaxRounds + 1
		case "base_zero":
			o.BaseBackoff = 0
		case "backoff_order":
			o.MaxBackoff = o.BaseBackoff - 1
		case "backoff_large":
			o.MaxBackoff = time.Minute + 1
		}
		if err := r.Recover(context.Background(), e.ClientMessageID, o); !errors.Is(err, ErrHTTPOptions) {
			t.Fatal(mode, err)
		}
	}
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{{"3", 3 * time.Second}, {"9999", time.Minute}, {"invalid", 0}, {"-1", 0}, {now.Add(4 * time.Second).Format(http.TimeFormat), 4 * time.Second}, {now.Add(-time.Minute).Format(http.TimeFormat), 0}, {now.Add(time.Hour).Format(http.TimeFormat), time.Minute}} {
		if got := retryAfter(tc.value, now); got != tc.want {
			t.Fatal(tc.value, got)
		}
	}
}
