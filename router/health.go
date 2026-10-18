package router

import (
	"net/http"

	"iChat/health"

	"github.com/gin-gonic/gin"
)

type StorageHealth interface{ StorageReady() bool }

func registerHealth(r *gin.Engine, storage StorageHealth) {
	// The legacy Router cannot opt itself into confirmed-MQ acceptance.
	RegisterHealth(r, storage, nil)
}

// RegisterHealth lets an explicitly assembled modern API attach its observed
// runtime health. The legacy Router always supplies nil. Providers must perform
// cached, nonblocking reads; health requests do not probe dependencies.
func RegisterHealth(r *gin.Engine, storage StorageHealth, runtime health.RuntimeHealth) {
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
		state := health.ObserveRuntime(runtime).Acceptance
		respondReadiness(c, state, false)
	})
	r.GET("/health/ready/consumers", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		state := health.ObserveRuntime(runtime).Consumers
		respondReadiness(c, state, true)
	})
}

func respondReadiness(c *gin.Context, state health.ReadinessState, consumers bool) {
	if state == health.Ready {
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
		return
	}
	code := state.ReasonCode()
	if consumers && state == health.NotConfigured {
		code = "CONSUMERS_NOT_CONFIGURED"
	}
	c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready", "reasonCode": code})
}
