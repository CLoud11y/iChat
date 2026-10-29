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

var ErrConversationsOptions = errors.New("invalid conversations options")

const DefaultConversationsTimeout = 3 * time.Second
const MaxConversationsQueryBytes = 1024

type ConversationReader interface {
	ListConversations(context.Context, uint64, string, int) (messaging.ConversationPage, error)
}

// QueryConversations retains ownership of its reader until the callback really
// returns. Auth middleware supplies the UID; query data cannot select a user.
func QueryConversations(reader ConversationReader, timeout time.Duration) (gin.HandlerFunc, error) {
	if timeout <= 0 || timeout > time.Minute {
		return nil, ErrConversationsOptions
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
			conversationsError(c, 503, "DEPENDENCY_UNAVAILABLE")
			return
		}
		query, err := conversationQuery(uint64(user), c.Request.URL.RawQuery)
		if err != nil {
			conversationsError(c, 400, "INVALID_REQUEST")
			return
		}
		if reader == nil || nilChatValue(reader) || ctx.Err() != nil {
			conversationsError(c, 503, "DEPENDENCY_UNAVAILABLE")
			return
		}
		page, err := reader.ListConversations(ctx, query.ReaderID, query.Cursor, query.Limit)
		if err != nil || ctx.Err() != nil {
			// A joined permission/SQL-release error is a dependency failure.
			// Only the repository's pure, completed permission denial is 403.
			if err == database.ErrConversationForbidden && ctx.Err() == nil {
				conversationsError(c, 403, "FORBIDDEN")
			} else {
				conversationsError(c, 503, "DEPENDENCY_UNAVAILABLE")
			}
			return
		}
		if page.ValidateFor(query) != nil || ctx.Err() != nil {
			conversationsError(c, 503, "DEPENDENCY_UNAVAILABLE")
			return
		}
		c.JSON(http.StatusOK, messaging.ConversationsResponse{Code: 0, Msg: "conversations", Data: page})
	}, nil
}

func conversationQuery(user uint64, raw string) (messaging.ConversationListQuery, error) {
	if len(raw) > MaxConversationsQueryBytes {
		return messaging.ConversationListQuery{}, messaging.ErrInvalidInput
	}
	params, err := url.ParseQuery(raw)
	if err != nil {
		return messaging.ConversationListQuery{}, messaging.ErrInvalidInput
	}
	for key, values := range params {
		if (key != "cursor" && key != "limit") || len(values) != 1 {
			return messaging.ConversationListQuery{}, messaging.ErrInvalidInput
		}
	}
	limit := 0
	if values, ok := params["limit"]; ok {
		limit, err = strconv.Atoi(values[0])
		if err != nil || limit < 1 || strconv.Itoa(limit) != values[0] {
			return messaging.ConversationListQuery{}, messaging.ErrInvalidInput
		}
	}
	return messaging.NormalizeConversationListQuery(user, params.Get("cursor"), limit)
}

func conversationsError(c *gin.Context, status int, reason string) {
	message := "conversations unavailable"
	if status == 400 {
		message = "invalid conversations query"
	} else if status == 403 {
		message = "conversations forbidden"
	}
	c.AbortWithStatusJSON(status, gin.H{"code": -1, "msg": message, "data": gin.H{"reasonCode": reason}})
}
