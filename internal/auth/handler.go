package auth

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"time"

	"ecom-core-service/internal/config"
	"ecom-core-service/internal/middleware"
	"ecom-core-service/internal/models"
	"ecom-core-service/pkg/errcodes"
	"ecom-core-service/pkg/logger"
	"ecom-core-service/pkg/utils"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

var log = logger.New("AUTH", "OTP")

type Handler struct {
	db  *mongo.Database
	cfg *config.Config
}

func NewHandler(db *mongo.Database, cfg *config.Config) *Handler {
	return &Handler{db: db, cfg: cfg}
}

func (h *Handler) SendOTP(c *gin.Context) {
	var req struct{ Phone string `json:"phone" binding:"required"` }
	if err := c.ShouldBindJSON(&req); err != nil {
		log.WarnWithCode("SEND", errcodes.EAuthInvalidPhone.Code, "Invalid phone input", "err", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Phone required", "code": errcodes.EAuthInvalidPhone.Code})
		return
	}

	log.Debug("SEND", "Generating OTP", "phone", req.Phone)
	code := fmt.Sprintf("%06d", rand.Intn(1000000))
	if h.cfg.OTPService == "mock" {
		code = "123456"
		log.Debug("SEND", "Using mock OTP", "phone", req.Phone)
	}

	otp := models.OTP{ID: uuid.New().String(), Phone: req.Phone, Code: code, ExpiresAt: time.Now().Add(5 * time.Minute), CreatedAt: time.Now()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	h.db.Collection("otps").DeleteMany(ctx, bson.M{"phone": req.Phone})
	if _, err := h.db.Collection("otps").InsertOne(ctx, otp); err != nil {
		log.ErrorWithCode("SEND", errcodes.EAuthOTPStoreFailed.Code, "Failed to store OTP", "phone", req.Phone, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errcodes.EAuthOTPStoreFailed.Message, "code": errcodes.EAuthOTPStoreFailed.Code})
		return
	}

	// Send OTP via notification service
	utils.SendSMS(req.Phone, fmt.Sprintf("Your OTP is: %s", code), "HIGH")
	log.Info("SEND", "OTP sent successfully", "phone", req.Phone)

	resp := gin.H{"message": "OTP sent", "phone": req.Phone}
	if h.cfg.Environment == "development" { resp["otp"] = code }
	c.JSON(http.StatusOK, resp)
}

func (h *Handler) VerifyOTP(c *gin.Context) {
	var req struct {
		Phone string `json:"phone" binding:"required"`
		Code  string `json:"code" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		log.WarnWithCode("VERIFY", errcodes.EAuthInvalidPhone.Code, "Invalid verify input", "err", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Phone and code required"})
		return
	}

	log.Debug("VERIFY", "Verifying OTP", "phone", req.Phone)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var otp models.OTP
	err := h.db.Collection("otps").FindOne(ctx, bson.M{"phone": req.Phone, "code": req.Code, "verified": false}).Decode(&otp)
	if err != nil {
		log.WarnWithCode("VERIFY", errcodes.EAuthOTPInvalid.Code, "OTP not found or already used", "phone", req.Phone)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid OTP", "code": errcodes.EAuthOTPInvalid.Code})
		return
	}
	if time.Now().After(otp.ExpiresAt) {
		log.WarnWithCode("VERIFY", errcodes.EAuthOTPInvalid.Code, "OTP expired", "phone", req.Phone, "expired_at", otp.ExpiresAt)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "OTP expired", "code": errcodes.EAuthOTPInvalid.Code})
		return
	}
	h.db.Collection("otps").UpdateOne(ctx, bson.M{"_id": otp.ID}, bson.M{"$set": bson.M{"verified": true}})

	var user models.User
	findErr := h.db.Collection("users").FindOne(ctx, bson.M{"phone": req.Phone}).Decode(&user)
	if findErr == mongo.ErrNoDocuments {
		user = models.User{ID: uuid.New().String(), Phone: req.Phone, CreatedAt: time.Now(), UpdatedAt: time.Now()}
		if _, err := h.db.Collection("users").InsertOne(ctx, user); err != nil {
			log.ErrorWithCode("VERIFY", errcodes.EAuthUserCreateFailed.Code, "Failed to create user", "phone", req.Phone, "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errcodes.EAuthUserCreateFailed.Message})
			return
		}
		log.Info("VERIFY", "New user created", "user_id", user.ID, "phone", req.Phone)
	}

	token, err := middleware.GenerateToken(user.ID, user.Phone, h.cfg.JWTSecret, user.IsAdmin)
	if err != nil {
		log.ErrorWithCode("VERIFY", errcodes.EAuthTokenGenFailed.Code, "Token generation failed", "user_id", user.ID, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errcodes.EAuthTokenGenFailed.Message})
		return
	}

	log.Info("VERIFY", "User authenticated", "user_id", user.ID, "phone", req.Phone, "is_admin", user.IsAdmin)
	c.JSON(http.StatusOK, gin.H{"token": token, "user": user})
}
