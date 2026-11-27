package middleware

import (
	"net/http"

	"ecom-core-service/pkg/errcodes"

	"github.com/gin-gonic/gin"
)

// RequireCheckoutEnabled blocks only the creation of new orders and payment
// attempts. Existing-order tracking, payment verification, provider webhooks,
// refunds and administration remain available during an incident.
func RequireCheckoutEnabled(enabled bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !enabled {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
				"error": errcodes.ECheckoutDisabled.Message,
				"code":  errcodes.ECheckoutDisabled.Code,
			})
			return
		}
		c.Next()
	}
}
