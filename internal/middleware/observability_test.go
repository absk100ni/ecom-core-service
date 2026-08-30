package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

func TestAdminAuthMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "test-secret-key"

	makeToken := func(claims jwt.MapClaims) string {
		token, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
		return token
	}

	tests := []struct {
		name       string
		token      string
		wantStatus int
	}{
		{
			"valid admin token",
			makeToken(jwt.MapClaims{"user_id": "admin1", "role": "admin", "exp": time.Now().Add(time.Hour).Unix()}),
			http.StatusOK,
		},
		{
			"user token without role claim",
			makeToken(jwt.MapClaims{"user_id": "user1", "is_admin": true, "exp": time.Now().Add(time.Hour).Unix()}),
			http.StatusForbidden,
		},
		{
			"user token with role=user",
			makeToken(jwt.MapClaims{"user_id": "user1", "role": "user", "exp": time.Now().Add(time.Hour).Unix()}),
			http.StatusForbidden,
		},
		{
			"expired admin token",
			makeToken(jwt.MapClaims{"user_id": "admin1", "role": "admin", "exp": time.Now().Add(-time.Hour).Unix()}),
			http.StatusUnauthorized,
		},
		{
			"no token",
			"",
			http.StatusUnauthorized,
		},
		{
			"invalid token",
			"not-a-valid-jwt",
			http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			_, r := gin.CreateTestContext(w)

			r.Use(AdminAuthMiddleware(secret))
			r.GET("/admin/test", func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"ok": true})
			})

			req, _ := http.NewRequest("GET", "/admin/test", nil)
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}
			r.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("status: got %d, want %d", w.Code, tt.wantStatus)
			}
		})
	}
}

func TestRequestIDMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	_, r := gin.CreateTestContext(w)

	r.Use(RequestIDMiddleware())
	r.GET("/test", func(c *gin.Context) {
		rid, _ := c.Get("request_id")
		c.JSON(http.StatusOK, gin.H{"request_id": rid})
	})

	req, _ := http.NewRequest("GET", "/test", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	if w.Header().Get("X-Request-ID") == "" {
		t.Error("X-Request-ID header not set")
	}
}
