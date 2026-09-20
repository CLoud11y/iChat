//go:build integration

package service

import (
	"context"
	"fmt"
	"iChat/config"
	"iChat/database"
	"iChat/middlewares"
	"iChat/models"
	"iChat/tests/testenv"
	"iChat/utils"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/gorilla/websocket"
)

func TestChatRedisIntegration(t *testing.T) {
	f := testenv.NewLegacyFixture(t)
	ctx := context.Background()
	store := database.NewMessageManager(f.DB, f.Redis)
	var conf config.Config
	conf.JWT.Key = "websocket-fixture"
	conf.JWT.TokenHourLifeSpan = 1
	tokens, err := utils.NewTokenService(conf, utils.NewRedisTokenBlacklist(f.Redis))
	if err != nil {
		t.Fatal(err)
	}
	uid := f.Users[0].ID
	raw, err := tokens.GenerateToken(uid)
	if err != nil {
		t.Fatal(err)
	}
	oldMessages, oldUsers, oldGroups := database.Mmanager, database.Umanager, database.Gmanager
	database.Mmanager, database.Umanager, database.Gmanager = nil, nil, nil
	t.Cleanup(func() { database.Mmanager, database.Umanager, database.Gmanager = oldMessages, oldUsers, oldGroups })
	var offline atomic.Int32
	done := make(chan string, 8)
	onOffline := func(uint) { offline.Add(1) }
	r := gin.New()
	r.Use(middlewares.JwtAuth(tokens))
	closed := redis.NewClient(f.Redis.Options())
	_ = closed.Close()
	for path, provider := range map[string]MessageSubscriptions{"/chat": store, "/getws": store, "/missing-db": database.NewMessageManager(nil, f.Redis), "/closed-redis": database.NewMessageManager(f.DB, closed)} {
		handler := Chat(provider, onOffline, quietChatLogger())
		r.GET(path, func(c *gin.Context) { handler(c); done <- c.Request.URL.Path })
	}
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)
	url := "ws" + strings.TrimPrefix(server.URL, "http")
	channels := []string{fmt.Sprintf("private_%d", uid), fmt.Sprintf("group_%d", f.Groups[0].ID), fmt.Sprintf("group_%d", f.Groups[1].ID)}
	assertCounts := func(want int64) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			counts, err := f.Redis.PubSubNumSub(ctx, channels...).Result()
			if err != nil {
				t.Fatal(err)
			}
			match := true
			for _, ch := range channels {
				if counts[ch] != want {
					match = false
				}
			}
			if match {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("counts=%v want=%d", counts, want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitDone := func(path string) {
		t.Helper()
		select {
		case got := <-done:
			if got != path {
				t.Fatalf("completed %s want %s", got, path)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("handler did not exit")
		}
	}
	failedDial := func(path, token string, status int) {
		t.Helper()
		header := http.Header{}
		if token != "" {
			header.Set("Authorization", "Bearer "+token)
		}
		conn, response, err := websocket.DefaultDialer.Dial(url+path, header)
		if conn != nil {
			_ = conn.Close()
			t.Fatal("failed setup upgraded connection")
		}
		if err == nil || response == nil || response.StatusCode != status {
			t.Fatalf("dial error=%v response=%v", err, response)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if status == 503 && !strings.Contains(string(body), "DEPENDENCY_UNAVAILABLE") {
			t.Fatalf("failure body=%s", body)
		}
	}
	failedDial("/chat", "", 401)
	assertCounts(0)
	headers := http.Header{"Authorization": []string{"Bearer " + raw}}
	conn, _, err := websocket.DefaultDialer.Dial(url+"/chat", headers)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	assertCounts(1)
	publish := func(group bool) {
		t.Helper()
		kind, target := uint(models.PrivateType), uid
		frame := "simple"
		if group {
			kind, target, frame = uint(models.GroupType), f.Groups[0].ID, "group"
		}
		msg := f.Message(f.Users[1].ID, target, kind, models.TextMedia, "socket fixture")
		if err := store.PublishMsg(ctx, &msg); err != nil {
			t.Fatal(err)
		}
		got := readSocketControl(t, conn)
		if got.Type != frame {
			t.Fatalf("frame=%s", got.Type)
		}
	}
	publish(false)
	publish(true)
	// Failed reconnect must close only its new private subscription and leave
	// the original authenticated session registered and usable.
	failedDial("/missing-db", raw, 503)
	waitDone("/missing-db")
	assertCounts(1)
	failedDial("/closed-redis", raw, 503)
	waitDone("/closed-redis")
	assertCounts(1)
	if offline.Load() != 0 {
		t.Fatal("failed reconnect took existing user offline")
	}
	publish(false)
	_ = conn.Close()
	waitDone("/chat")
	assertCounts(0)
	if offline.Load() != 1 {
		t.Fatalf("offline count=%d", offline.Load())
	}
	// A user with no group memberships still has a usable private connection.
	unrelated := f.Users[4].ID
	otherToken, err := tokens.GenerateToken(unrelated)
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := websocket.DefaultDialer.Dial(url+"/getws", http.Header{"Authorization": []string{"Bearer " + otherToken}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	msg := f.Message(uid, unrelated, models.PrivateType, models.TextMedia, "no groups")
	if err := store.PublishMsg(ctx, &msg); err != nil {
		t.Fatal(err)
	}
	if got := readSocketControl(t, other); got.Type != "simple" {
		t.Fatalf("no-group frame=%s", got.Type)
	}
	_ = other.Close()
	waitDone("/getws")
	channels = append(channels, fmt.Sprintf("private_%d", unrelated))
	assertCounts(0)
	if n, err := f.Redis.PubSubNumSub(ctx, fmt.Sprintf("private_%d", unrelated)).Result(); err != nil || n[fmt.Sprintf("private_%d", unrelated)] != 0 {
		t.Fatalf("private subscription left: %v %v", n, err)
	}
	if offline.Load() != 2 {
		t.Fatalf("offline count=%d", offline.Load())
	}
}
