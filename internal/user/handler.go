package user

import (
	"context"
	"net/http"
	"time"

	"ecom-core-service/internal/models"
	"ecom-core-service/pkg/errcodes"
	"ecom-core-service/pkg/logger"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

var log = logger.New("USER", "PROFILE")
var _ = errcodes.EUsrNotFound

type Handler struct{ db *mongo.Database }

func NewHandler(db *mongo.Database) *Handler { return &Handler{db: db} }

// GetProfile — GET /user
func (h *Handler) GetProfile(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var user models.User
	if err := h.db.Collection("users").FindOne(ctx, bson.M{"_id": userID}).Decode(&user); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
		return
	}
	c.JSON(http.StatusOK, user)
}

// UpdateProfile — PUT /user
func (h *Handler) UpdateProfile(c *gin.Context) {
	userID := c.GetString("user_id")
	var req struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid data"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	set := bson.M{"updated_at": time.Now()}
	if req.Name != "" { set["name"] = req.Name }
	if req.Email != "" { set["email"] = req.Email }

	result, err := h.db.Collection("users").UpdateOne(ctx, bson.M{"_id": userID}, bson.M{"$set": set})
	if err != nil || result.MatchedCount == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Profile updated"})
}

// ListAddresses — GET /user/addresses
func (h *Handler) ListAddresses(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var user models.User
	if err := h.db.Collection("users").FindOne(ctx, bson.M{"_id": userID}).Decode(&user); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
		return
	}
	if user.Addresses == nil { user.Addresses = []models.Address{} }
	c.JSON(http.StatusOK, gin.H{"addresses": user.Addresses, "total": len(user.Addresses)})
}

// AddAddress — POST /user/addresses
func (h *Handler) AddAddress(c *gin.Context) {
	userID := c.GetString("user_id")
	var addr models.Address
	if err := c.ShouldBindJSON(&addr); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid address data"})
		return
	}

	addr.ID = uuid.New().String()
	if addr.Country == "" { addr.Country = "India" }
	if addr.Label == "" { addr.Label = "Home" }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// If is_default or first address, clear other defaults
	var user models.User
	h.db.Collection("users").FindOne(ctx, bson.M{"_id": userID}).Decode(&user)
	if len(user.Addresses) == 0 { addr.IsDefault = true }

	if addr.IsDefault {
		h.db.Collection("users").UpdateOne(ctx, bson.M{"_id": userID},
			bson.M{"$set": bson.M{"addresses.$[].is_default": false}})
	}

	h.db.Collection("users").UpdateOne(ctx, bson.M{"_id": userID},
		bson.M{"$push": bson.M{"addresses": addr}, "$set": bson.M{"updated_at": time.Now()}})

	c.JSON(http.StatusCreated, gin.H{"message": "Address added", "address": addr})
}

// UpdateAddress — PUT /user/addresses/:addressId
func (h *Handler) UpdateAddress(c *gin.Context) {
	userID := c.GetString("user_id")
	addressID := c.Param("addressId")
	var updates map[string]interface{}
	if err := c.ShouldBindJSON(&updates); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid data"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	set := bson.M{"updated_at": time.Now()}
	fields := []string{"name", "label", "line1", "line2", "city", "state", "pincode", "country", "phone"}
	for _, f := range fields {
		if v, ok := updates[f]; ok { set["addresses.$."+f] = v }
	}

	result, err := h.db.Collection("users").UpdateOne(ctx, bson.M{"_id": userID, "addresses.id": addressID}, bson.M{"$set": set})
	if err != nil || result.MatchedCount == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Address not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Address updated"})
}

// DeleteAddress — DELETE /user/addresses/:addressId
func (h *Handler) DeleteAddress(c *gin.Context) {
	userID := c.GetString("user_id")
	addressID := c.Param("addressId")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	h.db.Collection("users").UpdateOne(ctx, bson.M{"_id": userID},
		bson.M{"$pull": bson.M{"addresses": bson.M{"id": addressID}}, "$set": bson.M{"updated_at": time.Now()}})
	c.JSON(http.StatusOK, gin.H{"message": "Address deleted"})
}

// SetDefaultAddress — PUT /user/addresses/:addressId/default
func (h *Handler) SetDefaultAddress(c *gin.Context) {
	userID := c.GetString("user_id")
	addressID := c.Param("addressId")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Unset all defaults first
	h.db.Collection("users").UpdateOne(ctx, bson.M{"_id": userID},
		bson.M{"$set": bson.M{"addresses.$[].is_default": false}})
	// Set this one as default
	h.db.Collection("users").UpdateOne(ctx, bson.M{"_id": userID, "addresses.id": addressID},
		bson.M{"$set": bson.M{"addresses.$.is_default": true, "updated_at": time.Now()}})
	c.JSON(http.StatusOK, gin.H{"message": "Default address set"})
}
