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

var ErrMessageSyncOptions = errors.New("invalid message sync options")

const DefaultSyncTimeout = 3 * time.Second
const MaxSyncQueryBytes = 1024

type MessageSyncReader interface {
	SyncMessages(context.Context, uint64, string, string, int) (messaging.SyncPage, error)
}

// QueryMessageSync owns the callback until it actually returns. Neither an
// expired observer nor a late nil error may release SQL or fabricate success.
func QueryMessageSync(reader MessageSyncReader, timeout time.Duration) (gin.HandlerFunc, error) {
	if timeout <= 0 || timeout > time.Minute {
		return nil, ErrMessageSyncOptions
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
			syncError(c, 503, "DEPENDENCY_UNAVAILABLE")
			return
		}
		query, err := messageSyncQuery(uint64(user), c.Request.URL.RawQuery)
		if err != nil {
			syncError(c, 400, "INVALID_REQUEST")
			return
		}
		if reader == nil || nilChatValue(reader) || ctx.Err() != nil {
			syncError(c, 503, "DEPENDENCY_UNAVAILABLE")
			return
		}
		cursor := ""
		if query.HasCursor {
			cursor = query.Cursor.String()
		}
		page, err := reader.SyncMessages(ctx, query.ReaderID, query.Conversation.ID, cursor, query.Limit)
		if err != nil || ctx.Err() != nil {
			switch {
			case ctx.Err() == nil && err == database.ErrConversationForbidden:
				syncError(c, 403, "FORBIDDEN")
			case ctx.Err() == nil && err == database.ErrSyncCursorAhead:
				syncError(c, 400, "INVALID_REQUEST")
			default:
				// Includes permission/range errors joined with SQL release failure.
				syncError(c, 503, "DEPENDENCY_UNAVAILABLE")
			}
			return
		}
		if page.ValidateFor(query) != nil || ctx.Err() != nil {
			syncError(c, 503, "DEPENDENCY_UNAVAILABLE")
			return
		}
		c.JSON(http.StatusOK, messaging.SyncResponse{Code: 0, Msg: "messages synced", Data: page})
	}, nil
}

func messageSyncQuery(user uint64, raw string) (messaging.PageQuery, error) {
	if len(raw) > MaxSyncQueryBytes {
		return messaging.PageQuery{}, messaging.ErrInvalidInput
	}
	params, err := url.ParseQuery(raw)
	if err != nil || len(params["conversationId"]) != 1 {
		return messaging.PageQuery{}, messaging.ErrInvalidInput
	}
	for key, values := range params {
		if (key != "conversationId" && key != "afterSeq" && key != "limit") || len(values) != 1 {
			return messaging.PageQuery{}, messaging.ErrInvalidInput
		}
	}
	if values, ok := params["afterSeq"]; ok && values[0] == "" {
		return messaging.PageQuery{}, messaging.ErrInvalidInput
	}
	limit := 0
	if values, ok := params["limit"]; ok {
		limit, err = strconv.Atoi(values[0])
		if err != nil || limit < 1 || strconv.Itoa(limit) != values[0] {
			return messaging.PageQuery{}, messaging.ErrInvalidInput
		}
	}
	return messaging.NormalizePageQuery(user, params.Get("conversationId"), params.Get("afterSeq"), limit)
}

func syncError(c *gin.Context, status int, reason string) {
	message := "message sync unavailable"
	if status == 400 {
		message = "invalid message sync query"
	} else if status == 403 {
		message = "message sync forbidden"
	}
	c.AbortWithStatusJSON(status, gin.H{"code": -1, "msg": message, "data": gin.H{"reasonCode": reason}})
}
