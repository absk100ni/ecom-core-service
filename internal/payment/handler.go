package payment

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"time"

	"ecom-core-service/internal/config"
	"ecom-core-service/internal/models"
	"ecom-core-service/pkg/errcodes"
	"ecom-core-service/pkg/logger"
	"ecom-core-service/pkg/utils"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

var log = logger.New("PAYMENT", "TXN")
var _ = errcodes.EPayCreateFailed

type Handler struct {
	db  *mongo.Database
	cfg *config.Config
}

func NewHandler(db *mongo.Database, cfg *config.Config) *Handler { return &Handler{db: db, cfg: cfg} }

// Create — POST /payment/create
func (h *Handler) Create(c *gin.Context) {
	userID := c.GetString("user_id")
	var req struct{ OrderID string `json:"order_id" binding:"required"` }
	if err := c.ShouldBindJSON(&req); err != nil { c.JSON(http.StatusBadRequest, gin.H{"error": "order_id required"}); return }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var order models.Order
	if err := h.db.Collection("orders").FindOne(ctx, bson.M{"_id": req.OrderID, "user_id": userID}).Decode(&order); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found"})
		return
	}
	if order.PaymentStatus == "paid" { c.JSON(http.StatusBadRequest, gin.H{"error": "Already paid"}); return }

	razorpayOrderID := "order_" + uuid.New().String()[:20]

	payment := models.Payment{
		ID: uuid.New().String(), OrderID: order.ID, UserID: userID,
		RazorpayOrderID: razorpayOrderID, Amount: order.Total, Currency: "INR",
		Status: "created", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	h.db.Collection("payments").InsertOne(ctx, payment)

	c.JSON(http.StatusOK, gin.H{
		"payment_id": payment.ID, "razorpay_order_id": razorpayOrderID,
		"razorpay_key_id": h.cfg.RazorpayKeyID, "amount": order.Total,
		"currency": "INR", "order_id": order.ID,
	})
}

// Verify — POST /payment/verify
func (h *Handler) Verify(c *gin.Context) {
	userID := c.GetString("user_id")
	var req struct {
		RazorpayOrderID   string `json:"razorpay_order_id" binding:"required"`
		RazorpayPaymentID string `json:"razorpay_payment_id" binding:"required"`
		RazorpaySignature string `json:"razorpay_signature" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil { c.JSON(http.StatusBadRequest, gin.H{"error": "All fields required"}); return }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var payment models.Payment
	if err := h.db.Collection("payments").FindOne(ctx, bson.M{"razorpay_order_id": req.RazorpayOrderID, "user_id": userID}).Decode(&payment); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Payment not found"})
		return
	}

	if h.cfg.RazorpaySecret != "" {
		expected := hmacSHA256(req.RazorpayOrderID+"|"+req.RazorpayPaymentID, h.cfg.RazorpaySecret)
		if expected != req.RazorpaySignature { c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid signature"}); return }
	}

	h.db.Collection("payments").UpdateOne(ctx, bson.M{"_id": payment.ID}, bson.M{"$set": bson.M{
		"razorpay_payment_id": req.RazorpayPaymentID, "razorpay_signature": req.RazorpaySignature,
		"status": "paid", "updated_at": time.Now(),
	}})
	h.db.Collection("orders").UpdateOne(ctx, bson.M{"_id": payment.OrderID}, bson.M{"$set": bson.M{
		"payment_id": req.RazorpayPaymentID, "payment_status": "paid", "status": "confirmed", "updated_at": time.Now(),
	}})

	// Send payment confirmation notification
	var order models.Order
	h.db.Collection("orders").FindOne(ctx, bson.M{"_id": payment.OrderID}).Decode(&order)
	if order.ShippingAddress.Phone != "" {
		utils.SendSMS(order.ShippingAddress.Phone, "Payment confirmed for "+order.OrderNumber+"! Your order is being prepared.", "HIGH")
	}

	c.JSON(http.StatusOK, gin.H{"message": "Payment verified", "order_id": payment.OrderID})
}

// Webhook — POST /payment/webhook
func (h *Handler) Webhook(c *gin.Context) {
	var payload struct {
		Event   string                 `json:"event"`
		Payload map[string]interface{} `json:"payload"`
	}
	if err := c.ShouldBindJSON(&payload); err != nil { c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid"}); return }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if payload.Event == "payment.captured" {
		if pe, ok := payload.Payload["payment"].(map[string]interface{}); ok {
			if e, ok := pe["entity"].(map[string]interface{}); ok {
				rid, _ := e["order_id"].(string)
				h.db.Collection("payments").UpdateOne(ctx, bson.M{"razorpay_order_id": rid}, bson.M{"$set": bson.M{"status": "paid", "updated_at": time.Now()}})
				var p models.Payment
				h.db.Collection("payments").FindOne(ctx, bson.M{"razorpay_order_id": rid}).Decode(&p)
				if p.OrderID != "" {
					h.db.Collection("orders").UpdateOne(ctx, bson.M{"_id": p.OrderID}, bson.M{"$set": bson.M{"payment_status": "paid", "status": "confirmed", "updated_at": time.Now()}})
				}
			}
		}
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func hmacSHA256(data, secret string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))
}
