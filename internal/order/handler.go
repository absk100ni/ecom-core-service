package order

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"time"

	"ecom-core-service/internal/models"
	"ecom-core-service/pkg/errcodes"
	"ecom-core-service/pkg/logger"
	"ecom-core-service/pkg/utils"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var log = logger.New("ORDER", "MGMT")

type Handler struct{ db *mongo.Database }

func NewHandler(db *mongo.Database) *Handler { return &Handler{db: db} }

func genOrderNumber() string {
	return fmt.Sprintf("ORD-%d%04d", time.Now().Unix()%100000, rand.Intn(10000))
}

func (h *Handler) Create(c *gin.Context) {
	userID := c.GetString("user_id")
	var req struct {
		ShippingAddress models.Address `json:"shipping_address" binding:"required"`
		CouponCode      string         `json:"coupon_code,omitempty"`
		Notes           string         `json:"notes,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		log.WarnWithCode("CREATE", errcodes.EOrdAddressInvalid.Code, "Shipping address required", "user_id", userID, "err", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Shipping address required", "code": errcodes.EOrdAddressInvalid.Code})
		return
	}

	if req.ShippingAddress.Name == "" || req.ShippingAddress.Line1 == "" ||
		req.ShippingAddress.City == "" || req.ShippingAddress.State == "" ||
		req.ShippingAddress.Pincode == "" || req.ShippingAddress.Phone == "" {
		log.WarnWithCode("CREATE", errcodes.EOrdAddressInvalid.Code, "Incomplete shipping address", "user_id", userID)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Complete shipping address required (name, line1, city, state, pincode, phone)", "code": errcodes.EOrdAddressInvalid.Code})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var cart models.Cart
	if err := h.db.Collection("carts").FindOne(ctx, bson.M{"user_id": userID}).Decode(&cart); err != nil || len(cart.Items) == 0 {
		log.WarnWithCode("CREATE", errcodes.EOrdCartEmpty.Code, "Cart is empty", "user_id", userID)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cart is empty", "code": errcodes.EOrdCartEmpty.Code})
		return
	}

	log.Info("CREATE", "Starting order creation", "user_id", userID, "cart_items", len(cart.Items))

	items := make([]models.OrderItem, 0, len(cart.Items))
	subtotal := 0
	stockErrors := []string{}

	for _, ci := range cart.Items {
		var product models.Product
		err := h.db.Collection("products").FindOneAndUpdate(ctx,
			bson.M{"_id": ci.ProductID, "is_active": true, "stock": bson.M{"$gte": ci.Quantity}},
			bson.M{"$inc": bson.M{"stock": -ci.Quantity}},
			options.FindOneAndUpdate().SetReturnDocument(options.Before),
		).Decode(&product)

		if err != nil {
			stockErrors = append(stockErrors, fmt.Sprintf("%s: insufficient stock or not found", ci.Name))
			log.WarnWithCode("CREATE", errcodes.EOrdStockFailed.Code, "Stock check failed for item", "user_id", userID, "product", ci.Name, "product_id", ci.ProductID, "requested_qty", ci.Quantity)
			// Rollback already deducted items
			for _, item := range items {
				h.db.Collection("products").UpdateOne(ctx, bson.M{"_id": item.ProductID}, bson.M{"$inc": bson.M{"stock": item.Quantity}})
			}
			log.Warn("CREATE", "Stock rollback completed", "user_id", userID, "rolled_back_items", len(items))
			c.JSON(http.StatusBadRequest, gin.H{"error": "Stock check failed", "code": errcodes.EOrdStockFailed.Code, "details": stockErrors})
			return
		}

		items = append(items, models.OrderItem{
			ProductID: ci.ProductID, VariantID: ci.VariantID,
			Name: ci.Name, SKU: product.SKU, Price: ci.Price,
			Quantity: ci.Quantity, Image: ci.Image,
		})
		subtotal += ci.Price * ci.Quantity
	}

	discount := 0
	if req.CouponCode != "" {
		var coupon models.Coupon
		err := h.db.Collection("coupons").FindOne(ctx, bson.M{"code": req.CouponCode, "is_active": true}).Decode(&coupon)
		if err == nil && subtotal >= coupon.MinOrder && coupon.UsedCount < coupon.UsageLimit && time.Now().Before(coupon.ExpiresAt) {
			if coupon.Type == "percentage" {
				discount = subtotal * coupon.Value / 100
				if coupon.MaxDiscount > 0 && discount > coupon.MaxDiscount { discount = coupon.MaxDiscount }
			} else {
				discount = coupon.Value
				if discount > subtotal { discount = subtotal }
			}
			h.db.Collection("coupons").UpdateOne(ctx, bson.M{"_id": coupon.ID}, bson.M{"$inc": bson.M{"used_count": 1}})
			log.Info("CREATE", "Coupon applied", "user_id", userID, "coupon", req.CouponCode, "discount", discount)
		} else {
			log.Debug("CREATE", "Coupon not applicable", "user_id", userID, "coupon", req.CouponCode)
		}
	}

	shippingCost := 0
	if subtotal < 50000 { shippingCost = 5000 }
	total := subtotal - discount + shippingCost
	if total < 0 { total = 0 }

	order := models.Order{
		ID: uuid.New().String(), UserID: userID, OrderNumber: genOrderNumber(),
		Items: items, Subtotal: subtotal, ShippingCost: shippingCost, Discount: discount,
		Total: total, CouponCode: req.CouponCode, Status: "placed", PaymentStatus: "pending",
		ShippingAddress: req.ShippingAddress, Notes: req.Notes,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}

	_, err := h.db.Collection("orders").InsertOne(ctx, order)
	if err != nil {
		log.ErrorWithCode("CREATE", errcodes.EOrdCreateFailed.Code, "Order insert failed, rolling back stock", "user_id", userID, "err", err)
		for _, item := range items {
			h.db.Collection("products").UpdateOne(ctx, bson.M{"_id": item.ProductID}, bson.M{"$inc": bson.M{"stock": item.Quantity}})
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create order", "code": errcodes.EOrdCreateFailed.Code})
		return
	}

	h.db.Collection("carts").UpdateOne(ctx, bson.M{"user_id": userID},
		bson.M{"$set": bson.M{"items": []models.CartItem{}, "updated_at": time.Now()}})

	log.Info("CREATE", "Order placed successfully", "order_id", order.ID, "order_number", order.OrderNumber, "user_id", userID, "total", order.Total, "items_count", len(items))

	go utils.SendSMS(req.ShippingAddress.Phone,
		fmt.Sprintf("Order %s placed! Total: Rs.%d. We'll update you on shipping.", order.OrderNumber, order.Total/100), "HIGH")

	c.JSON(http.StatusCreated, order)
}

func (h *Handler) Get(c *gin.Context) {
	userID := c.GetString("user_id")
	id := c.Param("id")
	isAdmin, _ := c.Get("is_admin")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	filter := bson.M{"$or": []bson.M{{"_id": id}, {"order_number": id}}}
	if isAdmin == nil || !isAdmin.(bool) { filter["user_id"] = userID }

	var order models.Order
	if err := h.db.Collection("orders").FindOne(ctx, filter).Decode(&order); err != nil {
		log.Debug("GET", "Order not found", "order_id", id, "user_id", userID)
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found", "code": errcodes.EOrdNotFound.Code})
		return
	}
	c.JSON(http.StatusOK, order)
}

func (h *Handler) List(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	opts := options.Find().SetSort(bson.M{"created_at": -1}).SetLimit(50)
	cursor, err := h.db.Collection("orders").Find(ctx, bson.M{"user_id": userID}, opts)
	if err != nil {
		log.ErrorWithCode("LIST", errcodes.EOrdListFailed.Code, "Failed to list orders", "user_id", userID, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed"})
		return
	}
	defer cursor.Close(ctx)
	var orders []models.Order
	cursor.All(ctx, &orders)
	if orders == nil { orders = []models.Order{} }
	c.JSON(http.StatusOK, gin.H{"orders": orders, "total": len(orders)})
}

func (h *Handler) Cancel(c *gin.Context) {
	userID := c.GetString("user_id")
	orderID := c.Param("id")
	var req struct { Reason string `json:"reason"` }
	c.ShouldBindJSON(&req)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var order models.Order
	if err := h.db.Collection("orders").FindOne(ctx, bson.M{"_id": orderID, "user_id": userID}).Decode(&order); err != nil {
		log.Warn("CANCEL", "Order not found for cancellation", "order_id", orderID, "user_id", userID)
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found", "code": errcodes.EOrdNotFound.Code})
		return
	}

	if order.Status != "placed" && order.Status != "confirmed" {
		log.WarnWithCode("CANCEL", errcodes.EOrdCancelFailed.Code, "Cannot cancel order with current status", "order_id", orderID, "status", order.Status, "user_id", userID)
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Cannot cancel order with status '%s'", order.Status), "code": errcodes.EOrdCancelFailed.Code})
		return
	}

	now := time.Now()
	update := bson.M{"status": "cancelled", "cancel_reason": req.Reason, "cancelled_at": now, "updated_at": now}
	if order.PaymentStatus == "paid" {
		update["refund_amount"] = order.Total
		update["payment_status"] = "refund_pending"
	}
	h.db.Collection("orders").UpdateOne(ctx, bson.M{"_id": orderID}, bson.M{"$set": update})

	for _, item := range order.Items {
		h.db.Collection("products").UpdateOne(ctx, bson.M{"_id": item.ProductID}, bson.M{"$inc": bson.M{"stock": item.Quantity}})
	}
	if order.CouponCode != "" {
		h.db.Collection("coupons").UpdateOne(ctx, bson.M{"code": order.CouponCode}, bson.M{"$inc": bson.M{"used_count": -1}})
	}

	log.Info("CANCEL", "Order cancelled", "order_id", orderID, "order_number", order.OrderNumber, "user_id", userID, "reason", req.Reason, "refund_pending", order.PaymentStatus == "paid")

	go utils.SendSMS(order.ShippingAddress.Phone,
		fmt.Sprintf("Order %s cancelled. %s", order.OrderNumber,
			func() string { if order.PaymentStatus == "paid" { return "Refund will be processed in 5-7 business days." }; return "" }()), "HIGH")

	c.JSON(http.StatusOK, gin.H{"message": "Order cancelled", "refund_amount": order.Total})
}

// ==================== ADMIN ENDPOINTS ====================

func (h *Handler) AdminList(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	filter := bson.M{}
	if status := c.Query("status"); status != "" { filter["status"] = status }
	if ps := c.Query("payment_status"); ps != "" { filter["payment_status"] = ps }

	page, limit := 1, 50
	if p := c.Query("page"); p != "" { fmt.Sscanf(p, "%d", &page) }
	if l := c.Query("limit"); l != "" { fmt.Sscanf(l, "%d", &limit) }
	if page < 1 { page = 1 }
	skip := int64((page - 1) * limit)

	total, _ := h.db.Collection("orders").CountDocuments(ctx, filter)
	opts := options.Find().SetSort(bson.M{"created_at": -1}).SetSkip(skip).SetLimit(int64(limit))
	cursor, _ := h.db.Collection("orders").Find(ctx, filter, opts)
	defer cursor.Close(ctx)
	var orders []models.Order
	cursor.All(ctx, &orders)
	if orders == nil { orders = []models.Order{} }

	totalPages := int(total) / limit
	if int(total)%limit > 0 { totalPages++ }

	log.Debug("ADMIN_LIST", "Orders listed", "filter", filter, "page", page, "total", total)
	c.JSON(http.StatusOK, gin.H{"orders": orders, "total": total, "page": page, "total_pages": totalPages})
}

func (h *Handler) UpdateStatus(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Status     string `json:"status" binding:"required"`
		TrackingID string `json:"tracking_id,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Status required"})
		return
	}

	validStatuses := map[string]bool{"placed": true, "confirmed": true, "processing": true, "shipped": true, "delivered": true, "cancelled": true, "returned": true}
	if !validStatuses[req.Status] {
		log.WarnWithCode("UPDATE_STATUS", errcodes.EOrdInvalidStatus.Code, "Invalid status value", "order_id", id, "status", req.Status)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid status", "code": errcodes.EOrdInvalidStatus.Code})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	update := bson.M{"$set": bson.M{"status": req.Status, "updated_at": time.Now()}}
	if req.TrackingID != "" { update["$set"].(bson.M)["tracking_id"] = req.TrackingID }

	result, err := h.db.Collection("orders").UpdateOne(ctx, bson.M{"_id": id}, update)
	if err != nil || result.MatchedCount == 0 {
		log.Warn("UPDATE_STATUS", "Order not found", "order_id", id)
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found"})
		return
	}

	if req.Status == "cancelled" || req.Status == "returned" {
		var order models.Order
		h.db.Collection("orders").FindOne(ctx, bson.M{"_id": id}).Decode(&order)
		for _, item := range order.Items {
			h.db.Collection("products").UpdateOne(ctx, bson.M{"_id": item.ProductID}, bson.M{"$inc": bson.M{"stock": item.Quantity}})
		}
		log.Info("UPDATE_STATUS", "Stock restored for cancelled/returned order", "order_id", id, "items", len(order.Items))
	}

	log.Info("UPDATE_STATUS", "Order status updated", "order_id", id, "new_status", req.Status)
	c.JSON(http.StatusOK, gin.H{"message": "Status updated to " + req.Status})
}

func (h *Handler) ProcessRefund(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Amount int    `json:"amount"`
		Notes  string `json:"notes"`
	}
	c.ShouldBindJSON(&req)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var order models.Order
	if err := h.db.Collection("orders").FindOne(ctx, bson.M{"_id": id}).Decode(&order); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found"})
		return
	}

	if order.PaymentStatus != "paid" && order.PaymentStatus != "refund_pending" {
		log.WarnWithCode("REFUND", errcodes.EOrdRefundFailed.Code, "Order not in refundable state", "order_id", id, "payment_status", order.PaymentStatus)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Order payment is not in a refundable state", "code": errcodes.EOrdRefundFailed.Code})
		return
	}

	refundAmount := order.Total
	if req.Amount > 0 && req.Amount <= order.Total { refundAmount = req.Amount }
	now := time.Now()
	refundID := "REFUND-" + uuid.New().String()[:8]

	h.db.Collection("orders").UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{
		"payment_status": "refunded", "refund_id": refundID,
		"refund_amount": refundAmount, "refunded_at": now, "updated_at": now,
	}})

	log.Info("REFUND", "Refund processed", "refund_id", refundID, "order_id", id, "order_number", order.OrderNumber, "amount", refundAmount)

	go utils.SendSMS(order.ShippingAddress.Phone,
		fmt.Sprintf("Refund of Rs.%d processed for order %s. RefID: %s", refundAmount/100, order.OrderNumber, refundID), "HIGH")

	c.JSON(http.StatusOK, gin.H{"message": "Refund processed", "refund_id": refundID, "refund_amount": refundAmount, "order_number": order.OrderNumber})
}
