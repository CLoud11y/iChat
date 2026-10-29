package service

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/gin-gonic/gin"
	"iChat/messaging"
	"iChat/utils"
)

var ErrCommandStatusOptions = errors.New("invalid command status options")

const DefaultCommandStatusTimeout = 3 * time.Second
const MaxStatusQueryBytes = 1024

type CommandStatusReader interface {
	GetCommandStatus(context.Context, uint64, string, string) (messaging.CommandStatus, error)
}

// QueryCommandStatus is read-only: identity comes from trusted auth middleware,
// and no storage failure or incomplete adapter result can become a pending DTO.
// It owns the callback until it really returns; it never abandons a SQL task.
func QueryCommandStatus(reader CommandStatusReader, timeout time.Duration) (gin.HandlerFunc, error) {
	if timeout <= 0 || timeout > time.Minute {
		return nil, ErrCommandStatusOptions
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
			commandStatusError(c, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE")
			return
		}
		if len(c.Request.URL.RawQuery) > MaxStatusQueryBytes {
			commandStatusError(c, http.StatusBadRequest, "INVALID_REQUEST")
			return
		}
		params, err := url.ParseQuery(c.Request.URL.RawQuery)
		if err != nil || len(params) != 2 || len(params["clientMessageId"]) != 1 || len(params["payloadHash"]) != 1 {
			commandStatusError(c, http.StatusBadRequest, "INVALID_REQUEST")
			return
		}
		query, err := messaging.NormalizeStatusQuery(uint64(user), params.Get("clientMessageId"), params.Get("payloadHash"))
		if err != nil {
			commandStatusError(c, http.StatusBadRequest, "INVALID_REQUEST")
			return
		}
		if reader == nil || nilChatValue(reader) || ctx.Err() != nil {
			commandStatusError(c, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE")
			return
		}
		status, err := reader.GetCommandStatus(ctx, query.SenderID, query.ClientMessageID, query.PayloadHash)
		if err != nil || ctx.Err() != nil || status.ValidateFor(query) != nil {
			commandStatusError(c, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE")
			return
		}
		code, message, httpStatus := 0, "command status", http.StatusOK
		if status.State == messaging.StatusConflict {
			code, message, httpStatus = -1, "message ID conflict", http.StatusConflict
		}
		c.JSON(httpStatus, messaging.CommandStatusResponse{Code: code, Msg: message, Data: status})
	}, nil
}

func commandStatusError(c *gin.Context, status int, reason string) {
	message := "command status unavailable"
	if status == http.StatusBadRequest {
		message = "invalid command status query"
	}
	c.AbortWithStatusJSON(status, gin.H{"code": -1, "msg": message, "data": gin.H{"reasonCode": reason}})
}
