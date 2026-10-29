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

type conversationsRouteReader struct {
	calls    int
	deadline time.Time
}

func (r *conversationsRouteReader) ListConversations(ctx context.Context, u uint64, cursor string, n int) (messaging.ConversationPage, error) {
	r.calls++
	r.deadline, _ = ctx.Deadline()
	if u != 42 || n != 100 || cursor != "" {
		return messaging.ConversationPage{}, errors.New("wrong query")
	}
	return messaging.ConversationPage{Items: []messaging.ConversationSummary{}}, nil
}
func TestConversationsRouteAuthAndDeadline(t *testing.T) {
	for _, mode := range []string{"ok", "missing", "query_token", "raw_header", "bad_token", "banned", "redis_error", "nil_tokens", "auth_timeout", "forged_user"} {
		t.Run(mode, func(t *testing.T) {
			var conf config.Config
			conf.JWT.Key, conf.JWT.TokenHourLifeSpan = "conversations-route-fixture", 1
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
			query := ""
			timeout := time.Second
			want, reason := 401, "UNAUTHENTICATED"
			switch mode {
			case "ok":
				want, reason = 200, "conversations"
			case "missing":
				header = ""
			case "query_token":
				header = ""
				query = "token=" + token
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
			case "forged_user":
				query = "userId=1"
				want, reason = 400, "INVALID_REQUEST"
			}
			r := gin.New()
			reader := &conversationsRouteReader{}
			if e := RegisterConversations(r, tokens, reader, timeout); e != nil {
				t.Fatal(e)
			}
			req := httptest.NewRequest("GET", "/auth/conversations?"+query, nil)
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
				t.Fatal("auth/parameter failure reached store")
			}
		})
	}
}
