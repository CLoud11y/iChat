package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iChat/database"
	"iChat/models"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

type messageStub struct {
	err               error
	ctx               context.Context
	msg               *models.Message
	rows              []string
	uid, target, kind uint
	cursor            models.Message
	limit, calls      int
}

func (s *messageStub) PublishAndSave(ctx context.Context, msg *models.Message) error {
	s.calls++
	s.ctx, s.msg = ctx, msg
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.err
}
func (s *messageStub) LoadMsgs(ctx context.Context, uid, target, kind uint, cursor models.Message, limit int) ([]string, error) {
	s.calls++
	s.ctx, s.uid, s.target, s.kind, s.cursor, s.limit = ctx, uid, target, kind, cursor, limit
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.rows, s.err
}

const sendMessageBody = `{"id":"m1","identifier":7,"sendTime":123,"toContactId":9,"fromUser":{"id":999},"type":"picture","content":"image","is_group":0}`
const messageListBody = `{"toContactId":9,"limit":10,"is_group":0,"uid":999}`

func TestMessageHTTPFailures(t *testing.T) {
	for _, action := range []string{"send", "history"} {
		for _, scenario := range []string{"missing", "nil_redis", "error", "canceled", "invalid_json"} {
			t.Run(action+"/"+scenario, func(t *testing.T) {
				stub := &messageStub{}
				var store MessageStore = stub
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				body := sendMessageBody
				if action == "history" {
					body = messageListBody
				}
				switch scenario {
				case "missing":
					store = nil
				case "nil_redis":
					store = database.NewMessageManager(nil, nil)
				case "error":
					stub.err = errors.New("private SQL detail")
				case "canceled":
					cancel()
				case "invalid_json":
					body = `{`
				}
				handler := SendMsg(store)
				if action == "history" {
					handler = GetMessageList(store)
				}
				response := contactRequest(handler, body, ctx, 42)
				if scenario == "invalid_json" {
					if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"code":-1`) || stub.calls != 0 {
						t.Fatalf("invalid binding: %s", response.Body)
					}
				} else {
					assertAccountUnavailable(t, response)
				}
				if scenario == "canceled" && stub.ctx != ctx {
					t.Fatal("context not forwarded")
				}
			})
		}
	}
}

func TestMessageHTTPInjectedSuccess(t *testing.T) {
	old := database.Mmanager
	database.Mmanager = nil
	t.Cleanup(func() { database.Mmanager = old })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stub := &messageStub{}
	response := contactRequest(SendMsg(stub), sendMessageBody, ctx, 42)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"code":0`) {
		t.Fatalf("send: %s", response.Body)
	}
	want := models.Message{Id: "m1", Identifier: 7, TimeStamp: 123, SenderId: 42, ReceiverId: 9, Type: models.PrivateType, Media: models.PictureMedia, Content: "image"}
	if stub.ctx != ctx || stub.msg == nil || *stub.msg != want {
		t.Fatalf("send args: %+v", stub.msg)
	}
	for _, empty := range []bool{false, true} {
		stub.rows = nil
		if !empty {
			for _, id := range []string{"new", "old"} {
				msg := want
				msg.Id = id
				raw, _ := json.Marshal(msg)
				stub.rows = append(stub.rows, string(raw))
			}
		}
		response = contactRequest(GetMessageList(stub), messageListBody, ctx, 42)
		var rows []RespMsgInfo
		total := contactData(t, response, &rows)
		if rows == nil || total != len(stub.rows) || len(rows) != total {
			t.Fatalf("history: %s", response.Body)
		}
		if !empty && (rows[0].Id != "old" || rows[1].Id != "new" || rows[0].FromUser.Id != 42 || rows[0].Content != "image") {
			t.Fatal("history mapping changed")
		}
		if stub.ctx != ctx || stub.uid != 42 || stub.target != 9 || stub.kind != models.PrivateType || stub.limit != 10 || stub.cursor.Type != models.InvalidType {
			t.Fatal("history arguments changed")
		}
	}
	cursor := models.MsgInfo{Id: "cursor", Identifier: 3, FromUser: models.FromUserInfo{Id: 8}, ToContactId: 9, Type: "audio", IsGroup: 1, SendTime: 123}
	body, _ := json.Marshal(ReqMsgInfo{TocontactId: 9, Limit: 2, IsGroup: 1, EarliestMsg: cursor})
	contactData(t, contactRequest(GetMessageList(stub), string(body), ctx, 42), &[]RespMsgInfo{})
	if stub.kind != models.GroupType || stub.cursor != *cursor.Conv2Msg() || stub.limit != 2 {
		t.Fatal("group/cursor arguments changed")
	}
}

func TestHistoryDecodeFailureIsNotPartialSuccess(t *testing.T) {
	stub := &messageStub{rows: []string{`{"id":"partial"}`, `{`}}
	response := contactRequest(GetMessageList(stub), messageListBody, context.Background(), 42)
	assertAccountUnavailable(t, response)
	if strings.Contains(response.Body.String(), "partial") {
		t.Fatal("partial history leaked")
	}
}

func TestMessageValidationAndBusinessErrors(t *testing.T) {
	for _, tc := range []struct {
		body string
		send bool
	}{
		{`{}`, true}, {`{"toContactId":9,"type":"unknown"}`, true}, {`{"toContactId":9,"type":"text","is_group":2}`, true},
		{`{}`, false}, {`{"toContactId":9,"limit":-1}`, false}, {`{"toContactId":9,"limit":101}`, false}, {`{"toContactId":9,"limit":10,"is_group":2}`, false},
	} {
		stub := &messageStub{}
		var handler gin.HandlerFunc = GetMessageList(stub)
		if tc.send {
			handler = SendMsg(stub)
		}
		response := contactRequest(handler, tc.body, context.Background(), 42)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"code":-1`) || stub.calls != 0 {
			t.Fatalf("invalid input: %s", response.Body)
		}
	}
	for _, err := range []error{database.ErrMessageCursorNotFound, database.ErrInvalidMessage} {
		stub := &messageStub{err: fmt.Errorf("private SQL: %w", err)}
		response := contactRequest(GetMessageList(stub), messageListBody, context.Background(), 42)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"code":-1`) || strings.Contains(response.Body.String(), "private SQL") {
			t.Fatalf("business: %s", response.Body)
		}
	}
}
