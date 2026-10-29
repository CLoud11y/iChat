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

type apiStatusReaderFunc func(context.Context, uint64, string, string) (messaging.CommandStatus, error)

func (f apiStatusReaderFunc) GetCommandStatus(c context.Context, u uint64, k, h string) (messaging.CommandStatus, error) {
	return f(c, u, k, h)
}

func apiFixtureStatus(apiSQL) (service.CommandStatusReader, error) {
	return apiStatusReaderFunc(func(context.Context, uint64, string, string) (messaging.CommandStatus, error) {
		return messaging.CommandStatus{}, errors.New("unexpected fixture status read")
	}), nil
}

func apiStatusRequest(t *testing.T, s *apiService, token string) (int, string) {
	t.Helper()
	code, body, err := apiStatusFetch(s, token)
	if err != nil {
		t.Fatal(err)
	}
	return code, body
}

func apiStatusFetch(s *apiService, token string) (int, string, error) {
	r, _ := http.NewRequest("GET", "http://"+apiAddress(s)+"/auth/messages/status?clientMessageId=8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1&payloadHash="+strings.Repeat("a", 64), nil)
	r.Header.Set("Authorization", "Bearer "+token)
	response, err := (&http.Client{Timeout: time.Second}).Do(r)
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return 0, "", err
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		return 0, "", errors.New("status cached")
	}
	return response.StatusCode, string(body), nil
}

func TestAPIStatusPendingOwnershipAndLateResult(t *testing.T) {
	h := newAPIHarness()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var admitted context.Context // Published by closing entered.
	h.d.status = func(apiSQL) (service.CommandStatusReader, error) {
		return apiStatusReaderFunc(func(c context.Context, u uint64, k, h string) (messaging.CommandStatus, error) {
			admitted = c
			close(entered)
			<-release
			return messaging.CommandStatus{ClientMessageID: k, PayloadHash: h, State: messaging.StatusPendingOrUnknown, PollAfterMs: 2000}, nil
		}), nil
	}
	s := h.service(t)
	if err := s.Start(context.Background(), context.Background()); err != nil {
		t.Fatal(err)
	}
	token := apiToken(t, h)
	type response struct {
		code int
		err  error
	}
	result := make(chan response, 1)
	go func() { code, _, err := apiStatusFetch(s, token); result <- response{code, err} }()
	apiWait(t, entered)
	observer, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Close(observer); !errors.Is(err, errAPIPending) {
		t.Fatal("status callback owner lost", err)
	}
	if h.sql.closed.Load() != 0 || h.redis.closed.Load() != 0 {
		t.Fatal("clients closed before status callback joined")
	}
	// Observer expiry is independent of the cleanup owner's actual work cancel.
	// Release the late callback only after the latter has really occurred.
	apiWait(t, admitted.Done())
	unblock()
	if got := <-result; got.err == nil && got.code != 503 {
		t.Fatal("canceled callback returned success", got.code)
	}
	apiWait(t, s.Done())
	if h.sql.closed.Load() != 1 || h.redis.closed.Load() != 1 {
		t.Fatal("clients not closed after status join")
	}
}

func TestAPIStatusPanicAndStoppedAdmission(t *testing.T) {
	for _, mode := range []string{"panic", "stopped"} {
		t.Run(mode, func(t *testing.T) {
			h := newAPIHarness()
			h.d.status = func(apiSQL) (service.CommandStatusReader, error) {
				return apiStatusReaderFunc(func(context.Context, uint64, string, string) (messaging.CommandStatus, error) {
					panic("PRIVATE_STATUS_BODY")
				}), nil
			}
			s := h.service(t)
			if err := s.Start(context.Background(), context.Background()); err != nil {
				t.Fatal(err)
			}
			if mode == "stopped" {
				s.StopAdmission()
			}
			code, body := apiStatusRequest(t, s, apiToken(t, h))
			if code != 503 || !strings.Contains(body, "DEPENDENCY_UNAVAILABLE") || strings.Contains(body, "PRIVATE") || strings.Contains(body, "state") {
				t.Fatal(code, body)
			}
		})
	}
}

func TestAPIStatusDoesNotGateOnCachedStorageProbeOrPublish(t *testing.T) {
	h := newAPIHarness()
	h.sql.offline.Store(true) // Deliberately stale/unavailable probe capability.
	h.d.status = func(apiSQL) (service.CommandStatusReader, error) {
		return apiStatusReaderFunc(func(ctx context.Context, user uint64, key, hash string) (messaging.CommandStatus, error) {
			return messaging.CommandStatus{ClientMessageID: key, PayloadHash: hash, State: messaging.StatusPendingOrUnknown, PollAfterMs: 2000}, nil
		}), nil
	}
	s := h.service(t)
	if err := s.Start(context.Background(), context.Background()); err != nil {
		t.Fatal(err)
	}
	code, body := apiStatusRequest(t, s, apiToken(t, h))
	if code != 200 || !strings.Contains(body, "pending_or_unknown") || h.publisher.calls.Load() != 0 {
		t.Fatal(code, body)
	}
}
