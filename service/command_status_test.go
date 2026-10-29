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
	"iChat/messaging"
)

type statusReaderFunc func(context.Context, uint64, string, string) (messaging.CommandStatus, error)

func (f statusReaderFunc) GetCommandStatus(c context.Context, u uint64, k, h string) (messaging.CommandStatus, error) {
	return f(c, u, k, h)
}

func statusFixture(t *testing.T) (messaging.Command, messaging.MessageDTO) {
	t.Helper()
	c, e := messaging.Normalize(42, messaging.SendRequest{ClientMessageID: "8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1", TargetType: messaging.Private, TargetID: "34", Media: messaging.Text, Content: "PRIVATE_STATUS_MESSAGE"})
	if e != nil {
		t.Fatal(e)
	}
	m, e := messaging.NewMessageDTO(c, messaging.Sequence(math.MaxUint64), time.Date(2026, 10, 29, 0, 0, 0, 123456000, time.UTC))
	if e != nil {
		t.Fatal(e)
	}
	return c, m
}
func statusTestRequest(t *testing.T, reader CommandStatusReader, query string, uid any, ctx context.Context, timeout time.Duration) *httptest.ResponseRecorder {
	t.Helper()
	handler, e := QueryCommandStatus(reader, timeout)
	if e != nil {
		t.Fatal(e)
	}
	r := gin.New()
	r.GET("/status", func(c *gin.Context) {
		if uid != nil {
			c.Set("uid", uid)
		}
		c.Next()
	}, handler)
	request := httptest.NewRequest("GET", "/status?"+query, nil)
	if ctx != nil {
		request = request.WithContext(ctx)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, request)
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("cache allowed")
	}
	return w
}
func TestCommandStatusHTTPContract(t *testing.T) {
	command, message := statusFixture(t)
	query := "clientMessageId=" + strings.ToUpper(command.ClientMessageID) + "&payloadHash=" + command.PayloadHash
	for _, state := range []messaging.StatusState{messaging.StatusPersisted, messaging.StatusRejected, messaging.StatusPendingOrUnknown, messaging.StatusQuarantined, messaging.StatusConflict} {
		t.Run(string(state), func(t *testing.T) {
			want := messaging.CommandStatus{ClientMessageID: command.ClientMessageID, PayloadHash: command.PayloadHash, State: state}
			switch state {
			case messaging.StatusPersisted:
				want.Message = &message
			case messaging.StatusRejected:
				want.ReasonCode = "NOT_FRIENDS"
			case messaging.StatusPendingOrUnknown, messaging.StatusQuarantined:
				want.PollAfterMs = 2000
			}
			calls := 0
			reader := statusReaderFunc(func(ctx context.Context, u uint64, k, h string) (messaging.CommandStatus, error) {
				calls++
				if u != 42 || k != command.ClientMessageID || h != command.PayloadHash {
					t.Error("wrong trusted identity")
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Error("missing deadline")
				}
				return want, nil
			})
			w := statusTestRequest(t, reader, query, uint(42), nil, time.Second)
			expected := 200
			code := 0
			if state == messaging.StatusConflict {
				expected, code = 409, -1
			}
			if w.Code != expected {
				t.Fatal(w.Code, w.Body.String())
			}
			var got messaging.CommandStatusResponse
			if e := json.Unmarshal(w.Body.Bytes(), &got); e != nil || got.Code != code || calls != 1 {
				t.Fatal(got, e)
			}
			wantJSON, _ := json.Marshal(want)
			gotJSON, _ := json.Marshal(got.Data)
			if string(wantJSON) != string(gotJSON) {
				t.Fatal(w.Body.String())
			}
			if state != messaging.StatusPersisted && strings.Contains(w.Body.String(), "PRIVATE") {
				t.Fatal("content leaked")
			}
			if state == messaging.StatusPersisted && !strings.Contains(w.Body.String(), `"seq":"18446744073709551615"`) {
				t.Fatal("sequence precision lost")
			}
		})
	}
}
func TestCommandStatusHTTPMalformedQueries(t *testing.T) {
	c, _ := statusFixture(t)
	valid := "clientMessageId=" + c.ClientMessageID + "&payloadHash=" + c.PayloadHash
	queries := []string{"", "clientMessageId=" + c.ClientMessageID, "payloadHash=" + c.PayloadHash, valid + "&senderId=1", valid + "&userId=1", valid + "&token=secret", valid + "&clientMessageId=" + c.ClientMessageID, valid + "&payloadHash=" + c.PayloadHash, strings.Replace(valid, "clientMessageId", "ClientMessageId", 1), strings.Replace(valid, c.PayloadHash, strings.ToUpper(c.PayloadHash), 1), strings.Replace(valid, c.PayloadHash, "", 1), strings.Replace(valid, c.ClientMessageID, "null", 1), valid + "&%zz=x", valid + ";extra=x", valid + strings.Repeat("x", MaxStatusQueryBytes)}
	for _, q := range queries {
		calls := 0
		reader := statusReaderFunc(func(context.Context, uint64, string, string) (messaging.CommandStatus, error) {
			calls++
			return messaging.CommandStatus{}, nil
		})
		w := statusTestRequest(t, reader, q, uint(42), nil, time.Second)
		if w.Code != 400 || calls != 0 || !strings.Contains(w.Body.String(), "INVALID_REQUEST") {
			t.Fatal("invalid query reached store", q, w.Code, w.Body.String())
		}
	}
}
func TestCommandStatusHTTPFailsClosed(t *testing.T) {
	c, m := statusFixture(t)
	query := "clientMessageId=" + c.ClientMessageID + "&payloadHash=" + c.PayloadHash
	for _, mode := range []string{"error_with_content", "zero_result", "cross_key", "cross_hash", "wrong_sender", "wrong_content", "unknown_state", "conflict_content", "conflict_reason", "pending_content", "pending_poll", "rejected_reason", "persisted_poll", "deadline", "late", "nil", "typed_nil", "canceled", "no_uid", "wrong_uid_type"} {
		t.Run(mode, func(t *testing.T) {
			uid := any(uint(42))
			timeout := time.Second
			ctx := context.Background()
			var reader CommandStatusReader
			reader = statusReaderFunc(func(ctx context.Context, _ uint64, _ string, _ string) (messaging.CommandStatus, error) {
				s := messaging.CommandStatus{ClientMessageID: c.ClientMessageID, PayloadHash: c.PayloadHash, State: messaging.StatusPersisted, Message: &m}
				switch mode {
				case "error_with_content":
					return s, errors.New("PRIVATE_SQL_DIAGNOSTIC")
				case "zero_result":
					return messaging.CommandStatus{}, nil
				case "cross_key":
					s.ClientMessageID = "other"
				case "cross_hash":
					s.PayloadHash = strings.Repeat("a", 64)
				case "wrong_sender":
					copy := m
					copy.SenderID = "1"
					s.Message = &copy
				case "wrong_content":
					copy := m
					copy.Content = "other"
					s.Message = &copy
				case "unknown_state":
					s.State = "accepted"
				case "conflict_content":
					s.State = messaging.StatusConflict
				case "conflict_reason":
					s.State = messaging.StatusConflict
					s.Message = nil
					s.ReasonCode = "PRIVATE_REASON"
				case "pending_content":
					s.State = messaging.StatusPendingOrUnknown
					s.PollAfterMs = 2000
				case "pending_poll":
					s.State = messaging.StatusPendingOrUnknown
					s.Message = nil
				case "rejected_reason":
					s.State = messaging.StatusRejected
					s.Message = nil
					s.ReasonCode = "PRIVATE_REASON"
				case "persisted_poll":
					s.PollAfterMs = 2000
				case "deadline":
					<-ctx.Done()
					return s, ctx.Err()
				case "late":
					<-ctx.Done()
					return s, nil
				default:
					t.Error("unexpected reader call")
				}
				return s, nil
			})
			switch mode {
			case "deadline", "late":
				timeout = 10 * time.Millisecond
			case "nil":
				reader = nil
			case "typed_nil":
				reader = statusReaderFunc(nil)
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "no_uid":
				uid = nil
			case "wrong_uid_type":
				uid = "42"
			}
			w := statusTestRequest(t, reader, query, uid, ctx, timeout)
			want, reason := 503, "DEPENDENCY_UNAVAILABLE"
			if mode == "no_uid" || mode == "wrong_uid_type" {
				want, reason = 401, "UNAUTHENTICATED"
			}
			if w.Code != want || !strings.Contains(w.Body.String(), reason) || strings.Contains(w.Body.String(), "PRIVATE") || strings.Contains(w.Body.String(), `"state"`) {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
	for _, timeout := range []time.Duration{0, -1, time.Minute + 1} {
		if _, e := QueryCommandStatus(nil, timeout); e == nil {
			t.Fatal("invalid timeout accepted")
		}
	}
}
