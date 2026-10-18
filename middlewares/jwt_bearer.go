package middlewares

import (
	"strings"

	"github.com/gin-gonic/gin"
	"iChat/utils"
)

// JwtBearerAuth never parses a form or query token. In particular, missing
// headers cannot cause FormValue to read an unbounded body before validation.
// Legacy JwtAuth keeps its existing transports for its existing routes.
func JwtBearerAuth(tokens *utils.TokenService) gin.HandlerFunc {
	return func(c *gin.Context) {
		parts := strings.Fields(c.GetHeader("Authorization"))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			utils.RespondAuthError(c, utils.ErrUnauthenticated)
			return
		}
		identity, err := tokens.ValidateToken(c.Request.Context(), parts[1])
		if err != nil {
			utils.RespondAuthError(c, err)
			return
		}
		c.Set("uid", identity.UserID)
		c.Next()
	}
}
