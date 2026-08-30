package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ecom-core-service/internal/middleware"
	"ecom-core-service/internal/models"
	"ecom-core-service/pkg/errcodes"
	"ecom-core-service/pkg/utils"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// GoogleTokenInfo represents the response from Google's tokeninfo endpoint.
type GoogleTokenInfo struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified string `json:"email_verified"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
	Aud           string `json:"aud"`
	Exp           string `json:"exp"`
	Error         string `json:"error_description"`
}

// VerifyGoogleToken validates a Google ID token and returns parsed info.
func VerifyGoogleToken(idToken, expectedClientID string) (*GoogleTokenInfo, error) {
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(
		"https://oauth2.googleapis.com/tokeninfo?id_token=" + idToken,
	)
	if err != nil {
		return nil, fmt.Errorf("google tokeninfo request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var info GoogleTokenInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("failed to parse tokeninfo: %w", err)
	}
	if info.Error != "" {
		return nil, fmt.Errorf("google token invalid: %s", info.Error)
	}
	if info.Aud != expectedClientID {
		return nil, fmt.Errorf("audience mismatch: got %s, want %s", info.Aud, expectedClientID)
	}
	if info.EmailVerified != "true" {
		return nil, fmt.Errorf("email not verified")
	}
	return &info, nil
}

// GoogleSignIn — POST /api/v1/auth/google
func (h *Handler) GoogleSignIn(c *gin.Context) {
	var req struct {
		IDToken string `json:"id_token" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id_token required"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var info *GoogleTokenInfo

	// Dev mock: accept mock-google:<email> tokens when GOOGLE_CLIENT_ID is unset in development
	if h.cfg.GoogleClientID == "" {
		if h.cfg.Environment != "development" {
			log.ErrorWithCode("GOOGLE", errcodes.EGoogUnconfigured.Code, "GOOGLE_CLIENT_ID not set in production")
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": errcodes.EGoogUnconfigured.Message, "code": errcodes.EGoogUnconfigured.Code})
			return
		}
		// Mock mode
		if strings.HasPrefix(req.IDToken, "mock-google:") {
			email := strings.TrimPrefix(req.IDToken, "mock-google:")
			info = &GoogleTokenInfo{
				Sub:           "mock_sub_" + email,
				Email:         email,
				EmailVerified: "true",
				Name:          "Mock User",
				Picture:       "",
			}
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "In dev without GOOGLE_CLIENT_ID, use mock-google:<email> format"})
			return
		}
	} else {
		var err error
		info, err = VerifyGoogleToken(req.IDToken, h.cfg.GoogleClientID)
		if err != nil {
			log.WarnWithCode("GOOGLE", errcodes.EGoogInvalidToken.Code, "Google token verification failed", "err", err)
			c.JSON(http.StatusUnauthorized, gin.H{"error": errcodes.EGoogInvalidToken.Message, "code": errcodes.EGoogInvalidToken.Code})
			return
		}
	}

	// Find-or-create user: match by google_sub first, then by verified email
	var user models.User
	err := h.db.Collection("users").FindOne(ctx, bson.M{"google_sub": info.Sub}).Decode(&user)
	if err == mongo.ErrNoDocuments {
		// Try email match
		err = h.db.Collection("users").FindOne(ctx, bson.M{"email": info.Email}).Decode(&user)
		if err == mongo.ErrNoDocuments {
			// Create new user
			user = models.User{
				ID:        uuid.New().String(),
				Email:     info.Email,
				Name:      info.Name,
				Avatar:    info.Picture,
				GoogleSub: info.Sub,
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			}
			if _, iErr := h.db.Collection("users").InsertOne(ctx, user); iErr != nil {
				log.ErrorWithCode("GOOGLE", errcodes.EAuthUserCreateFailed.Code, "Failed to create Google user", "err", iErr)
				c.JSON(http.StatusInternalServerError, gin.H{"error": errcodes.EAuthUserCreateFailed.Message})
				return
			}
			log.Info("GOOGLE", "New Google user created", "user_id", user.ID, "email", info.Email)
		} else if err != nil {
			log.Error("GOOGLE", "DB lookup failed", "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal error"})
			return
		} else {
			// Link google_sub to existing email-matched user
			h.db.Collection("users").UpdateOne(ctx, bson.M{"_id": user.ID}, bson.M{"$set": bson.M{
				"google_sub": info.Sub, "avatar": info.Picture, "updated_at": time.Now(),
			}})
			user.GoogleSub = info.Sub
			user.Avatar = info.Picture
		}
	} else if err != nil {
		log.Error("GOOGLE", "DB lookup failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal error"})
		return
	}

	token, err := middleware.GenerateToken(user.ID, user.Phone, h.cfg.JWTSecret, user.IsAdmin)
	if err != nil {
		log.ErrorWithCode("GOOGLE", errcodes.EAuthTokenGenFailed.Code, "Token generation failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errcodes.EAuthTokenGenFailed.Message})
		return
	}

	log.Info("GOOGLE", "Google sign-in successful", "user_id", user.ID, "email", info.Email)
	c.JSON(http.StatusOK, gin.H{"token": token, "user": user})
}

// LinkPhone — POST /api/v1/users/me/link-phone (sends OTP to attach phone)
func (h *Handler) LinkPhone(c *gin.Context) {
	userID := c.GetString("user_id")
	var req struct {
		Phone string `json:"phone" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Phone required"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Check if phone already belongs to another user
	var existing models.User
	err := h.db.Collection("users").FindOne(ctx, bson.M{"phone": req.Phone, "_id": bson.M{"$ne": userID}}).Decode(&existing)
	if err == nil {
		log.WarnWithCode("LINK_PHONE", errcodes.EGoogLinkConflict.Code, "Phone already owned", "phone", req.Phone, "user_id", userID)
		c.JSON(http.StatusConflict, gin.H{"error": errcodes.EGoogLinkConflict.Message, "code": errcodes.EGoogLinkConflict.Code})
		return
	}

	// Send OTP via existing flow
	code := "123456"
	if h.cfg.OTPService != "mock" {
		code = fmt.Sprintf("%06d", time.Now().UnixNano()%1000000)
	}

	otp := models.OTP{ID: uuid.New().String(), Phone: req.Phone, Code: code, ExpiresAt: time.Now().Add(5 * time.Minute), CreatedAt: time.Now()}
	h.db.Collection("otps").DeleteMany(ctx, bson.M{"phone": req.Phone})
	if _, err := h.db.Collection("otps").InsertOne(ctx, otp); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send OTP"})
		return
	}
	utils.SendSMS(req.Phone, fmt.Sprintf("Your OTP is: %s", code), "HIGH")

	resp := gin.H{"message": "OTP sent to phone for linking", "phone": req.Phone}
	if h.cfg.Environment == "development" {
		resp["otp"] = code
	}
	c.JSON(http.StatusOK, resp)
}

// VerifyPhoneLink — POST /api/v1/users/me/verify-phone (completes phone linking)
func (h *Handler) VerifyPhoneLink(c *gin.Context) {
	userID := c.GetString("user_id")
	var req struct {
		Phone string `json:"phone" binding:"required"`
		OTP   string `json:"otp" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Phone and OTP required"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Verify OTP
	var otp models.OTP
	err := h.db.Collection("otps").FindOne(ctx, bson.M{"phone": req.Phone, "code": req.OTP, "verified": false}).Decode(&otp)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid OTP", "code": errcodes.EAuthOTPInvalid.Code})
		return
	}
	if time.Now().After(otp.ExpiresAt) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "OTP expired", "code": errcodes.EAuthOTPInvalid.Code})
		return
	}

	// Check phone not taken by another user
	var existing models.User
	if err := h.db.Collection("users").FindOne(ctx, bson.M{"phone": req.Phone, "_id": bson.M{"$ne": userID}}).Decode(&existing); err == nil {
		c.JSON(http.StatusConflict, gin.H{"error": errcodes.EGoogLinkConflict.Message, "code": errcodes.EGoogLinkConflict.Code})
		return
	}

	// Link phone
	h.db.Collection("users").UpdateOne(ctx, bson.M{"_id": userID}, bson.M{"$set": bson.M{"phone": req.Phone, "updated_at": time.Now()}})
	h.db.Collection("otps").UpdateOne(ctx, bson.M{"_id": otp.ID}, bson.M{"$set": bson.M{"verified": true}})

	log.Info("LINK_PHONE", "Phone linked to account", "user_id", userID, "phone", req.Phone)
	c.JSON(http.StatusOK, gin.H{"message": "Phone linked successfully", "phone": req.Phone})
}
