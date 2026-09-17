package utils

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// ExtractToken preserves the existing header, query and form token transports.
func ExtractToken(c *gin.Context) (string, error) {
	if header := c.GetHeader("Authorization"); header != "" {
		parts := strings.Fields(header)
		if len(parts) == 1 {
			return parts[0], nil
		}
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			return parts[1], nil
		}
		return "", ErrUnauthenticated
	}
	if token := c.Query("token"); token != "" {
		return token, nil
	}
	if token := c.Request.FormValue("token"); token != "" {
		return token, nil
	}
	return "", ErrUnauthenticated
}
