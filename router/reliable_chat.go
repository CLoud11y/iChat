package router

import (
	"context"
	"strings"

	"github.com/gin-gonic/gin"
	"iChat/service"
	"iChat/utils"
)

// RegisterReliableChat belongs only on the modern API engine. The bearer token
// is copied once, checked before SQL/subscriptions, and rechecked on refresh.
func RegisterReliableChat(r gin.IRouter, tokens *utils.TokenService, chat *service.ReliableChat) error {
	if r == nil || tokens == nil || chat == nil {
		return service.ErrReliableChatInput
	}
	r.GET("/auth/chat", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		parts := strings.Fields(c.GetHeader("Authorization"))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			utils.RespondAuthError(c, utils.ErrUnauthenticated)
			return
		}
		token := parts[1]
		chat.Serve(c, func(ctx context.Context) (uint64, error) {
			id, err := tokens.ValidateToken(ctx, token)
			if err != nil {
				return 0, err
			}
			return uint64(id.UserID), nil
		})
	})
	return nil
}
