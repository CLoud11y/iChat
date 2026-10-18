package router

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"iChat/config"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
	"iChat/utils"
)

type submitBlacklist struct {
	reads  int
	banned bool
	err    error
	wait   bool
}

func (b *submitBlacklist) Contains(ctx context.Context, _ string) (bool, error) {
	b.reads++
	if b.wait {
		<-ctx.Done()
		return false, ctx.Err()
	}
	return b.banned, b.err
}
func (*submitBlacklist) Add(context.Context, string, time.Duration) error { return nil }

type routePublisher struct{ calls int }

func (p *routePublisher) Publish(ctx context.Context, c messaging.Command, at time.Time) (rabbitmq.PublishResult, error) {
	p.calls++
	e, err := messaging.NewEnvelope(c, at)
	if err != nil {
		return rabbitmq.PublishResult{}, err
	}
	return rabbitmq.PublishResult{Outcome: rabbitmq.PublishAccepted, CommandID: c.CommandID, PublishAttemptID: e.PublishAttemptID, ConnectionEpoch: 1, ChannelEpoch: 1, DeliveryTag: uint64(p.calls)}, nil
}

type unreadSubmissionBody struct{ reads int }

func (b *unreadSubmissionBody) Read([]byte) (int, error) {
	b.reads++
	return 0, errors.New("body must not be read")
}
func (*unreadSubmissionBody) Close() error { return nil }

func TestSubmissionRouteBearerOnlyAndAuthDependencyFailures(t *testing.T) {
	for _, scenario := range []string{"ok", "missing", "raw_header", "query", "form", "bad_token", "banned", "redis_error", "nil_tokens", "auth_deadline"} {
		t.Run(scenario, func(t *testing.T) {
			var conf config.Config
			conf.JWT.Key, conf.JWT.TokenHourLifeSpan = "submission-route-fixture", 1
			blacklist := &submitBlacklist{}
			tokens, err := utils.NewTokenService(conf, blacklist)
			if err != nil {
				t.Fatal(err)
			}
			token, err := tokens.GenerateToken(42)
			if err != nil {
				t.Fatal(err)
			}
			publisher := &routePublisher{}
			r := gin.New()
			timeout := time.Second
			wantStatus, wantReason := 401, "UNAUTHENTICATED"
			header := "Bearer " + token
			path := "/auth/sendMessage"
			switch scenario {
			case "ok":
				wantStatus, wantReason = 202, "accepted"
			case "missing":
				header = ""
			case "raw_header":
				header = token
			case "query":
				header, path = "", path+"?token="+token
			case "form":
				header = ""
			case "bad_token":
				header = "Bearer broken-token"
			case "banned":
				blacklist.banned = true
			case "redis_error":
				blacklist.err = errors.New("PRIVATE_REDIS_DIAGNOSTIC")
				wantStatus, wantReason = 503, "DEPENDENCY_UNAVAILABLE"
			case "nil_tokens":
				tokens = nil
				wantStatus, wantReason = 503, "DEPENDENCY_UNAVAILABLE"
			case "auth_deadline":
				blacklist.wait = true
				timeout = 20 * time.Millisecond
				wantStatus, wantReason = 503, "DEPENDENCY_UNAVAILABLE"
			}
			if err := RegisterSubmission(r, tokens, publisher, timeout); err != nil {
				t.Fatal(err)
			}
			body := &unreadSubmissionBody{}
			request := httptest.NewRequest(http.MethodPost, path, nil)
			request.Header.Set("Authorization", header)
			if scenario == "ok" {
				request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"clientMessageId":"8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1","targetType":"private","targetId":"34","media":"text","content":"hello"}`))
				request.Header.Set("Authorization", header)
				request.Header.Set("Content-Type", "application/json")
			} else {
				request.Body, request.ContentLength = body, -1
				request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}
			response := httptest.NewRecorder()
			r.ServeHTTP(response, request)
			var decoded messaging.SubmissionResponse
			if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			if response.Code != wantStatus || !strings.Contains(response.Body.String(), wantReason) || strings.Contains(response.Body.String(), "PRIVATE") || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("incorrect submission auth", scenario, response.Code, decoded)
			}
			if scenario != "ok" && (body.reads != 0 || publisher.calls != 0) {
				t.Fatal("failed authentication read body or published", body.reads, publisher.calls)
			}
			if scenario == "ok" && publisher.calls != 1 {
				t.Fatal("authenticated send did not publish exactly once")
			}
		})
	}
}

func TestSubmissionRouteInvalidOptionsDoNotRegister(t *testing.T) {
	r := gin.New()
	if err := RegisterSubmission(r, nil, nil, 0); err == nil || len(r.Routes()) != 0 {
		t.Fatal("invalid submission setup mutated routes")
	}
}
