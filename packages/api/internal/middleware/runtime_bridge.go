package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func RuntimeBridgePOC() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := strings.Trim(c.Request.URL.Path, "/")
		parts := strings.Split(path, "/")
		method := c.Request.Method
		allowed := method == http.MethodGet && (path == "health" || path == "sandboxes" || path == "v2/sandboxes")
		allowed = allowed || method == http.MethodPost && path == "sandboxes"
		allowed = allowed || len(parts) == 2 && parts[0] == "sandboxes" && parts[1] != "metrics" && (method == http.MethodGet || method == http.MethodDelete)
		allowed = allowed || len(parts) == 3 && parts[0] == "sandboxes" && parts[2] == "timeout" && method == http.MethodPost
		if !allowed {
			c.AbortWithStatusJSON(http.StatusNotImplemented, gin.H{"code": http.StatusNotImplemented, "message": "This operation is unavailable in the runtime bridge POC"})

			return
		}
		c.Next()
	}
}
