package middlewares

import (
	"iChat/utils"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

func Logger2File() gin.HandlerFunc {
	logger := utils.Logger()
	return func(c *gin.Context) {
		// 开始时间
		startTime := time.Now()
		// 处理请求
		c.Next()
		// 结束时间
		endTime := time.Now()
		// 执行时间
		latencyTime := endTime.Sub(startTime)
		// 请求方法
		reqMethod := c.Request.Method
		// 请求路由
		// Query parameters may contain JWTs or other secrets.
		reqPath := c.Request.URL.Path
		// 状态码
		statusCode := c.Writer.Status()
		// 请求IP
		clientIP := c.ClientIP()
		//日志格式
		entry := logger.WithFields(logrus.Fields{
			"status_code":  statusCode,
			"latency_time": latencyTime,
			"client_ip":    clientIP,
			"req_method":   reqMethod,
			"req_path":     reqPath,
		})
		if len(c.Errors) > 0 {
			entry = entry.WithField("errors", c.Errors.String())
		}
		switch {
		case statusCode >= 500:
			entry.Error("http request completed")
		case statusCode >= 400:
			entry.Warn("http request completed")
		default:
			entry.Info("http request completed")
		}
	}
}
