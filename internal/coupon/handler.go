package coupon

import (
	"context"
	"net/http"
	"strings"
	"time"

	"ecom-core-service/internal/models"
	"ecom-core-service/pkg/errcodes"
	"ecom-core-service/pkg/logger"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var log = logger.New("COUPON", "MGMT")

// Ensure errcodes is referenced
var _ = errcodes.ECpnNotFound

type Handler struct{ db *mongo.Database }

func NewHandler(db *mongo.Database) *Handler { return &Handler{db: db} }

// Create — POST /admin/coupons
func (h *Handler) Create(c *gin.Context) {
	var coupon models.Coupon
	if err := c.ShouldBindJSON(&coupon); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid data", "details": err.Error()})
		return
	}
	if coupon.Type != "percentage" && coupon.Type != "fixed" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Type must be 'percentage' or 'fixed'"})
		return
	}
	if coupon.Type == "percentage" && coupon.Value > 100 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Percentage value cannot exceed 100"})
		return
	}

	coupon.ID = uuid.New().String()
	coupon.Code = strings.ToUpper(strings.TrimSpace(coupon.Code))
	coupon.IsActive = true
	coupon.UsedCount = 0
	coupon.CreatedAt = time.Now()
	coupon.UpdatedAt = time.Now()
	if coupon.UsageLimit == 0 { coupon.UsageLimit = 1000 }
	if coupon.ExpiresAt.IsZero() { coupon.ExpiresAt = time.Now().AddDate(0, 1, 0) }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Check duplicate code
	count, _ := h.db.Collection("coupons").CountDocuments(ctx, bson.M{"code": coupon.Code})
	if count > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "Coupon code already exists"})
		return
	}

	if _, err := h.db.Collection("coupons").InsertOne(ctx, coupon); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create coupon"})
		return
	}
	c.JSON(http.StatusCreated, coupon)
}

// List — GET /admin/coupons
func (h *Handler) List(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	filter := bson.M{}
	if c.Query("active") == "true" { filter["is_active"] = true }
	if c.Query("active") == "false" { filter["is_active"] = false }

	opts := options.Find().SetSort(bson.M{"created_at": -1})
	cursor, err := h.db.Collection("coupons").Find(ctx, filter, opts)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch coupons"})
		return
	}
	defer cursor.Close(ctx)

	var coupons []models.Coupon
	cursor.All(ctx, &coupons)
	if coupons == nil { coupons = []models.Coupon{} }
	c.JSON(http.StatusOK, gin.H{"coupons": coupons, "total": len(coupons)})
}

// Get — GET /admin/coupons/:id
func (h *Handler) Get(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var coupon models.Coupon
	err := h.db.Collection("coupons").FindOne(ctx, bson.M{"_id": c.Param("id")}).Decode(&coupon)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Coupon not found"})
		return
	}
	c.JSON(http.StatusOK, coupon)
}

// Update — PUT /admin/coupons/:id
func (h *Handler) Update(c *gin.Context) {
	var updates map[string]interface{}
	if err := c.ShouldBindJSON(&updates); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid data"})
		return
	}
	updates["updated_at"] = time.Now()
	delete(updates, "_id")
	delete(updates, "id")
	if code, ok := updates["code"].(string); ok {
		updates["code"] = strings.ToUpper(strings.TrimSpace(code))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := h.db.Collection("coupons").UpdateOne(ctx, bson.M{"_id": c.Param("id")}, bson.M{"$set": updates})
	if err != nil || result.MatchedCount == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Coupon not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Coupon updated"})
}

// Delete — DELETE /admin/coupons/:id (soft)
func (h *Handler) Delete(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := h.db.Collection("coupons").UpdateOne(ctx, bson.M{"_id": c.Param("id")},
		bson.M{"$set": bson.M{"is_active": false, "updated_at": time.Now()}})
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Coupon not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Coupon deactivated"})
}

// Validate — POST /coupons/validate (customer-facing)
func (h *Handler) Validate(c *gin.Context) {
	var req struct {
		Code     string `json:"code" binding:"required"`
		Subtotal int    `json:"subtotal" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Code and subtotal required"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var coupon models.Coupon
	code := strings.ToUpper(strings.TrimSpace(req.Code))
	err := h.db.Collection("coupons").FindOne(ctx, bson.M{"code": code, "is_active": true}).Decode(&coupon)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Invalid coupon code"})
		return
	}

	// Validate
	if time.Now().After(coupon.ExpiresAt) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Coupon has expired"})
		return
	}
	if coupon.UsedCount >= coupon.UsageLimit {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Coupon usage limit reached"})
		return
	}
	if req.Subtotal < coupon.MinOrder {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Minimum order amount not met", "min_order": coupon.MinOrder})
		return
	}

	// Calculate discount
	discount := 0
	if coupon.Type == "percentage" {
		discount = req.Subtotal * coupon.Value / 100
		if coupon.MaxDiscount > 0 && discount > coupon.MaxDiscount {
			discount = coupon.MaxDiscount
		}
	} else {
		discount = coupon.Value
		if discount > req.Subtotal { discount = req.Subtotal }
	}

	c.JSON(http.StatusOK, gin.H{
		"valid": true, "coupon": coupon, "discount": discount,
		"final_amount": req.Subtotal - discount,
	})
}
