//go:build integration

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iChat/config"
	"iChat/database"
	"iChat/models"
	"iChat/tests/testenv"
	"iChat/utils"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	after    string
	afterErr error
	cancel   context.CancelFunc
}

func (h *messageCommandHook) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	h.commands = append(h.commands, cmd.Name())
	h.contexts = append(h.contexts, ctx)
	if cmd.Name() == h.fail {
		return ctx, h.cause
	}
	return ctx, nil
}
func (h *messageCommandHook) AfterProcess(_ context.Context, cmd redis.Cmder) error {
	if cmd.Name() == h.after && cmd.Err() == nil {
		if h.cancel != nil {
			h.cancel()
		}
		return h.afterErr
	}
	return nil
}
func (*messageCommandHook) BeforeProcessPipeline(ctx context.Context, _ []redis.Cmder) (context.Context, error) {
	return ctx, nil
}
func (*messageCommandHook) AfterProcessPipeline(context.Context, []redis.Cmder) error { return nil }

func TestMessageHTTPIntegration(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	logPath, err := filepath.Rel(config.FindProjectRoot(wd), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	oldPath := config.Conf.LOG.Path
	config.Conf.LOG.Path = logPath
	logger := utils.Logger()
	config.Conf.LOG.Path = oldPath
	if file, ok := logger.Out.(*os.File); ok {
		t.Cleanup(func() { _ = file.Close(); logger.SetOutput(io.Discard) })
	}
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
	// Enqueued/cached messages are not yet persisted history.
	var pendingRows []RespMsgInfo
	contactData(t, contactRequest(GetMessageList(store), historyBody, ctx, uid), &pendingRows)
	if len(pendingRows) != 0 {
		t.Fatalf("pending message presented as persisted history: %+v", pendingRows)
	}
	assertAccountUnavailable(t, contactRequest(GetMessageList(database.NewMessageManager(nil, f.Redis)), historyBody, ctx, uid))
	if err := store.SaveMsgs2DB(ctx, raw); err != nil {
		t.Fatal(err)
	}
	if err := f.Redis.Del(ctx, key).Err(); err != nil {
		t.Fatal(err)
	}
	assertAccountUnavailable(t, contactRequest(GetMessageList(database.NewMessageManager(nil, f.Redis)), historyBody, ctx, uid))
	assertHistory(store)
	// Old cache contents are also not an authority for history.
	if err := f.Redis.Del(ctx, key).Err(); err != nil {
		t.Fatal(err)
	}
	legacyKey := fmt.Sprintf("msg:private:%d.%d", uid, target)
	if err := f.Redis.ZAdd(ctx, legacyKey, &redis.Z{Score: float64(msg.TimeStamp), Member: raw[0]}).Err(); err != nil {
		t.Fatal(err)
	}
	assertAccountUnavailable(t, contactRequest(GetMessageList(database.NewMessageManager(nil, f.Redis)), historyBody, ctx, uid))
	assertHistory(store)
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
	// MySQL failure must not fall back to a cache hit.
	if err := f.Redis.Set(ctx, key, "warm-but-invalid-cache", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := f.DB.Exec("RENAME TABLE message TO http_message_unavailable").Error; err != nil {
		t.Fatal(err)
	}
	assertAccountUnavailable(t, contactRequest(GetMessageList(store), historyBody, ctx, uid))
	if err := f.DB.Exec("RENAME TABLE http_message_unavailable TO message").Error; err != nil {
		t.Fatal(err)
	}
	assertHistory(store)
	if err := f.Redis.Del(ctx, key).Err(); err != nil {
		t.Fatal(err)
	}
	missingCursor := msg
	missingCursor.Id = "missing-cursor"
	if _, err := store.LoadMsgs(ctx, uid, target, models.PrivateType, missingCursor, 10); !errors.Is(err, database.ErrMessageCursorNotFound) {
		t.Fatalf("missing cursor: %v", err)
	}
	// Corrupt one cache member with a valid prefix but malformed JSON.
	if err := f.Redis.ZAdd(ctx, key, &redis.Z{Score: float64(msg.TimeStamp + 1), Member: fmt.Sprintf("%020d:%020d:{", msg.TimeStamp+1, 99)}).Err(); err != nil {
		t.Fatal(err)
	}
	assertHistory(store)

	assertUnknown := func(response *httptest.ResponseRecorder) {
		t.Helper()
		if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"reasonCode":"MESSAGE_ENQUEUE_UNKNOWN"`) || !strings.Contains(response.Body.String(), `"status":"enqueue_unknown"`) {
			t.Fatalf("enqueue outcome: %d %s", response.Code, response.Body)
		}
	}
	assertSuccess := func(response *httptest.ResponseRecorder) {
		t.Helper()
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"code":0`) {
			t.Fatalf("send: %d %s", response.Code, response.Body)
		}
	}
	assertQueued := func() {
		t.Helper()
		got, err := f.Redis.LRange(ctx, "dirty:msgs", 0, -1).Result()
		if err != nil || !reflect.DeepEqual(got, raw) {
			t.Fatalf("queue=%v want=%v error=%v", got, raw, err)
		}
	}
	// Real WRONGTYPE on RPUSH must skip both cache and notification. The shared
	// retrying client exposes no per-attempt evidence, so the HTTP result is unknown.
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
	if err := f.Redis.Del(ctx, key).Err(); err != nil {
		t.Fatal(err)
	}
	assertUnknown(contactRequest(SendMsg(database.NewMessageManager(f.DB, client)), string(body), ctx, uid))
	if !reflect.DeepEqual(hook.commands, []string{"rpush"}) {
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
	if n, err := f.Redis.Exists(ctx, key).Result(); err != nil || n != 0 {
		t.Fatalf("rejected send created cache: %d %v", n, err)
	}
	if err := f.Redis.Del(ctx, "dirty:msgs").Err(); err != nil {
		t.Fatal(err)
	}
	// RPUSH succeeds while ZADD receives a real Redis WRONGTYPE response.
	if err := f.Redis.Set(ctx, key, "wrong-type", 0).Err(); err != nil {
		t.Fatal(err)
	}
	hook.commands = nil
	assertSuccess(contactRequest(SendMsg(database.NewMessageManager(f.DB, client)), string(body), ctx, uid))
	assertQueued()
	if !reflect.DeepEqual(hook.commands, []string{"rpush", "zadd", "publish"}) {
		t.Fatalf("cache failure commands: %v", hook.commands)
	}
	if value, err := f.Redis.Get(ctx, key).Result(); err != nil || value != "wrong-type" {
		t.Fatalf("cache failure replaced key: %q %v", value, err)
	}
	if err := f.Redis.Del(ctx, "dirty:msgs", key).Err(); err != nil {
		t.Fatal(err)
	}
	// Notification failure must retain both the queue and the successfully written cache.
	hook.fail, hook.cause = "publish", errors.New("injected publish failure")
	hook.commands = nil
	assertSuccess(contactRequest(SendMsg(database.NewMessageManager(f.DB, client)), string(body), ctx, uid))
	assertQueued()
	// History now reads MySQL, so it cannot prove that PUBLISH failure kept
	// the ZADD result. Inspect Redis itself to catch accidental cache removal.
	cached, err := f.Redis.ZRange(ctx, key, 0, -1).Result()
	if err != nil || len(cached) != 1 || !strings.HasSuffix(cached[0], ":"+raw[0]) {
		t.Fatalf("publish failure changed cache: %q err=%v", cached, err)
	}
	assertHistory(store)
	if !reflect.DeepEqual(hook.commands, []string{"rpush", "zadd", "publish"}) {
		t.Fatalf("publish failure commands: %v", hook.commands)
	}
	// Simulate losing the result AFTER a real RPUSH. Redis demonstrably contains
	// the message while the caller receives a timeout/cancellation/connection loss.
	hook.fail = ""
	hook.after = "rpush"
	for _, cause := range []error{context.DeadlineExceeded, context.Canceled, io.EOF} {
		if err := f.Redis.Del(ctx, "dirty:msgs", key).Err(); err != nil {
			t.Fatal(err)
		}
		hook.commands, hook.afterErr = nil, cause
		assertUnknown(contactRequest(SendMsg(database.NewMessageManager(f.DB, client)), string(body), ctx, uid))
		assertQueued()
		if !reflect.DeepEqual(hook.commands, []string{"rpush"}) {
			t.Fatalf("lost reply commands: %v", hook.commands)
		}
		if n, err := f.Redis.Exists(ctx, key).Result(); err != nil || n != 0 {
			t.Fatalf("unknown send created cache: %d %v", n, err)
		}
	}
	// Request cancellation after confirmed RPUSH also cannot undo acceptance.
	if err := f.Redis.Del(ctx, "dirty:msgs").Err(); err != nil {
		t.Fatal(err)
	}
	acceptedCtx, cancelAccepted := context.WithCancel(ctx)
	defer cancelAccepted()
	hook.afterErr, hook.cancel = nil, cancelAccepted
	assertSuccess(contactRequest(SendMsg(database.NewMessageManager(f.DB, client)), string(body), acceptedCtx, uid))
	assertQueued()

	closed := redis.NewClient(f.Redis.Options())
	_ = closed.Close()
	unavailable := database.NewMessageManager(f.DB, closed)
	assertUnknown(contactRequest(SendMsg(unavailable), string(body), ctx, uid))
	assertHistory(unavailable)
}
