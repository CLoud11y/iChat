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
	"iChat/utils"
)

type statusRouteReader struct {
	calls    int
	deadline time.Time
}

func (r *statusRouteReader) GetCommandStatus(ctx context.Context, u uint64, k, h string) (messaging.CommandStatus, error) {
	r.calls++
	r.deadline, _ = ctx.Deadline()
	if u != 42 {
		return messaging.CommandStatus{}, errors.New("unexpected user")
	}
	return messaging.CommandStatus{ClientMessageID: k, PayloadHash: h, State: messaging.StatusPendingOrUnknown, PollAfterMs: 2000}, nil
}
func TestCommandStatusRouteAuthAndSharedDeadline(t *testing.T) {
	for _, mode := range []string{"ok", "missing", "query_token", "raw_header", "invalid", "banned", "redis_error", "nil_tokens", "auth_timeout", "forged_uid"} {
		t.Run(mode, func(t *testing.T) {
			var conf config.Config
			conf.JWT.Key, conf.JWT.TokenHourLifeSpan = "status-route-fixture", 1
			b := &submitBlacklist{}
			tokens, e := utils.NewTokenService(conf, b)
			if e != nil {
				t.Fatal(e)
			}
			token, e := tokens.GenerateToken(42)
			if e != nil {
				t.Fatal(e)
			}
			header := "Bearer " + token
			query := "clientMessageId=8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1&payloadHash=" + strings.Repeat("a", 64)
			timeout := time.Second
			want, reason := 401, "UNAUTHENTICATED"
			switch mode {
			case "ok":
				want, reason = 200, "pending_or_unknown"
			case "missing":
				header = ""
			case "query_token":
				header = ""
				query += "&token=" + token
			case "raw_header":
				header = token
			case "invalid":
				header = "Bearer invalid"
			case "banned":
				b.banned = true
			case "redis_error":
				b.err = errors.New("PRIVATE_REDIS")
				want, reason = 503, "DEPENDENCY_UNAVAILABLE"
			case "nil_tokens":
				tokens = nil
				want, reason = 503, "DEPENDENCY_UNAVAILABLE"
			case "auth_timeout":
				b.wait = true
				timeout = 10 * time.Millisecond
				want, reason = 503, "DEPENDENCY_UNAVAILABLE"
			case "forged_uid":
				query += "&senderId=1"
				want, reason = 400, "INVALID_REQUEST"
			}
			r := gin.New()
			reader := &statusRouteReader{}
			if e := RegisterCommandStatus(r, tokens, reader, timeout); e != nil {
				t.Fatal(e)
			}
			request := httptest.NewRequest(http.MethodGet, "/auth/messages/status?"+query, nil)
			request.Header.Set("Authorization", header)
			body := &unreadSubmissionBody{}
			request.Body = body
			w := httptest.NewRecorder()
			start := time.Now()
			r.ServeHTTP(w, request)
			if w.Code != want || !strings.Contains(w.Body.String(), reason) || strings.Contains(w.Body.String(), "PRIVATE") || w.Header().Get("Cache-Control") != "no-store" || body.reads != 0 {
				t.Fatal(w.Code, w.Body.String())
			}
			if mode == "ok" {
				if reader.calls != 1 || reader.deadline.After(start.Add(timeout+10*time.Millisecond)) {
					t.Fatal("deadline/auth handoff")
				}
				var response messaging.CommandStatusResponse
				if e := json.Unmarshal(w.Body.Bytes(), &response); e != nil || response.Data.PollAfterMs != 2000 {
					t.Fatal(e)
				}
			} else if reader.calls != 0 {
				t.Fatal("unauthorized/invalid request reached SQL")
			}
		})
	}
}
