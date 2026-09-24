package service

import (
	"context"
	"encoding/json"
	"fmt"
	"iChat/database"
	"iChat/models"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"net/http/httptest"
)

func TestMessageHistoryMedia(t *testing.T) {
	for _, media := range []struct {
		value uint
		name  string
	}{
		{models.TextMedia, "text"}, {models.PictureMedia, "picture"}, {models.AudioMedia, "audio"},
	} {
		t.Run(media.name, func(t *testing.T) {
			msg := models.Message{Id: "media", SenderId: 42, ReceiverId: 9, Type: models.PrivateType, Media: media.value, Content: "/original/reference"}
			raw, err := json.Marshal(msg)
			if err != nil {
				t.Fatal(err)
			}
			stub := &messageStub{rows: []string{string(raw)}}
			var rows []RespMsgInfo
			contactData(t, contactRequest(GetMessageList(stub), messageListBody, context.Background(), 42), &rows)
			if len(rows) != 1 || rows[0].Type != media.name || rows[0].Content != msg.Content {
				t.Fatalf("media response: %+v", rows)
			}
		})
	}
}

func TestMessageHistoryAuthorization(t *testing.T) {
	stub := &messageStub{err: fmt.Errorf("internal SQL: %w", database.ErrMessageHistoryForbidden)}
	response := contactRequest(GetMessageList(stub), `{"toContactId":9,"limit":10,"is_group":1,"uid":999}`, context.Background(), 42)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "GROUP_HISTORY_FORBIDDEN") || strings.Contains(response.Body.String(), "internal SQL") || stub.uid != 42 || stub.kind != models.GroupType {
		t.Fatalf("authorization response=%d %s args=%+v", response.Code, response.Body, stub)
	}
	for _, identity := range []any{nil, uint(0), 42, "42"} {
		stub := &messageStub{}
		r := gin.New()
		r.POST("/", func(c *gin.Context) {
			if identity != nil {
				c.Set("uid", identity)
			}
		}, GetMessageList(stub))
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{"))
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		r.ServeHTTP(response, req)
		if response.Code != http.StatusUnauthorized || stub.calls != 0 {
			t.Fatalf("identity=%v response=%d calls=%d", identity, response.Code, stub.calls)
		}
	}
}

func TestMessageHistoryInvalidMediaIsNotPartialSuccess(t *testing.T) {
	stub := &messageStub{rows: []string{`{"id":"partial","media":0}`, `{"id":"invalid","media":999}`}}
	response := contactRequest(GetMessageList(stub), messageListBody, context.Background(), 42)
	assertAccountUnavailable(t, response)
	if strings.Contains(response.Body.String(), "partial") {
		t.Fatal("partial history leaked")
	}
}
