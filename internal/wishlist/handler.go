package wishlist

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

var log = logger.New("WISHLIST", "USER")
var _ = errcodes.EWishAddFailed

type Handler struct{ db *mongo.Database }

func NewHandler(db *mongo.Database) *Handler { return &Handler{db: db} }

// Get — GET /wishlist
func (h *Handler) Get(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wl models.Wishlist
	err := h.db.Collection("wishlists").FindOne(ctx, bson.M{"user_id": userID}).Decode(&wl)
	if err != nil {
		wl = models.Wishlist{ID: uuid.New().String(), UserID: userID, Items: []models.WishlistItem{}, CreatedAt: time.Now(), UpdatedAt: time.Now()}
		h.db.Collection("wishlists").InsertOne(ctx, wl)
	}

	// Enrich with product details
	type enrichedItem struct {
		models.WishlistItem
		Product *models.Product `json:"product,omitempty"`
	}
	enriched := make([]enrichedItem, 0, len(wl.Items))
	for _, item := range wl.Items {
		ei := enrichedItem{WishlistItem: item}
		var p models.Product
		if err := h.db.Collection("products").FindOne(ctx, bson.M{"_id": item.ProductID, "is_active": true}).Decode(&p); err == nil {
			ei.Product = &p
		}
		enriched = append(enriched, ei)
	}

	c.JSON(http.StatusOK, gin.H{"wishlist": enriched, "total": len(enriched)})
}

// Add — POST /wishlist
func (h *Handler) Add(c *gin.Context) {
	userID := c.GetString("user_id")
	var req struct{ ProductID string `json:"product_id" binding:"required"` }
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "product_id required"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Check product exists
	count, _ := h.db.Collection("products").CountDocuments(ctx, bson.M{"_id": req.ProductID, "is_active": true})
	if count == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Product not found"})
		return
	}

	item := models.WishlistItem{ProductID: req.ProductID, AddedAt: time.Now()}

	var wl models.Wishlist
	err := h.db.Collection("wishlists").FindOne(ctx, bson.M{"user_id": userID}).Decode(&wl)
	if err != nil {
		wl = models.Wishlist{ID: uuid.New().String(), UserID: userID, Items: []models.WishlistItem{item}, CreatedAt: time.Now(), UpdatedAt: time.Now()}
		h.db.Collection("wishlists").InsertOne(ctx, wl)
	} else {
		// Check duplicate
		for _, existing := range wl.Items {
			if existing.ProductID == req.ProductID {
				c.JSON(http.StatusOK, gin.H{"message": "Already in wishlist"})
				return
			}
		}
		h.db.Collection("wishlists").UpdateOne(ctx, bson.M{"_id": wl.ID},
			bson.M{"$push": bson.M{"items": item}, "$set": bson.M{"updated_at": time.Now()}})
	}
	c.JSON(http.StatusOK, gin.H{"message": "Added to wishlist"})
}

// Remove — DELETE /wishlist/:productId
func (h *Handler) Remove(c *gin.Context) {
	userID := c.GetString("user_id")
	productID := c.Param("productId")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	h.db.Collection("wishlists").UpdateOne(ctx, bson.M{"user_id": userID},
		bson.M{"$pull": bson.M{"items": bson.M{"product_id": productID}}, "$set": bson.M{"updated_at": time.Now()}})
	c.JSON(http.StatusOK, gin.H{"message": "Removed from wishlist"})
}

// Check — GET /wishlist/check/:productId
func (h *Handler) Check(c *gin.Context) {
	userID := c.GetString("user_id")
	productID := c.Param("productId")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	count, _ := h.db.Collection("wishlists").CountDocuments(ctx,
		bson.M{"user_id": userID, "items.product_id": productID})
	c.JSON(http.StatusOK, gin.H{"in_wishlist": count > 0})
}
