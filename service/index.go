package service

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// @BasePath /api
// @Summary index
// @Description index page
// @Tags example
// @Accept json
// @Produce json
// @Success 200 {string} string “ok”
// @Router /api/index [get]
func Index(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"message": "this is an index",
	})
}
