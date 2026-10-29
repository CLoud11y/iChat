package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"iChat/messaging"
	"iChat/service"
)

type apiSyncReaderFunc func(context.Context, uint64, string, string, int) (messaging.SyncPage, error)

func (f apiSyncReaderFunc) SyncMessages(c context.Context, u uint64, id, after string, n int) (messaging.SyncPage, error) {
	return f(c, u, id, after, n)
}
func apiFixtureSync(apiSQL) (service.MessageSyncReader, error) {
	return apiSyncReaderFunc(func(context.Context, uint64, string, string, int) (messaging.SyncPage, error) {
		return messaging.SyncPage{}, errors.New("unexpected fixture sync read")
	}), nil
}
func apiSyncFetch(s *apiService, token string) (int, string, error) {
	r, _ := http.NewRequest("GET", "http://"+apiAddress(s)+"/auth/messages/sync?conversationId=p:42:43", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	resp, e := (&http.Client{Timeout: time.Second}).Do(r)
	if e != nil {
		return 0, "", e
	}
	defer resp.Body.Close()
	b, e := io.ReadAll(resp.Body)
	if e != nil {
		return 0, "", e
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		return 0, "", errors.New("sync response cached")
	}
	return resp.StatusCode, string(b), nil
}
func TestAPISyncPendingOwnershipAndLateResult(t *testing.T) {
	h := newAPIHarness()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var admitted context.Context
	h.d.syncMessages = func(apiSQL) (service.MessageSyncReader, error) {
		return apiSyncReaderFunc(func(c context.Context, _ uint64, id, _ string, _ int) (messaging.SyncPage, error) {
			admitted = c
			close(entered)
			<-release
			return messaging.SyncPage{ConversationID: id, Items: []messaging.MessageDTO{}}, nil
		}), nil
	}
	s := h.service(t)
	if e := s.Start(context.Background(), context.Background()); e != nil {
		t.Fatal(e)
	}
	token := apiToken(t, h)
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() { code, _, e := apiSyncFetch(s, token); done <- result{code, e} }()
	apiWait(t, entered)
	observer, stop := context.WithCancel(context.Background())
	stop()
	if e := s.Close(observer); !errors.Is(e, errAPIPending) {
		t.Fatal("sync owner lost", e)
	}
	if h.sql.closed.Load() != 0 || h.redis.closed.Load() != 0 {
		t.Fatal("clients closed before callback join")
	}
	apiWait(t, admitted.Done())
	unblock()
	if r := <-done; r.err == nil && r.code != 503 {
		t.Fatal("late result succeeded", r.code)
	}
	apiWait(t, s.Done())
	if h.sql.closed.Load() != 1 || h.redis.closed.Load() != 1 {
		t.Fatal("clients not released after callback joined")
	}
}
func TestAPISyncPanicAdmissionAndProbeIndependence(t *testing.T) {
	for _, mode := range []string{"panic", "stopped", "unready_probe"} {
		t.Run(mode, func(t *testing.T) {
			h := newAPIHarness()
			h.sql.offline.Store(true)
			h.d.syncMessages = func(apiSQL) (service.MessageSyncReader, error) {
				return apiSyncReaderFunc(func(context.Context, uint64, string, string, int) (messaging.SyncPage, error) {
					if mode != "unready_probe" {
						panic("PRIVATE_SYNC_PANIC")
					}
					return messaging.SyncPage{ConversationID: "p:42:43", Items: []messaging.MessageDTO{}}, nil
				}), nil
			}
			s := h.service(t)
			if e := s.Start(context.Background(), context.Background()); e != nil {
				t.Fatal(e)
			}
			if mode == "stopped" {
				s.StopAdmission()
			}
			code, body, e := apiSyncFetch(s, apiToken(t, h))
			if e != nil {
				t.Fatal(e)
			}
			if mode == "unready_probe" {
				if code != 200 || !strings.Contains(body, `"items":[]`) || h.publisher.calls.Load() != 0 {
					t.Fatal(code, body)
				}
			} else if code != 503 || !strings.Contains(body, "DEPENDENCY_UNAVAILABLE") || strings.Contains(body, "PRIVATE") || strings.Contains(body, `"state"`) {
				t.Fatal(code, body)
			}
		})
	}
}
