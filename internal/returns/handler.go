package returns

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"ecom-core-service/internal/config"
	"ecom-core-service/internal/models"
	"ecom-core-service/pkg/errcodes"
	"ecom-core-service/pkg/logger"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var log = logger.New("RETURNS", "REQ")

type Handler struct {
	db  *mongo.Database
	cfg *config.Config
}

func NewHandler(db *mongo.Database, cfg *config.Config) *Handler {
	return &Handler{db: db, cfg: cfg}
}

type returnItemRequest struct {
	ProductID string `json:"product_id" binding:"required"`
	Qty       int    `json:"qty" binding:"required,gt=0"`
	Reason    string `json:"reason" binding:"required"`
}

func validateReturnItems(orderItems []models.OrderItem, requested []returnItemRequest) ([]models.ReturnItem, error) {
	purchasedQty := make(map[string]int, len(orderItems))
	for _, item := range orderItems {
		purchasedQty[item.ProductID] += item.Quantity
	}
	seen := make(map[string]bool, len(requested))
	items := make([]models.ReturnItem, 0, len(requested))
	for _, item := range requested {
		maxQty, exists := purchasedQty[item.ProductID]
		if !exists {
			return nil, fmt.Errorf("return item is not part of this order")
		}
		if seen[item.ProductID] {
			return nil, fmt.Errorf("duplicate product in return request")
		}
		if item.Qty > maxQty {
			return nil, fmt.Errorf("return quantity for product %s exceeds purchased quantity of %d", item.ProductID, maxQty)
		}
		seen[item.ProductID] = true
		items = append(items, models.ReturnItem{ProductID: item.ProductID, Qty: item.Qty, Reason: item.Reason})
	}
	return items, nil
}

// Create handles POST /api/v1/orders/:id/return
func (h *Handler) Create(c *gin.Context) {
	userID := c.GetString("user_id")
	orderID := c.Param("id")

	var req struct {
		Items   []returnItemRequest `json:"items" binding:"required,min=1"`
		Type    string              `json:"type" binding:"required,oneof=return replacement"`
		Comment string              `json:"comment"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": errcodes.ERtnInvalidData.Code})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Fetch order
	var order models.Order
	if err := h.db.Collection("orders").FindOne(ctx, bson.M{"_id": orderID}).Decode(&order); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found", "code": errcodes.EOrdNotFound.Code})
		return
	}
	if order.UserID != userID {
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found", "code": errcodes.EOrdNotFound.Code})
		return
	}

	// Must be delivered
	if order.Status != "delivered" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Returns allowed only for delivered orders", "code": errcodes.ERtnNotDelivered.Code})
		return
	}

	// Check return window
	windowDays := h.cfg.ReturnWindowDays
	if windowDays == 0 {
		windowDays = 7
	}
	if order.DeliveredAt == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Delivery date not recorded", "code": errcodes.ERtnNotDelivered.Code})
		return
	}
	deadline := order.DeliveredAt.Add(time.Duration(windowDays) * 24 * time.Hour)
	if time.Now().After(deadline) {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Return window of %d days has expired", windowDays), "code": errcodes.ERtnWindowExpired.Code})
		return
	}

	// Check no open request for this order
	count, _ := h.db.Collection("return_requests").CountDocuments(ctx, bson.M{
		"order_id": orderID,
		"status":   bson.M{"$in": []string{"requested", "approved"}},
	})
	if count > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "An open return request already exists for this order", "code": errcodes.ERtnDuplicate.Code})
		return
	}

	// Validate every requested line against the immutable order snapshot. Never
	// trust product ids or quantities supplied by the browser.
	items, validationErr := validateReturnItems(order.Items, req.Items)
	if validationErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": validationErr.Error(), "code": errcodes.ERtnInvalidData.Code})
		return
	}

	now := time.Now()
	returnReq := models.ReturnRequest{
		ID:        uuid.New().String(),
		OrderID:   orderID,
		UserID:    userID,
		Items:     items,
		Type:      req.Type,
		Status:    "requested",
		Comment:   req.Comment,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if _, err := h.db.Collection("return_requests").InsertOne(ctx, returnReq); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			c.JSON(http.StatusConflict, gin.H{"error": "An open return request already exists for this order", "code": errcodes.ERtnDuplicate.Code})
			return
		}
		log.Error("Create", "Failed to insert return request", "order_id", orderID, "err", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create return request", "code": errcodes.ERtnCreateFailed.Code})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"return_request": returnReq})
}

// Get handles GET /api/v1/orders/:id/return — customer view
func (h *Handler) Get(c *gin.Context) {
	userID := c.GetString("user_id")
	orderID := c.Param("id")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var rr models.ReturnRequest
	err := h.db.Collection("return_requests").FindOne(ctx, bson.M{"order_id": orderID, "user_id": userID}).Decode(&rr)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "No return request found", "code": errcodes.ERtnNotFound.Code})
		return
	}
	c.JSON(http.StatusOK, gin.H{"return_request": rr})
}

// AdminList handles GET /api/v1/admin/returns?status=
func (h *Handler) AdminList(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	filter := bson.M{}
	if status := c.Query("status"); status != "" {
		filter["status"] = status
	}

	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(100)
	cursor, err := h.db.Collection("return_requests").Find(ctx, filter, opts)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list returns", "code": errcodes.ERtnListFailed.Code})
		return
	}
	defer cursor.Close(ctx)

	results := make([]models.ReturnRequest, 0)
	if err := cursor.All(ctx, &results); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to decode returns", "code": errcodes.ERtnListFailed.Code})
		return
	}
	c.JSON(http.StatusOK, gin.H{"return_requests": results, "total": len(results)})
}

func validReturnTransition(current, next string) bool {
	switch current {
	case "requested":
		return next == "approved" || next == "rejected"
	case "approved":
		return next == "completed"
	default:
		return false
	}
}

// AdminUpdate handles PUT /api/v1/admin/returns/:id
func (h *Handler) AdminUpdate(c *gin.Context) {
	id := c.Param("id")

	var req struct {
		Status    string `json:"status" binding:"required,oneof=approved rejected completed"`
		AdminNote string `json:"admin_note"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": errcodes.ERtnInvalidData.Code})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var current models.ReturnRequest
	if err := h.db.Collection("return_requests").FindOne(ctx, bson.M{"_id": id}).Decode(&current); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Return request not found", "code": errcodes.ERtnNotFound.Code})
		return
	}
	if !validReturnTransition(current.Status, req.Status) {
		c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("Cannot transition return from '%s' to '%s'", current.Status, req.Status), "code": errcodes.ERtnInvalidData.Code})
		return
	}

	update := bson.M{"$set": bson.M{"status": req.Status, "updated_at": time.Now()}}
	if req.AdminNote != "" {
		update["$set"].(bson.M)["admin_note"] = req.AdminNote
	}

	result, err := h.db.Collection("return_requests").UpdateOne(ctx, bson.M{"_id": id, "status": current.Status}, update)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Return request update failed", "code": errcodes.ERtnCreateFailed.Code})
		return
	}
	if result.MatchedCount == 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "Return request changed while the update was being processed", "code": errcodes.ERtnInvalidData.Code})
		return
	}

	// Approval records the operational decision only. Refunds are processed from
	// the order screen through the payment provider, and replacements must never
	// enter a refund path implicitly.

	c.Set("audit_reason", req.AdminNote)
	c.JSON(http.StatusOK, gin.H{"message": "Return request updated", "status": req.Status})
}
