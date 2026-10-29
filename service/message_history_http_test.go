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

type historyReaderFunc func(context.Context, uint64, string, string, int) (messaging.HistoryPage, error)

func (f historyReaderFunc) HistoryMessages(c context.Context, u uint64, id, k string, n int) (messaging.HistoryPage, error) {
	return f(c, u, id, k, n)
}
func historyTestRequest(t *testing.T, reader MessageHistoryReader, raw string, user any, ctx context.Context, timeout time.Duration) *httptest.ResponseRecorder {
	t.Helper()
	handler, e := QueryMessageHistory(reader, timeout)
	if e != nil {
		t.Fatal(e)
	}
	r := gin.New()
	r.GET("/history", func(c *gin.Context) {
		if user != nil {
			c.Set("uid", user)
		}
		c.Next()
	}, handler)
	req := httptest.NewRequest("GET", "/history?"+raw, nil)
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("history cached")
	}
	return w
}
func TestMessageHistoryHTTPDefaultsAndExplicitCursor(t *testing.T) {
	for _, tc := range []struct {
		cursor string
		n      int
	}{{"", 100}, {"0", 100}, {"1", 100}, {"18446744073709551615", 500}} {
		t.Run(tc.cursor, func(t *testing.T) {
			raw := "conversationId=p:34:42"
			if tc.cursor != "" {
				raw += "&beforeSeq=" + tc.cursor
			}
			if tc.n == 500 {
				raw += "&limit=500"
			}
			q, _ := messaging.NormalizePageQuery(42, "p:34:42", tc.cursor, tc.n)
			calls := 0
			reader := historyReaderFunc(func(c context.Context, u uint64, id, before string, n int) (messaging.HistoryPage, error) {
				calls++
				if u != 42 || id != q.Conversation.ID || before != tc.cursor || n != tc.n {
					t.Error(u, id, before, n)
				}
				if _, ok := c.Deadline(); !ok {
					t.Error("no deadline")
				}
				p := messaging.HistoryPage{ConversationID: id, NextBeforeSeq: q.Cursor, Items: []messaging.MessageDTO{}}
				if q.HasCursor {
					p.BeforeSeq = &q.Cursor
				}
				return p, nil
			})
			w := historyTestRequest(t, reader, raw, uint(42), nil, time.Second)
			var got messaging.HistoryResponse
			if e := json.Unmarshal(w.Body.Bytes(), &got); e != nil || w.Code != 200 || got.Code != 0 || got.Data.NextBeforeSeq != q.Cursor || calls != 1 || !strings.Contains(w.Body.String(), `"items":[]`) || (got.Data.BeforeSeq != nil) != q.HasCursor {
				t.Fatal(w.Code, w.Body.String(), e)
			}
		})
	}
}
func TestMessageHistoryHTTPRejectsMalformedQueries(t *testing.T) {
	base := "conversationId=p:34:42"
	queries := []string{"", "conversationId=", "conversationId=p:42:34", base + "&conversationId=p:34:42", base + "&beforeSeq=", base + "&beforeSeq=-1", base + "&beforeSeq=01", base + "&beforeSeq=%2B1", base + "&beforeSeq=1e2", base + "&beforeSeq=18446744073709551616", base + "&beforeSeq=0&beforeSeq=0", base + "&limit=", base + "&limit=0", base + "&limit=01", base + "&limit=501", base + "&limit=1&limit=1", base + "&afterSeq=1", base + "&userId=1", base + "&senderId=1", base + "&token=secret", "ConversationId=p:34:42", base + "&%zz=x", base + ";limit=1", strings.Repeat("x", MaxHistoryQueryBytes+1)}
	for _, raw := range queries {
		calls := 0
		reader := historyReaderFunc(func(context.Context, uint64, string, string, int) (messaging.HistoryPage, error) {
			calls++
			return messaging.HistoryPage{}, nil
		})
		w := historyTestRequest(t, reader, raw, uint(42), nil, time.Second)
		if w.Code != 400 || calls != 0 || !strings.Contains(w.Body.String(), "INVALID_REQUEST") {
			t.Fatal(raw, w.Code, w.Body.String())
		}
	}
}
func TestMessageHistoryHTTPErrorClassificationAndLateResults(t *testing.T) {
	for _, mode := range []string{"nil", "typed_nil", "missing_uid", "wrong_uid", "canceled", "SQL", "forbidden", "joined_forbidden", "other_invalid", "late", "late_forbidden", "zero_page", "partial_page"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			user := any(uint(42))
			timeout := time.Second
			calls := 0
			var reader MessageHistoryReader = historyReaderFunc(func(c context.Context, _ uint64, id, _ string, _ int) (messaging.HistoryPage, error) {
				calls++
				p := messaging.HistoryPage{ConversationID: id, Items: []messaging.MessageDTO{}}
				switch mode {
				case "SQL":
					return p, errors.New("PRIVATE_SQL")
				case "forbidden":
					return p, database.ErrConversationForbidden
				case "joined_forbidden":
					return p, errors.Join(database.ErrConversationForbidden, errors.New("PRIVATE_RELEASE"))
				case "other_invalid":
					return p, messaging.ErrInvalidInput
				case "late":
					<-c.Done()
					return p, nil
				case "late_forbidden":
					<-c.Done()
					return p, database.ErrConversationForbidden
				case "zero_page":
					return messaging.HistoryPage{}, nil
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
				reader = historyReaderFunc(nil)
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
			w := historyTestRequest(t, reader, "conversationId=p:34:42", user, ctx, timeout)
			want, reason := 503, "DEPENDENCY_UNAVAILABLE"
			switch mode {
			case "missing_uid", "wrong_uid":
				want, reason = 401, "UNAUTHENTICATED"
			case "forbidden":
				want, reason = 403, "FORBIDDEN"
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
		if _, e := QueryMessageHistory(nil, d); !errors.Is(e, ErrMessageHistoryOptions) {
			t.Fatal(e)
		}
	}
}
