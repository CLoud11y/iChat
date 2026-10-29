package router

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"
	"iChat/middlewares"
	"iChat/service"
	"iChat/utils"
)

// RegisterCommandStatus shares one deadline across Redis auth and SQL. It does
// not gate authoritative reads on publisher or cached storage readiness.
func RegisterCommandStatus(r gin.IRouter, tokens *utils.TokenService, reader service.CommandStatusReader, timeout time.Duration) error {
	handler, err := service.QueryCommandStatus(reader, timeout)
	if err != nil {
		return err
	}
	r.GET("/auth/messages/status", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}, middlewares.JwtBearerAuth(tokens), handler)
	return nil
}
