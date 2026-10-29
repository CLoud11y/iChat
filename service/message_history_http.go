package service

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"iChat/database"
	"iChat/messaging"
	"iChat/utils"
)

var ErrMessageHistoryOptions = errors.New("invalid message history options")

const DefaultHistoryTimeout = 3 * time.Second
const MaxHistoryQueryBytes = 1024

type MessageHistoryReader interface {
	HistoryMessages(context.Context, uint64, string, string, int) (messaging.HistoryPage, error)
}

// QueryMessageHistory owns the callback until it actually returns. Neither an
// expired observer nor a late nil error may release SQL or fabricate success.
func QueryMessageHistory(reader MessageHistoryReader, timeout time.Duration) (gin.HandlerFunc, error) {
	if timeout <= 0 || timeout > time.Minute {
		return nil, ErrMessageHistoryOptions
	}
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		user := c.GetUint("uid")
		if user == 0 {
			utils.RespondAuthError(c, utils.ErrUnauthenticated)
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
		defer cancel()
		if ctx.Err() != nil {
			historyError(c, 503, "DEPENDENCY_UNAVAILABLE")
			return
		}
		query, err := messageHistoryQuery(uint64(user), c.Request.URL.RawQuery)
		if err != nil {
			historyError(c, 400, "INVALID_REQUEST")
			return
		}
		if reader == nil || nilChatValue(reader) || ctx.Err() != nil {
			historyError(c, 503, "DEPENDENCY_UNAVAILABLE")
			return
		}
		cursor := ""
		if query.HasCursor {
			cursor = query.Cursor.String()
		}
		page, err := reader.HistoryMessages(ctx, query.ReaderID, query.Conversation.ID, cursor, query.Limit)
		if err != nil || ctx.Err() != nil {
			switch {
			case ctx.Err() == nil && err == database.ErrConversationForbidden:
				historyError(c, 403, "FORBIDDEN")
			default:
				// Includes permission errors joined with SQL release failure.
				historyError(c, 503, "DEPENDENCY_UNAVAILABLE")
			}
			return
		}
		if page.ValidateFor(query) != nil || ctx.Err() != nil {
			historyError(c, 503, "DEPENDENCY_UNAVAILABLE")
			return
		}
		c.JSON(http.StatusOK, messaging.HistoryResponse{Code: 0, Msg: "message history loaded", Data: page})
	}, nil
}

func messageHistoryQuery(user uint64, raw string) (messaging.PageQuery, error) {
	if len(raw) > MaxHistoryQueryBytes {
		return messaging.PageQuery{}, messaging.ErrInvalidInput
	}
	params, err := url.ParseQuery(raw)
	if err != nil || len(params["conversationId"]) != 1 {
		return messaging.PageQuery{}, messaging.ErrInvalidInput
	}
	for key, values := range params {
		if (key != "conversationId" && key != "beforeSeq" && key != "limit") || len(values) != 1 {
			return messaging.PageQuery{}, messaging.ErrInvalidInput
		}
	}
	if values, ok := params["beforeSeq"]; ok && values[0] == "" {
		return messaging.PageQuery{}, messaging.ErrInvalidInput
	}
	limit := 0
	if values, ok := params["limit"]; ok {
		limit, err = strconv.Atoi(values[0])
		if err != nil || limit < 1 || strconv.Itoa(limit) != values[0] {
			return messaging.PageQuery{}, messaging.ErrInvalidInput
		}
	}
	return messaging.NormalizePageQuery(user, params.Get("conversationId"), params.Get("beforeSeq"), limit)
}

func historyError(c *gin.Context, status int, reason string) {
	message := "message history unavailable"
	if status == 400 {
		message = "invalid message history query"
	} else if status == 403 {
		message = "message history forbidden"
	}
	c.AbortWithStatusJSON(status, gin.H{"code": -1, "msg": message, "data": gin.H{"reasonCode": reason}})
}
