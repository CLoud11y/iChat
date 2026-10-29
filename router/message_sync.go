package router

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"
	"iChat/middlewares"
	"iChat/service"
	"iChat/utils"
)

func RegisterMessageSync(r gin.IRouter, tokens *utils.TokenService, reader service.MessageSyncReader, timeout time.Duration) error {
	handler, err := service.QueryMessageSync(reader, timeout)
	if err != nil {
		return err
	}
	r.GET("/auth/messages/sync", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}, middlewares.JwtBearerAuth(tokens), handler)
	return nil
}
