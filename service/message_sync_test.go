package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"iChat/database"
	"iChat/messaging"
)

type syncReaderFunc func(context.Context, uint64, string, string, int) (messaging.SyncPage, error)

func (f syncReaderFunc) SyncMessages(c context.Context, u uint64, id, k string, n int) (messaging.SyncPage, error) {
	return f(c, u, id, k, n)
}
func syncTestRequest(t *testing.T, reader MessageSyncReader, raw string, user any, ctx context.Context, timeout time.Duration) *httptest.ResponseRecorder {
	t.Helper()
	handler, e := QueryMessageSync(reader, timeout)
	if e != nil {
		t.Fatal(e)
	}
	r := gin.New()
	r.GET("/sync", func(c *gin.Context) {
		if user != nil {
			c.Set("uid", user)
		}
		c.Next()
	}, handler)
	req := httptest.NewRequest("GET", "/sync?"+raw, nil)
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("sync cached")
	}
	return w
}
func TestMessageSyncHTTPDefaultsAndExplicitCursor(t *testing.T) {
	for _, tc := range []struct {
		raw, cursor string
		n           int
		seq         messaging.Sequence
	}{{"conversationId=p:34:42", "", 100, 0}, {"conversationId=p:34:42&afterSeq=0", "0", 100, 0}, {"conversationId=p:34:42&afterSeq=18446744073709551615&limit=500", "18446744073709551615", 500, ^messaging.Sequence(0)}} {
		t.Run(tc.raw, func(t *testing.T) {
			calls := 0
			reader := syncReaderFunc(func(c context.Context, u uint64, id, after string, n int) (messaging.SyncPage, error) {
				calls++
				if u != 42 || id != "p:34:42" || after != tc.cursor || n != tc.n {
					t.Error(u, id, after, n)
				}
				if _, ok := c.Deadline(); !ok {
					t.Error("no deadline")
				}
				return messaging.SyncPage{ConversationID: id, AfterSeq: tc.seq, NextAfterSeq: tc.seq, HighWatermark: tc.seq, Items: []messaging.MessageDTO{}}, nil
			})
			w := syncTestRequest(t, reader, tc.raw, uint(42), nil, time.Second)
			var got messaging.SyncResponse
			if e := json.Unmarshal(w.Body.Bytes(), &got); e != nil || w.Code != 200 || got.Code != 0 || got.Data.AfterSeq != tc.seq || got.Data.NextAfterSeq != tc.seq || calls != 1 || !strings.Contains(w.Body.String(), `"items":[]`) {
				t.Fatal(w.Code, w.Body.String(), e)
			}
		})
	}
}
func TestMessageSyncHTTPRejectsMalformedQueries(t *testing.T) {
	base := "conversationId=p:34:42"
	queries := []string{"", "conversationId=", "conversationId=p:42:34", base + "&conversationId=p:34:42", base + "&afterSeq=", base + "&afterSeq=-1", base + "&afterSeq=01", base + "&afterSeq=%2B1", base + "&afterSeq=1e2", base + "&afterSeq=18446744073709551616", base + "&afterSeq=0&afterSeq=0", base + "&limit=", base + "&limit=0", base + "&limit=01", base + "&limit=501", base + "&limit=1&limit=1", base + "&beforeSeq=1", base + "&userId=1", base + "&senderId=1", base + "&token=secret", "ConversationId=p:34:42", base + "&%zz=x", base + ";limit=1", strings.Repeat("x", MaxSyncQueryBytes+1)}
	for _, raw := range queries {
		calls := 0
		reader := syncReaderFunc(func(context.Context, uint64, string, string, int) (messaging.SyncPage, error) {
			calls++
			return messaging.SyncPage{}, nil
		})
		w := syncTestRequest(t, reader, raw, uint(42), nil, time.Second)
		if w.Code != 400 || calls != 0 || !strings.Contains(w.Body.String(), "INVALID_REQUEST") {
			t.Fatal(raw, w.Code, w.Body.String())
		}
	}
}
func TestMessageSyncHTTPErrorClassificationAndLateResults(t *testing.T) {
	for _, mode := range []string{"nil", "typed_nil", "missing_uid", "wrong_uid", "canceled", "SQL", "forbidden", "ahead", "joined_forbidden", "joined_ahead", "other_invalid", "late", "late_forbidden", "zero_page", "partial_page"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			user := any(uint(42))
			timeout := time.Second
			calls := 0
			var reader MessageSyncReader = syncReaderFunc(func(c context.Context, _ uint64, id, _ string, _ int) (messaging.SyncPage, error) {
				calls++
				p := messaging.SyncPage{ConversationID: id, Items: []messaging.MessageDTO{}}
				switch mode {
				case "SQL":
					return p, errors.New("PRIVATE_SQL")
				case "forbidden":
					return p, database.ErrConversationForbidden
				case "ahead":
					return p, database.ErrSyncCursorAhead
				case "joined_forbidden":
					return p, errors.Join(database.ErrConversationForbidden, errors.New("PRIVATE_RELEASE"))
				case "joined_ahead":
					return p, errors.Join(database.ErrSyncCursorAhead, errors.New("PRIVATE_RELEASE"))
				case "other_invalid":
					return p, messaging.ErrInvalidInput
				case "late":
					<-c.Done()
					return p, nil
				case "late_forbidden":
					<-c.Done()
					return p, database.ErrConversationForbidden
				case "zero_page":
					return messaging.SyncPage{}, nil
				case "partial_page":
					p.HighWatermark = 1
					return p, nil
				default:
					t.Error("unexpected reader call")
				}
				return p, nil
			})
			switch mode {
			case "nil":
				reader = nil
			case "typed_nil":
				reader = syncReaderFunc(nil)
			case "missing_uid":
				user = nil
			case "wrong_uid":
				user = "42"
			case "canceled":
				var stop context.CancelFunc
				ctx, stop = context.WithCancel(ctx)
				stop()
			case "late", "late_forbidden":
				timeout = 10 * time.Millisecond
			}
			w := syncTestRequest(t, reader, "conversationId=p:34:42", user, ctx, timeout)
			want, reason := 503, "DEPENDENCY_UNAVAILABLE"
			switch mode {
			case "missing_uid", "wrong_uid":
				want, reason = 401, "UNAUTHENTICATED"
			case "forbidden":
				want, reason = 403, "FORBIDDEN"
			case "ahead":
				want, reason = 400, "INVALID_REQUEST"
			}
			if w.Code != want || !strings.Contains(w.Body.String(), reason) || strings.Contains(w.Body.String(), "PRIVATE") || strings.Contains(w.Body.String(), `"items"`) || strings.Contains(w.Body.String(), `"state"`) {
				t.Fatal(mode, w.Code, w.Body.String())
			}
			if (mode == "nil" || mode == "typed_nil" || mode == "missing_uid" || mode == "wrong_uid" || mode == "canceled") && calls != 0 {
				t.Fatal("invalid request reached reader")
			}
		})
	}
	for _, d := range []time.Duration{0, -1, time.Minute + 1} {
		if _, e := QueryMessageSync(nil, d); !errors.Is(e, ErrMessageSyncOptions) {
			t.Fatal(e)
		}
	}
}
