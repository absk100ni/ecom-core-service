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

// Create handles POST /api/v1/orders/:id/return
func (h *Handler) Create(c *gin.Context) {
	userID := c.GetString("user_id")
	orderID := c.Param("id")

	var req struct {
		Items []struct {
			ProductID string `json:"product_id" binding:"required"`
			Qty       int    `json:"qty" binding:"required,gt=0"`
			Reason    string `json:"reason" binding:"required"`
		} `json:"items" binding:"required,min=1"`
		Type    string `json:"type" binding:"required,oneof=return replacement"`
		Comment string `json:"comment"`
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

	// Build items
	items := make([]models.ReturnItem, 0, len(req.Items))
	for _, ri := range req.Items {
		items = append(items, models.ReturnItem{
			ProductID: ri.ProductID,
			Qty:       ri.Qty,
			Reason:    ri.Reason,
		})
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

	update := bson.M{"$set": bson.M{"status": req.Status, "updated_at": time.Now()}}
	if req.AdminNote != "" {
		update["$set"].(bson.M)["admin_note"] = req.AdminNote
	}

	result, err := h.db.Collection("return_requests").UpdateOne(ctx, bson.M{"_id": id}, update)
	if err != nil || result.MatchedCount == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Return request not found", "code": errcodes.ERtnNotFound.Code})
		return
	}

	// P1-2b: Auto-refund when return is approved for a paid order
	if req.Status == "approved" || req.Status == "completed" {
		var rr models.ReturnRequest
		h.db.Collection("return_requests").FindOne(ctx, bson.M{"_id": id}).Decode(&rr)

		var returnOrder models.Order
		if err := h.db.Collection("orders").FindOne(ctx, bson.M{"_id": rr.OrderID}).Decode(&returnOrder); err == nil {
			if returnOrder.PaymentStatus == "paid" || returnOrder.PaymentStatus == "refund_pending" {
				// Calculate return amount from returned items
				returnAmount := 0
				for _, ri := range rr.Items {
					for _, oi := range returnOrder.Items {
						if oi.ProductID == ri.ProductID {
							returnAmount += oi.Price * ri.Qty
						}
					}
				}
				if returnAmount > 0 {
					now := time.Now()
					h.db.Collection("orders").UpdateOne(ctx, bson.M{"_id": rr.OrderID}, bson.M{"$set": bson.M{
						"payment_status": "refund_pending",
						"refund_amount":  returnAmount,
						"updated_at":     now,
					}})
					log.Info("RETURN_REFUND", "Return approved — refund pending", "order_id", rr.OrderID, "return_amount", returnAmount)
				}
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{"message": "Return request updated", "status": req.Status})
}
