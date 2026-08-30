package middleware

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"ecom-core-service/pkg/logger"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/time/rate"
)

var log = logger.New("MIDDLEWARE", "AUTH")

type Claims struct {
	UserID  string `json:"user_id"`
	Phone   string `json:"phone"`
	IsAdmin bool   `json:"is_admin"`
	jwt.RegisteredClaims
}

func GenerateToken(userID, phone, secret string, isAdmin bool) (string, error) {
	claims := Claims{
		UserID: userID, Phone: phone, IsAdmin: isAdmin,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(72 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
}

func AuthMiddleware(secret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Authorization required"})
			return
		}
		tokenStr := strings.TrimPrefix(authHeader, "Bearer ")
		if tokenStr == authHeader {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid token format"})
			return
		}
		claims := &Claims{}
		token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
			return []byte(secret), nil
		})
		if err != nil || !token.Valid {
			log.Warn("AuthMiddleware", "Invalid or expired token", "ip", c.ClientIP())
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired token"})
			return
		}
		c.Set("user_id", claims.UserID)
		c.Set("phone", claims.Phone)
		c.Set("is_admin", claims.IsAdmin)
		c.Next()
	}
}

func AdminMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		isAdmin, exists := c.Get("is_admin")
		if !exists || !isAdmin.(bool) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Admin access required"})
			return
		}
		c.Next()
	}
}

// rateLimiter is the subset of pkg/cache.Cache the middleware needs. Declaring it as an
// interface here keeps the middleware package free of a direct cache/redis dependency and
// makes the limiter trivially testable.
type rateLimiter interface {
	RateAllow(ctx context.Context, key string, limit int, window time.Duration) (allowed bool, counted bool)
}

// RateLimitMiddleware limits requests per client IP to maxReq per window.
//
// When a shared limiter (Redis) is provided it enforces the limit consistently across all
// app instances — the prerequisite for running more than one replica. If Redis is absent
// or unreachable it transparently falls back to a per-process in-memory token-bucket, so a
// single instance (or a Redis outage) still gets local protection rather than failing open.
func RateLimitMiddleware(shared rateLimiter, maxReq int, window time.Duration) gin.HandlerFunc {
	type client struct {
		limiter  *rate.Limiter
		lastSeen time.Time
	}
	var mu sync.Mutex
	clients := make(map[string]*client)
	go func() {
		for {
			time.Sleep(time.Minute)
			mu.Lock()
			for ip, c := range clients {
				if time.Since(c.lastSeen) > 3*time.Minute {
					delete(clients, ip)
				}
			}
			mu.Unlock()
		}
	}()

	localAllow := func(ip string) bool {
		mu.Lock()
		if _, ok := clients[ip]; !ok {
			clients[ip] = &client{limiter: rate.NewLimiter(rate.Every(window/time.Duration(maxReq)), maxReq)}
		}
		clients[ip].lastSeen = time.Now()
		l := clients[ip].limiter
		mu.Unlock()
		return l.Allow()
	}

	return func(c *gin.Context) {
		ip := c.ClientIP()

		allowed := true
		if shared != nil {
			ok, counted := shared.RateAllow(c.Request.Context(), "ratelimit:"+ip, maxReq, window)
			if counted {
				allowed = ok
			} else {
				allowed = localAllow(ip) // Redis unavailable — fall back to local bucket
			}
		} else {
			allowed = localAllow(ip)
		}

		if !allowed {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "Rate limit exceeded"})
			return
		}
		c.Next()
	}
}
