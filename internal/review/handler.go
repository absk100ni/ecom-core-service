package review

import (
	"context"
	"fmt"
	"net/http"
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

var log = logger.New("REVIEW", "UGC")
var _ = errcodes.ERevCreateFailed

type Handler struct{ db *mongo.Database }

func NewHandler(db *mongo.Database) *Handler { return &Handler{db: db} }

// Create — POST /reviews
func (h *Handler) Create(c *gin.Context) {
	userID := c.GetString("user_id")
	var rev models.Review
	if err := c.ShouldBindJSON(&rev); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "product_id and rating (1-5) required", "details": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Check product exists
	count, _ := h.db.Collection("products").CountDocuments(ctx, bson.M{"_id": rev.ProductID, "is_active": true})
	if count == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Product not found"})
		return
	}

	// One review per user per product
	existing, _ := h.db.Collection("reviews").CountDocuments(ctx, bson.M{"product_id": rev.ProductID, "user_id": userID})
	if existing > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "You have already reviewed this product"})
		return
	}

	// Check verified purchase
	deliveredCount, _ := h.db.Collection("orders").CountDocuments(ctx, bson.M{
		"user_id": userID, "status": "delivered", "items.product_id": rev.ProductID,
	})

	// Get user name
	var user models.User
	h.db.Collection("users").FindOne(ctx, bson.M{"_id": userID}).Decode(&user)

	rev.ID = uuid.New().String()
	rev.UserID = userID
	rev.UserName = user.Name
	if rev.UserName == "" { rev.UserName = "Customer" }
	rev.UserPhone = user.Phone
	rev.IsVerified = deliveredCount > 0
	rev.IsApproved = true // Auto-approve; admin can reject later
	if rev.Images == nil { rev.Images = []string{} }
	rev.CreatedAt = time.Now()
	rev.UpdatedAt = time.Now()

	if _, err := h.db.Collection("reviews").InsertOne(ctx, rev); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create review"})
		return
	}
	c.JSON(http.StatusCreated, rev)
}

// ListByProduct — GET /reviews/product/:productId
func (h *Handler) ListByProduct(c *gin.Context) {
	productID := c.Param("productId")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	page, limit := 1, 20
	if p := c.Query("page"); p != "" { fmt.Sscanf(p, "%d", &page) }
	if l := c.Query("limit"); l != "" { fmt.Sscanf(l, "%d", &limit) }
	if page < 1 { page = 1 }
	if limit < 1 || limit > 50 { limit = 20 }

	filter := bson.M{"product_id": productID, "is_approved": true}
	total, _ := h.db.Collection("reviews").CountDocuments(ctx, filter)

	opts := options.Find().SetSort(bson.M{"created_at": -1}).SetSkip(int64((page - 1) * limit)).SetLimit(int64(limit))
	cursor, err := h.db.Collection("reviews").Find(ctx, filter, opts)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch reviews"})
		return
	}
	defer cursor.Close(ctx)

	var reviews []models.Review
	cursor.All(ctx, &reviews)
	if reviews == nil { reviews = []models.Review{} }

	// Calculate average rating
	avgRating := 0.0
	ratingDist := map[int]int{1: 0, 2: 0, 3: 0, 4: 0, 5: 0}
	allCursor, _ := h.db.Collection("reviews").Find(ctx, bson.M{"product_id": productID, "is_approved": true})
	var allReviews []models.Review
	allCursor.All(ctx, &allReviews)
	allCursor.Close(ctx)
	if len(allReviews) > 0 {
		sum := 0
		for _, r := range allReviews {
			sum += r.Rating
			ratingDist[r.Rating]++
		}
		avgRating = float64(sum) / float64(len(allReviews))
	}

	c.JSON(http.StatusOK, gin.H{
		"reviews": reviews, "total": total, "page": page,
		"average_rating": avgRating, "total_reviews": len(allReviews),
		"rating_distribution": ratingDist,
	})
}

// ListByUser — GET /reviews/my-reviews
func (h *Handler) ListByUser(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cursor, _ := h.db.Collection("reviews").Find(ctx, bson.M{"user_id": userID}, options.Find().SetSort(bson.M{"created_at": -1}))
	defer cursor.Close(ctx)
	var reviews []models.Review
	cursor.All(ctx, &reviews)
	if reviews == nil { reviews = []models.Review{} }
	c.JSON(http.StatusOK, gin.H{"reviews": reviews, "total": len(reviews)})
}

// Update — PUT /reviews/:id
func (h *Handler) Update(c *gin.Context) {
	userID := c.GetString("user_id")
	id := c.Param("id")
	var updates struct {
		Rating  int      `json:"rating"`
		Title   string   `json:"title"`
		Comment string   `json:"comment"`
		Images  []string `json:"images"`
	}
	if err := c.ShouldBindJSON(&updates); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid data"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	set := bson.M{"updated_at": time.Now()}
	if updates.Rating >= 1 && updates.Rating <= 5 { set["rating"] = updates.Rating }
	if updates.Title != "" { set["title"] = updates.Title }
	if updates.Comment != "" { set["comment"] = updates.Comment }
	if updates.Images != nil { set["images"] = updates.Images }

	result, err := h.db.Collection("reviews").UpdateOne(ctx, bson.M{"_id": id, "user_id": userID}, bson.M{"$set": set})
	if err != nil || result.MatchedCount == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Review not found or not yours"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Review updated"})
}

// Delete — DELETE /reviews/:id
func (h *Handler) Delete(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := h.db.Collection("reviews").DeleteOne(ctx, bson.M{"_id": c.Param("id"), "user_id": userID})
	if err != nil || result.DeletedCount == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Review not found or not yours"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Review deleted"})
}

// AdminList — GET /admin/reviews
func (h *Handler) AdminList(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	filter := bson.M{}
	if c.Query("approved") == "true" { filter["is_approved"] = true }
	if c.Query("approved") == "false" { filter["is_approved"] = false }
	if pid := c.Query("product_id"); pid != "" { filter["product_id"] = pid }

	cursor, _ := h.db.Collection("reviews").Find(ctx, filter, options.Find().SetSort(bson.M{"created_at": -1}).SetLimit(100))
	defer cursor.Close(ctx)
	var reviews []models.Review
	cursor.All(ctx, &reviews)
	if reviews == nil { reviews = []models.Review{} }
	c.JSON(http.StatusOK, gin.H{"reviews": reviews, "total": len(reviews)})
}

// AdminApprove — PUT /admin/reviews/:id/approve
func (h *Handler) AdminApprove(c *gin.Context) {
	var req struct{ IsApproved bool `json:"is_approved"` }
	c.ShouldBindJSON(&req)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, _ := h.db.Collection("reviews").UpdateOne(ctx, bson.M{"_id": c.Param("id")},
		bson.M{"$set": bson.M{"is_approved": req.IsApproved, "updated_at": time.Now()}})
	if result.MatchedCount == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Review not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Review approval updated"})
}
