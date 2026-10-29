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

type apiConversationsReaderFunc func(context.Context, uint64, string, int) (messaging.ConversationPage, error)

func (f apiConversationsReaderFunc) ListConversations(c context.Context, u uint64, k string, n int) (messaging.ConversationPage, error) {
	return f(c, u, k, n)
}
func apiFixtureConversations(apiSQL) (service.ConversationReader, error) {
	return apiConversationsReaderFunc(func(context.Context, uint64, string, int) (messaging.ConversationPage, error) {
		return messaging.ConversationPage{}, errors.New("unexpected fixture conversations read")
	}), nil
}

func apiConversationsFetch(s *apiService, token string) (int, string, error) {
	r, _ := http.NewRequest("GET", "http://"+apiAddress(s)+"/auth/conversations?limit=1", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	response, e := (&http.Client{Timeout: time.Second}).Do(r)
	if e != nil {
		return 0, "", e
	}
	defer response.Body.Close()
	b, e := io.ReadAll(response.Body)
	if e != nil {
		return 0, "", e
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		return 0, "", errors.New("conversations cached")
	}
	return response.StatusCode, string(b), nil
}
func TestAPIConversationsPendingOwnershipAndLateResult(t *testing.T) {
	h := newAPIHarness()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var admitted context.Context
	h.d.conversations = func(apiSQL) (service.ConversationReader, error) {
		return apiConversationsReaderFunc(func(c context.Context, u uint64, k string, n int) (messaging.ConversationPage, error) {
			admitted = c
			close(entered)
			<-release
			return messaging.ConversationPage{Items: []messaging.ConversationSummary{}, NextCursor: k}, nil
		}), nil
	}
	s := h.service(t)
	if e := s.Start(context.Background(), context.Background()); e != nil {
		t.Fatal(e)
	}
	token := apiToken(t, h)
	type response struct {
		code int
		err  error
	}
	result := make(chan response, 1)
	go func() { code, _, e := apiConversationsFetch(s, token); result <- response{code, e} }()
	apiWait(t, entered)
	observer, stop := context.WithCancel(context.Background())
	stop()
	if e := s.Close(observer); !errors.Is(e, errAPIPending) {
		t.Fatal("conversations owner lost", e)
	}
	if h.sql.closed.Load() != 0 || h.redis.closed.Load() != 0 {
		t.Fatal("clients released before reader joined")
	}
	apiWait(t, admitted.Done())
	unblock()
	if got := <-result; got.err == nil && got.code != 503 {
		t.Fatal("canceled reader returned a successful page", got.code)
	}
	apiWait(t, s.Done())
	if h.sql.closed.Load() != 1 || h.redis.closed.Load() != 1 {
		t.Fatal("clients not released after join")
	}
}
func TestAPIConversationsPanicAdmissionAndProbeIndependence(t *testing.T) {
	for _, mode := range []string{"panic", "stopped", "unready_probe"} {
		t.Run(mode, func(t *testing.T) {
			h := newAPIHarness()
			h.sql.offline.Store(true)
			h.d.conversations = func(apiSQL) (service.ConversationReader, error) {
				return apiConversationsReaderFunc(func(context.Context, uint64, string, int) (messaging.ConversationPage, error) {
					if mode != "unready_probe" {
						panic("PRIVATE_READER_BODY")
					}
					return messaging.ConversationPage{Items: []messaging.ConversationSummary{}}, nil
				}), nil
			}
			s := h.service(t)
			if e := s.Start(context.Background(), context.Background()); e != nil {
				t.Fatal(e)
			}
			if mode == "stopped" {
				s.StopAdmission()
			}
			code, body, e := apiConversationsFetch(s, apiToken(t, h))
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
