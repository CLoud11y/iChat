package service

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"iChat/database"
	"iChat/messaging"
)

type conversationReaderFunc func(context.Context, uint64, string, int) (messaging.ConversationPage, error)

func (f conversationReaderFunc) ListConversations(c context.Context, u uint64, k string, n int) (messaging.ConversationPage, error) {
	return f(c, u, k, n)
}
func conversationsRequest(t *testing.T, reader ConversationReader, query string, user any, ctx context.Context, timeout time.Duration) *httptest.ResponseRecorder {
	t.Helper()
	handler, e := QueryConversations(reader, timeout)
	if e != nil {
		t.Fatal(e)
	}
	r := gin.New()
	r.GET("/list", func(c *gin.Context) {
		if user != nil {
			c.Set("uid", user)
		}
		c.Next()
	}, handler)
	req := httptest.NewRequest("GET", "/list?"+query, nil)
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("conversation metadata cached")
	}
	return w
}
func TestConversationsHTTPDefaultsAndPaging(t *testing.T) {
	for _, tc := range []struct {
		raw, cursor string
		limit       int
		empty       bool
	}{{"", "", 100, true}, {"cursor=", "", 100, true}, {"limit=2", "", 2, false}, {"cursor=g%3A1&limit=2", "g:1", 2, false}, {"cursor=p%3A1%3A999&limit=500", "p:1:999", 500, true}} {
		t.Run(tc.raw, func(t *testing.T) {
			want := messaging.ConversationPage{Items: []messaging.ConversationSummary{}, NextCursor: tc.cursor}
			if !tc.empty {
				want.Items = []messaging.ConversationSummary{{ConversationID: "g:10", LastSeq: messaging.Sequence(math.MaxUint64), DeliveredSeq: messaging.Sequence(math.MaxUint64)}, {ConversationID: "g:2", LastSeq: 2, DeliveredSeq: 0}}
				want.NextCursor = "g:2"
				want.HasMore = true
			}
			calls := 0
			reader := conversationReaderFunc(func(ctx context.Context, u uint64, c string, n int) (messaging.ConversationPage, error) {
				calls++
				if u != 42 || c != tc.cursor || n != tc.limit {
					t.Error("untrusted/wrong reader query", u, c, n)
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Error("missing read deadline")
				}
				return want, nil
			})
			w := conversationsRequest(t, reader, tc.raw, uint(42), nil, time.Second)
			if w.Code != 200 || calls != 1 {
				t.Fatal(w.Code, w.Body.String())
			}
			var got messaging.ConversationsResponse
			if e := json.Unmarshal(w.Body.Bytes(), &got); e != nil || got.Code != 0 || got.Msg != "conversations" {
				t.Fatal(e, w.Body.String())
			}
			b, _ := json.Marshal(want)
			actual, _ := json.Marshal(got.Data)
			if string(b) != string(actual) {
				t.Fatal(string(b), string(actual))
			}
			if tc.empty && !strings.Contains(w.Body.String(), `"items":[]`) {
				t.Fatal(w.Body.String())
			}
			if !tc.empty && !strings.Contains(w.Body.String(), `"lastSeq":"18446744073709551615"`) {
				t.Fatal("sequence precision lost")
			}
		})
	}
}
func TestConversationsHTTPRejectsMalformedQueries(t *testing.T) {
	for _, raw := range []string{"limit=0", "limit=", "limit=-1", "limit=501", "limit=01", "limit=%2B1", "limit=%201", "limit=1.0", "limit=1e2", "limit=999999999999999999999999", "limit=1&limit=1", "cursor=&cursor=", "cursor=g:01", "cursor=p:2:1", "cursor=g:18446744073709551616", "cursor=bad", "Cursor=g:1", "Limit=1", "senderId=1", "userId=1", "token=secret", "afterSeq=1", "payloadHash=secret", "%zz=x", "limit=1;cursor=g:1", "=x", strings.Repeat("x", MaxConversationsQueryBytes+1)} {
		calls := 0
		reader := conversationReaderFunc(func(context.Context, uint64, string, int) (messaging.ConversationPage, error) {
			calls++
			return messaging.ConversationPage{}, nil
		})
		w := conversationsRequest(t, reader, raw, uint(42), nil, time.Second)
		if w.Code != 400 || calls != 0 || !strings.Contains(w.Body.String(), "INVALID_REQUEST") {
			t.Fatal(raw, w.Code, w.Body.String())
		}
	}
}
func TestConversationsHTTPFailsClosed(t *testing.T) {
	for _, mode := range []string{"nil", "typed_nil", "no_uid", "wrong_uid", "canceled", "error", "forbidden", "joined_forbidden", "canceled_forbidden", "late", "zero", "nil_items", "wrong_owner", "bad_id", "ahead", "duplicate", "reverse", "at_cursor", "bad_next", "empty_next", "short_more", "too_many"} {
		t.Run(mode, func(t *testing.T) {
			raw := "limit=1"
			ctx := context.Background()
			user := any(uint(42))
			timeout := time.Second
			calls := 0
			var reader ConversationReader = conversationReaderFunc(func(c context.Context, _ uint64, _ string, _ int) (messaging.ConversationPage, error) {
				calls++
				p := messaging.ConversationPage{Items: []messaging.ConversationSummary{{ConversationID: "g:10", LastSeq: 2, DeliveredSeq: 1}}, NextCursor: "g:10"}
				switch mode {
				case "error":
					return p, errors.New("PRIVATE_READER_DIAGNOSTIC")
				case "forbidden":
					return p, database.ErrConversationForbidden
				case "joined_forbidden":
					return p, errors.Join(database.ErrConversationForbidden, errors.New("PRIVATE_RELEASE_FAILURE"))
				case "canceled_forbidden":
					<-c.Done()
					return p, database.ErrConversationForbidden
				case "late":
					<-c.Done()
					return p, nil
				case "zero":
					return messaging.ConversationPage{}, nil
				case "nil_items":
					p.Items = nil
					p.NextCursor = ""
				case "wrong_owner":
					p.Items[0].ConversationID = "p:1:2"
					p.NextCursor = "p:1:2"
				case "bad_id":
					p.Items[0].ConversationID = "g:01"
					p.NextCursor = "g:01"
				case "ahead":
					p.Items[0].DeliveredSeq = 3
				case "duplicate":
					p.Items = append(p.Items, p.Items[0])
				case "reverse":
					p.Items = append([]messaging.ConversationSummary{{ConversationID: "g:2"}}, p.Items...)
				case "at_cursor":
				case "bad_next":
					p.NextCursor = "g:2"
				case "empty_next":
					p.Items = []messaging.ConversationSummary{}
					p.NextCursor = "g:2"
				case "short_more":
					p.Items = []messaging.ConversationSummary{}
					p.NextCursor = ""
					p.HasMore = true
				case "too_many":
					p.Items = append(p.Items, messaging.ConversationSummary{ConversationID: "g:2"})
					p.NextCursor = "g:2"
				default:
					t.Error("unexpected reader call")
				}
				return p, nil
			})
			switch mode {
			case "nil":
				reader = nil
			case "typed_nil":
				reader = conversationReaderFunc(nil)
			case "no_uid":
				user = nil
			case "wrong_uid":
				user = "42"
			case "canceled":
				var stop context.CancelFunc
				ctx, stop = context.WithCancel(ctx)
				stop()
			case "late", "canceled_forbidden":
				timeout = 10 * time.Millisecond
			case "duplicate", "reverse":
				raw = "limit=2"
			case "at_cursor":
				raw = "cursor=g:10&limit=1"
			}
			w := conversationsRequest(t, reader, raw, user, ctx, timeout)
			want, reason := 503, "DEPENDENCY_UNAVAILABLE"
			if mode == "no_uid" || mode == "wrong_uid" {
				want, reason = 401, "UNAUTHENTICATED"
			}
			if mode == "forbidden" {
				want, reason = 403, "FORBIDDEN"
			}
			if w.Code != want || !strings.Contains(w.Body.String(), reason) || strings.Contains(w.Body.String(), "PRIVATE") || strings.Contains(w.Body.String(), `"items"`) || strings.Contains(w.Body.String(), `"state"`) {
				t.Fatal(mode, w.Code, w.Body.String())
			}
			if (mode == "no_uid" || mode == "wrong_uid" || mode == "canceled" || mode == "nil" || mode == "typed_nil") && calls != 0 {
				t.Fatal("invalid context/identity reached SQL")
			}
		})
	}
	for _, timeout := range []time.Duration{0, -1, time.Minute + 1} {
		if _, e := QueryConversations(nil, timeout); !errors.Is(e, ErrConversationsOptions) {
			t.Fatal(e)
		}
	}
}
