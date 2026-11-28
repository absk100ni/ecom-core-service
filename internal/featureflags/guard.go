package featureflags

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func (s *Service) RequireEnabled(key string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.Enabled(c.Request.Context(), key) {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "This feature is temporarily unavailable", "code": "EFLAG001", "feature": key})
			return
		}
		c.Next()
	}
}
