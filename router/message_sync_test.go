package router

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"iChat/config"
	"iChat/messaging"
	"iChat/utils"
)

type syncRouteReader struct {
	calls    int
	deadline time.Time
}

func (r *syncRouteReader) SyncMessages(c context.Context, u uint64, id, after string, n int) (messaging.SyncPage, error) {
	r.calls++
	r.deadline, _ = c.Deadline()
	if u != 42 || id != "p:34:42" || after != "" || n != 100 {
		return messaging.SyncPage{}, errors.New("wrong identity")
	}
	return messaging.SyncPage{ConversationID: id, Items: []messaging.MessageDTO{}}, nil
}
func TestMessageSyncRouteBearerAndSharedDeadline(t *testing.T) {
	for _, mode := range []string{"ok", "missing", "query_token", "raw_header", "bad_token", "banned", "redis_error", "nil_tokens", "auth_timeout", "forged_uid"} {
		t.Run(mode, func(t *testing.T) {
			var conf config.Config
			conf.JWT.Key, conf.JWT.TokenHourLifeSpan = "sync-route-fixture", 1
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
			raw := "conversationId=p:34:42"
			timeout := time.Second
			want, reason := 401, "UNAUTHENTICATED"
			switch mode {
			case "ok":
				want, reason = 200, "messages synced"
			case "missing":
				header = ""
			case "query_token":
				header = ""
				raw += "&token=" + token
			case "raw_header":
				header = token
			case "bad_token":
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
				raw += "&userId=1"
				want, reason = 400, "INVALID_REQUEST"
			}
			r := gin.New()
			reader := &syncRouteReader{}
			if e := RegisterMessageSync(r, tokens, reader, timeout); e != nil {
				t.Fatal(e)
			}
			req := httptest.NewRequest("GET", "/auth/messages/sync?"+raw, nil)
			req.Header.Set("Authorization", header)
			body := &unreadSubmissionBody{}
			req.Body = body
			w := httptest.NewRecorder()
			start := time.Now()
			r.ServeHTTP(w, req)
			if w.Code != want || !strings.Contains(w.Body.String(), reason) || strings.Contains(w.Body.String(), "PRIVATE") || w.Header().Get("Cache-Control") != "no-store" || body.reads != 0 {
				t.Fatal(w.Code, w.Body.String())
			}
			if mode == "ok" {
				if reader.calls != 1 || reader.deadline.After(start.Add(timeout+10*time.Millisecond)) {
					t.Fatal("deadline not shared")
				}
			} else if reader.calls != 0 {
				t.Fatal("auth failure reached reader")
			}
		})
	}
}
