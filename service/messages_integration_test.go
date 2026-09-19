//go:build integration

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iChat/database"
	"iChat/models"
	"iChat/tests/testenv"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/go-redis/redis/v8"
)

type messageCommandHook struct {
	fail     string
	cause    error
	commands []string
	contexts []context.Context
}

func (h *messageCommandHook) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	h.commands = append(h.commands, cmd.Name())
	h.contexts = append(h.contexts, ctx)
	if cmd.Name() == h.fail {
		return ctx, h.cause
	}
	return ctx, nil
}
func (*messageCommandHook) AfterProcess(context.Context, redis.Cmder) error { return nil }
func (*messageCommandHook) BeforeProcessPipeline(ctx context.Context, _ []redis.Cmder) (context.Context, error) {
	return ctx, nil
}
func (*messageCommandHook) AfterProcessPipeline(context.Context, []redis.Cmder) error { return nil }

func TestMessageHTTPIntegration(t *testing.T) {
	f := testenv.NewLegacyFixture(t)
	ctx := context.Background()
	store := database.NewMessageManager(f.DB, f.Redis)
	old := database.Mmanager
	database.Mmanager = nil
	t.Cleanup(func() { database.Mmanager = old })
	uid, target := f.Users[0].ID, f.Users[1].ID
	msg := f.Message(uid, target, models.PrivateType, models.TextMedia, "original")
	info := msg.Conv2MsgInfo()
	info.FromUser.Id = f.Users[4].ID
	body, _ := json.Marshal(info)
	response := contactRequest(SendMsg(store), string(body), ctx, uid)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"code":0`) {
		t.Fatalf("send: %s", response.Body)
	}
	raw, err := f.Redis.LRange(ctx, "dirty:msgs", 0, -1).Result()
	if err != nil || len(raw) != 1 {
		t.Fatalf("queue: %v %v", raw, err)
	}
	var queued models.Message
	if err := json.Unmarshal([]byte(raw[0]), &queued); err != nil || queued != msg {
		t.Fatalf("queued identity/content: %+v err=%v", queued, err)
	}
	key := fmt.Sprintf("msg:v2:private:%d.%d", uid, target)
	historyBody := fmt.Sprintf(`{"toContactId":%d,"limit":10}`, target)
	assertHistory := func(s MessageStore) {
		t.Helper()
		var rows []RespMsgInfo
		contactData(t, contactRequest(GetMessageList(s), historyBody, ctx, uid), &rows)
		if len(rows) != 1 || rows[0].Id != msg.Id || rows[0].FromUser.Id != uid || rows[0].Content != msg.Content {
			t.Fatalf("history=%+v", rows)
		}
	}
	assertHistory(store)
	// A cache hit needs Redis only; a cold read additionally needs MySQL.
	assertHistory(database.NewMessageManager(nil, f.Redis))
	if err := store.SaveMsgs2DB(ctx, raw); err != nil {
		t.Fatal(err)
	}
	if err := f.Redis.Del(ctx, key).Err(); err != nil {
		t.Fatal(err)
	}
	assertAccountUnavailable(t, contactRequest(GetMessageList(database.NewMessageManager(nil, f.Redis)), historyBody, ctx, uid))
	assertHistory(store)
	// The old cache format still migrates using the request's context.
	if err := f.Redis.Del(ctx, key).Err(); err != nil {
		t.Fatal(err)
	}
	legacyKey := fmt.Sprintf("msg:private:%d.%d", uid, target)
	if err := f.Redis.ZAdd(ctx, legacyKey, &redis.Z{Score: float64(msg.TimeStamp), Member: raw[0]}).Err(); err != nil {
		t.Fatal(err)
	}
	assertHistory(database.NewMessageManager(nil, f.Redis))
	if err := f.Redis.Del(ctx, key, legacyKey).Err(); err != nil {
		t.Fatal(err)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.LoadMsgs(canceled, uid, target, models.PrivateType, models.Message{Type: models.InvalidType}, 10); !errors.Is(err, context.Canceled) || !errors.Is(err, database.ErrStorageUnavailable) {
		t.Fatalf("canceled history: %v", err)
	}
	assertAccountUnavailable(t, contactRequest(SendMsg(store), string(body), canceled, uid))
	if n, err := f.Redis.LLen(ctx, "dirty:msgs").Result(); err != nil || n != 1 {
		t.Fatalf("canceled send changed queue: %d %v", n, err)
	}
	// MySQL is only reached on a cache miss; make that read really fail.
	if err := f.DB.Exec("RENAME TABLE message TO http_message_unavailable").Error; err != nil {
		t.Fatal(err)
	}
	assertAccountUnavailable(t, contactRequest(GetMessageList(store), historyBody, ctx, uid))
	if err := f.DB.Exec("RENAME TABLE http_message_unavailable TO message").Error; err != nil {
		t.Fatal(err)
	}
	assertHistory(store)
	missingCursor := msg
	missingCursor.Id = "missing-cursor"
	if _, err := store.LoadMsgs(ctx, uid, target, models.PrivateType, missingCursor, 10); !errors.Is(err, database.ErrMessageCursorNotFound) {
		t.Fatalf("missing cursor: %v", err)
	}
	// Corrupt one cache member with a valid prefix but malformed JSON.
	if err := f.Redis.ZAdd(ctx, key, &redis.Z{Score: float64(msg.TimeStamp + 1), Member: fmt.Sprintf("%020d:%020d:{", msg.TimeStamp+1, 99)}).Err(); err != nil {
		t.Fatal(err)
	}
	assertAccountUnavailable(t, contactRequest(GetMessageList(store), historyBody, ctx, uid))

	// Real WRONGTYPE on RPUSH after ZADD succeeds must produce 503 and skip PUBLISH.
	if err := f.Redis.Del(ctx, "dirty:msgs").Err(); err != nil {
		t.Fatal(err)
	}
	if err := f.Redis.Set(ctx, "dirty:msgs", "wrong-type", 0).Err(); err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(f.Redis.Options())
	t.Cleanup(func() { _ = client.Close() })
	hook := &messageCommandHook{}
	client.AddHook(hook)
	assertAccountUnavailable(t, contactRequest(SendMsg(database.NewMessageManager(f.DB, client)), string(body), ctx, uid))
	if !reflect.DeepEqual(hook.commands, []string{"zadd", "rpush"}) {
		t.Fatalf("unexpected send commands: %v", hook.commands)
	}
	for _, got := range hook.contexts {
		if got != ctx {
			t.Fatal("request context not passed to Redis")
		}
	}
	if value, err := f.Redis.Get(ctx, "dirty:msgs").Result(); err != nil || value != "wrong-type" {
		t.Fatalf("queue modified: %q %v", value, err)
	}
	if err := f.Redis.Del(ctx, "dirty:msgs").Err(); err != nil {
		t.Fatal(err)
	}
	hook.fail, hook.cause = "publish", errors.New("injected publish failure")
	assertAccountUnavailable(t, contactRequest(SendMsg(database.NewMessageManager(f.DB, client)), string(body), ctx, uid))
	if n, err := f.Redis.LLen(ctx, "dirty:msgs").Result(); err != nil || n != 1 {
		t.Fatalf("publish failure queue: %d %v", n, err)
	}
	// Publication failed after enqueue; 503 does not mean nothing was written.
	closed := redis.NewClient(f.Redis.Options())
	_ = closed.Close()
	unavailable := database.NewMessageManager(f.DB, closed)
	assertAccountUnavailable(t, contactRequest(SendMsg(unavailable), string(body), ctx, uid))
	assertAccountUnavailable(t, contactRequest(GetMessageList(unavailable), historyBody, ctx, uid))
}
