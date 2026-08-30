package admin

import (
	"context"
	"net/http"
	"time"

	"ecom-core-service/internal/config"
	"ecom-core-service/internal/models"
	"ecom-core-service/pkg/cache"
	"ecom-core-service/pkg/errcodes"
	"ecom-core-service/pkg/logger"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"golang.org/x/crypto/bcrypt"
)

var log = logger.New("ADMIN", "AUTH")

// Handler provides admin-specific endpoints (auth + refactored inline handlers).
type Handler struct {
	db    *mongo.Database
	cfg   *config.Config
	cache *cache.Cache
}

func NewHandler(db *mongo.Database, cfg *config.Config, c *cache.Cache) *Handler {
	return &Handler{db: db, cfg: cfg, cache: c}
}

// SeedAdmin creates a default admin if the admins collection is empty and env vars are set.
func (h *Handler) SeedAdmin() {
	if h.cfg.AdminEmail == "" || h.cfg.AdminPassword == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	count, _ := h.db.Collection("admins").CountDocuments(ctx, bson.M{})
	if count > 0 {
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(h.cfg.AdminPassword), bcrypt.DefaultCost)
	if err != nil {
		log.Error("SEED", "Failed to hash admin password", "err", err)
		return
	}

	admin := models.Admin{
		ID:           uuid.New().String(),
		Email:        h.cfg.AdminEmail,
		Name:         "Admin",
		PasswordHash: string(hash),
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
	if _, err := h.db.Collection("admins").InsertOne(ctx, admin); err != nil {
		log.Error("SEED", "Failed to seed admin", "err", err)
		return
	}
	log.Warn("SEED", "Default admin seeded — CHANGE THE PASSWORD", "email", h.cfg.AdminEmail)
}

// Login — POST /api/v1/admin/auth/login
func (h *Handler) Login(c *gin.Context) {
	ip := c.ClientIP()

	// Rate limit: 5 attempts per minute per IP
	if h.cache != nil {
		allowed, counted := h.cache.RateAllow(c.Request.Context(), "admin_login:"+ip, 5, time.Minute)
		if counted && !allowed {
			log.WarnWithCode("LOGIN", errcodes.EAdmRateLimited.Code, "Admin login rate limited", "ip", ip)
			c.JSON(http.StatusTooManyRequests, gin.H{"error": errcodes.EAdmRateLimited.Message, "code": errcodes.EAdmRateLimited.Code})
			return
		}
	}

	var req struct {
		Email    string `json:"email" binding:"required"`
		Password string `json:"password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Email and password required"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var admin models.Admin
	if err := h.db.Collection("admins").FindOne(ctx, bson.M{"email": req.Email}).Decode(&admin); err != nil {
		log.WarnWithCode("LOGIN", errcodes.EAdmInvalidCreds.Code, "Admin not found", "email", req.Email)
		c.JSON(http.StatusUnauthorized, gin.H{"error": errcodes.EAdmInvalidCreds.Message, "code": errcodes.EAdmInvalidCreds.Code})
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(admin.PasswordHash), []byte(req.Password)); err != nil {
		log.WarnWithCode("LOGIN", errcodes.EAdmInvalidCreds.Code, "Invalid admin password", "email", req.Email)
		c.JSON(http.StatusUnauthorized, gin.H{"error": errcodes.EAdmInvalidCreds.Message, "code": errcodes.EAdmInvalidCreds.Code})
		return
	}

	// Generate admin JWT with role=admin claim
	claims := jwt.MapClaims{
		"user_id": admin.ID,
		"email":   admin.Email,
		"role":    "admin",
		"exp":     time.Now().Add(24 * time.Hour).Unix(),
		"iat":     time.Now().Unix(),
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(h.cfg.JWTSecret))
	if err != nil {
		log.Error("LOGIN", "Token generation failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Authentication failed"})
		return
	}

	log.Info("LOGIN", "Admin authenticated", "email", admin.Email)
	c.JSON(http.StatusOK, gin.H{
		"token": token,
		"admin": gin.H{"email": admin.Email, "name": admin.Name},
	})
}

// ListUsers — GET /api/v1/admin/users (refactored from inline handler)
func (h *Handler) ListUsers(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cursor, err := h.db.Collection("users").Find(ctx, bson.M{})
	if err != nil {
		log.ErrorWithCode("USERS", errcodes.EDBQueryFailed.Code, "Failed to list users", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list users", "code": errcodes.EDBQueryFailed.Code})
		return
	}
	defer cursor.Close(ctx)

	var users []map[string]interface{}
	if err := cursor.All(ctx, &users); err != nil {
		log.ErrorWithCode("USERS", errcodes.EDBQueryFailed.Code, "Cursor decode failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to decode users", "code": errcodes.EDBQueryFailed.Code})
		return
	}
	if users == nil {
		users = []map[string]interface{}{}
	}
	c.JSON(http.StatusOK, gin.H{"users": users, "total": len(users)})
}

// Stats — GET /api/v1/admin/stats (refactored from inline handler)
func (h *Handler) Stats(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userCount, _ := h.db.Collection("users").CountDocuments(ctx, bson.M{})
	orderCount, _ := h.db.Collection("orders").CountDocuments(ctx, bson.M{})
	productCount, _ := h.db.Collection("products").CountDocuments(ctx, bson.M{"is_active": true})

	totalRevenue := 0
	revCursor, err := h.db.Collection("orders").Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.M{"payment_status": "paid"}}},
		{{Key: "$group", Value: bson.M{"_id": nil, "total": bson.M{"$sum": "$total"}}}},
	})
	if err == nil {
		var res []struct {
			Total int64 `bson:"total"`
		}
		if err := revCursor.All(ctx, &res); err != nil {
			log.Warn("STATS", "Revenue cursor decode failed", "err", err)
		}
		revCursor.Close(ctx)
		if len(res) > 0 {
			totalRevenue = int(res[0].Total)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"total_users": userCount, "total_orders": orderCount,
		"total_products": productCount, "total_revenue": totalRevenue,
	})
}
