package middlewares

import (
	"iChat/utils"

	"github.com/gin-gonic/gin"
)

func JwtAuth(tokens *utils.TokenService) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, err := utils.ExtractToken(c)
		if err != nil {
			utils.RespondAuthError(c, err)
			return
		}
		identity, err := tokens.ValidateToken(c.Request.Context(), token)
		if err != nil {
			utils.RespondAuthError(c, err)
			return
		}
		c.Set("uid", identity.UserID)
		c.Next()
	}
}
