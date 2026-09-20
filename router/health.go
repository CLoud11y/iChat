package router

import (
	"github.com/gin-gonic/gin"
	"net/http"
)

type StorageHealth interface{ StorageReady() bool }

func registerHealth(r *gin.Engine, storage StorageHealth) {
	r.GET("/health/live", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "live"})
	})
	r.GET("/health/ready/storage", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if storage == nil || !storage.StorageReady() {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready", "reasonCode": "STORAGE_UNAVAILABLE"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})
	r.GET("/health/ready/accept", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		// This endpoint describes the future confirmed MQ acceptance path.
		// Legacy Redis PING cannot establish that contract.
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready", "reasonCode": "MQ_ACCEPTANCE_NOT_CONFIGURED"})
	})
}
