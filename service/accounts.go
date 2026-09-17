package service

import (
	"context"
	"iChat/models"
	"net/http"

	"github.com/gin-gonic/gin"
)

// AccountUsers contains only the operations used by account HTTP handlers.
// Implementations must return an error rather than a nil user on success.
type AccountUsers interface {
	Signup(context.Context, string, string, string) (*models.User, error)
	GetUserByPhone(context.Context, string) (*models.User, error)
	Online(*models.User)
	Offline(uint)
}

func accountStorageUnavailable(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
		"code": -1, "msg": "storage dependency unavailable",
		"data": gin.H{"reasonCode": "DEPENDENCY_UNAVAILABLE"},
	})
}
