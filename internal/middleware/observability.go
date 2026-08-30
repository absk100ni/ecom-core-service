package middleware

import (
	"fmt"
	"net/http"
	"time"

	"ecom-core-service/internal/sentry"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// RequestIDMiddleware adds a unique X-Request-ID header to every request/response.
func RequestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := c.GetHeader("X-Request-ID")
		if requestID == "" {
			requestID = uuid.New().String()
		}
		c.Set("request_id", requestID)
		c.Header("X-Request-ID", requestID)
		c.Next()
	}
}

// AccessLogMiddleware logs method, path, status, latency, request_id, user_id for all requests.
// If a Sentry client is provided, 5xx responses are reported.
func AccessLogMiddleware() gin.HandlerFunc {
	return AccessLogMiddlewareWithSentry(nil)
}

// AccessLogMiddlewareWithSentry is AccessLogMiddleware with optional Sentry 5xx capture.
func AccessLogMiddlewareWithSentry(sentryClient *sentry.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		latency := time.Since(start).Milliseconds()
		requestID, _ := c.Get("request_id")
		userID := c.GetString("user_id")
		status := c.Writer.Status()
		log.Info("ACCESS", "Request completed",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", status,
			"latency_ms", latency,
			"request_id", requestID,
			"user_id", userID,
		)

		// Capture 5xx to Sentry (never 4xx)
		if sentryClient != nil && status >= 500 {
			reqID, _ := requestID.(string)
			sentryClient.CaptureError(
				fmt.Sprintf("%d %s %s", status, c.Request.Method, c.Request.URL.Path),
				map[string]string{
					"request_id": reqID,
					"path":       c.Request.URL.Path,
					"method":     c.Request.Method,
					"status":     fmt.Sprintf("%d", status),
				},
			)
		}
	}
}

// SentryRecoveryMiddleware recovers from panics, reports to Sentry, and returns 500 JSON.
// Must be used INSTEAD of gin.Recovery() when Sentry is enabled.
func SentryRecoveryMiddleware(sentryClient *sentry.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				requestID, _ := c.Get("request_id")
				reqID, _ := requestID.(string)
				msg := fmt.Sprintf("%v", r)
				log.Error("PANIC", "Recovered from panic",
					"panic", msg,
					"path", c.Request.URL.Path,
					"request_id", reqID,
				)
				if sentryClient != nil {
					sentryClient.CapturePanic(r, map[string]string{
						"request_id": reqID,
						"path":       c.Request.URL.Path,
						"method":     c.Request.Method,
						"status":     "500",
					})
				}
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
					"error": "Internal server error",
				})
			}
		}()
		c.Next()
	}
}

// AdminAuthMiddleware requires a JWT with role=admin claim. Rejects user tokens.
func AdminAuthMiddleware(secret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		// The JWT must already be parsed by AuthMiddleware or extracted here.
		// We re-parse to check the role claim specifically.
		tokenStr := ""
		authHeader := c.GetHeader("Authorization")
		if len(authHeader) > 7 && authHeader[:7] == "Bearer " {
			tokenStr = authHeader[7:]
		}
		if tokenStr == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Authorization required"})
			return
		}

		token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
			return []byte(secret), nil
		})
		if err != nil || !token.Valid {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
			return
		}

		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Invalid token claims"})
			return
		}

		role, _ := claims["role"].(string)
		if role != "admin" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Admin token required", "code": "EADM002"})
			return
		}

		// Set context values from admin JWT
		if uid, ok := claims["user_id"].(string); ok {
			c.Set("user_id", uid)
		}
		if email, ok := claims["email"].(string); ok {
			c.Set("email", email)
		}
		c.Set("is_admin", true)
		c.Next()
	}
}
