package service

import (
	"context"
	"encoding/json"
	"errors"
	"iChat/config"
	"iChat/middlewares"
	"iChat/utils"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

type authBlacklist struct {
	banned            bool
	readErr, writeErr error
	reads, writes     int
	ctx               context.Context
}

func (b *authBlacklist) Contains(ctx context.Context, _ string) (bool, error) {
	b.reads++
	b.ctx = ctx
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return b.banned, b.readErr
}
func (b *authBlacklist) Add(ctx context.Context, _ string, _ time.Duration) error {
	b.writes++
	b.ctx = ctx
	if b.writeErr != nil {
		return b.writeErr
	}
	b.banned = true
	return nil
}

func TestAuthHTTPFailuresAndLogoutOrdering(t *testing.T) {
	for _, action := range []string{"protected", "logout"} {
		for _, failure := range []string{"none", "missing", "malformed", "read", "banned", "write", "nil_service", "nil_redis", "canceled"} {
			t.Run(action+"/"+failure, func(t *testing.T) {
				var conf config.Config
				conf.JWT.Key, conf.JWT.TokenHourLifeSpan = "http-test-key", 1
				store := &authBlacklist{}
				tokens, err := utils.NewTokenService(conf, store)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := tokens.GenerateToken(42)
				if err != nil {
					t.Fatal(err)
				}
				wantStatus := http.StatusOK
				switch failure {
				case "missing":
					raw = ""
					wantStatus = http.StatusUnauthorized
				case "malformed":
					raw = "broken-token"
					wantStatus = http.StatusUnauthorized
				case "read":
					store.readErr = errors.New("private Redis endpoint details")
					wantStatus = http.StatusServiceUnavailable
				case "banned":
					store.banned = true
					wantStatus = http.StatusUnauthorized
				case "write":
					store.writeErr = errors.New("private Redis endpoint details")
					if action == "logout" {
						wantStatus = http.StatusServiceUnavailable
					}
				case "nil_service":
					tokens = nil
					wantStatus = http.StatusServiceUnavailable
				case "nil_redis":
					tokens, err = utils.NewTokenService(conf, utils.NewRedisTokenBlacklist(nil))
					if err != nil {
						t.Fatal(err)
					}
					wantStatus = http.StatusServiceUnavailable
				case "canceled":
					wantStatus = http.StatusServiceUnavailable
				}
				outbound := make(chan websocketOutbound, 1)
				session := &websocketSession{outbound: outbound, cancel: func() {}}
				registerWebsocketSession(42, session)
				t.Cleanup(func() { unregisterWebsocketSession(42, session) })
				offline, visits := 0, 0
				r := gin.New()
				r.POST("/logout", LogoutUser(tokens, func(uid uint) {
					if uid != 42 || store.writes != 1 || !store.banned {
						t.Error("offline before successful revocation")
					}
					offline++
				}))
				r.POST("/protected", middlewares.JwtAuth(tokens), func(c *gin.Context) {
					visits++
					if c.GetUint("uid") != 42 {
						t.Error("wrong authenticated UID")
					}
					c.JSON(http.StatusOK, gin.H{"code": 0})
				})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if failure == "canceled" {
					cancel()
				}
				req := httptest.NewRequest("POST", "/"+action, nil).WithContext(ctx)
				if raw != "" {
					req.Header.Set("Authorization", "Bearer "+raw)
				}
				recorder := httptest.NewRecorder()
				r.ServeHTTP(recorder, req)
				if recorder.Code != wantStatus {
					t.Fatalf("status=%d want=%d body=%s", recorder.Code, wantStatus, recorder.Body)
				}
				if wantStatus != http.StatusOK {
					if offline != 0 || visits != 0 || len(outbound) != 0 {
						t.Fatal("failed auth changed session or reached business handler")
					}
					var body struct {
						Code int `json:"code"`
						Data struct {
							Reason string `json:"reasonCode"`
						} `json:"data"`
					}
					if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
						t.Fatal(err)
					}
					wantReason := "UNAUTHENTICATED"
					if wantStatus == http.StatusServiceUnavailable {
						wantReason = "DEPENDENCY_UNAVAILABLE"
					}
					if body.Code != -1 || body.Data.Reason != wantReason || strings.Contains(recorder.Body.String(), "private Redis") {
						t.Fatalf("incorrect error contract: %s", recorder.Body)
					}
				} else if action == "logout" {
					if offline != 1 || store.writes != 1 || len(outbound) != 1 {
						t.Fatal("successful logout did not revoke then disconnect")
					}
					message := <-outbound
					if !message.closeAfter || !strings.Contains(string(message.data), "offline") {
						t.Fatal("incorrect logout WebSocket frame")
					}
					next := httptest.NewRequest("POST", "/protected", nil)
					next.Header.Set("Authorization", "Bearer "+raw)
					result := httptest.NewRecorder()
					r.ServeHTTP(result, next)
					if result.Code != http.StatusUnauthorized || visits != 0 {
						t.Fatal("revoked token still reaches protected route")
					}
				} else if visits != 1 || store.writes != 0 {
					t.Fatal("valid request not authorized exactly once")
				}
				if (failure == "canceled" || failure == "read") && store.ctx != ctx {
					t.Fatal("request context not forwarded")
				}
			})
		}
	}
}
