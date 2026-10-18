package router

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"
	"iChat/middlewares"
	"iChat/service"
	"iChat/utils"
)

// RegisterSubmission attaches only the modern send route to a modern engine.
// Do not combine it with Router's legacy routes at the same path. It creates no
// listener, business worker, store or publisher, and does not set health ready.
func RegisterSubmission(r gin.IRouter, tokens *utils.TokenService, publisher service.CommandPublisher, timeout time.Duration) error {
	handler, err := service.SubmitCommand(publisher, timeout)
	if err != nil {
		return err
	}
	r.POST("/auth/sendMessage", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}, middlewares.JwtBearerAuth(tokens), handler)
	return nil
}
