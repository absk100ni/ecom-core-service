package payment

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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

// ==================== RAZORPAY REST API ====================

// createRazorpayOrder calls Razorpay Orders API to create a real order
func (h *Handler) createRazorpayOrder(amountPaise int, currency, receipt string) (string, error) {
	if h.cfg.RazorpayKeyID == "" || h.cfg.RazorpaySecret == "" {
		return "", fmt.Errorf("razorpay credentials not configured")
	}

	payload := map[string]interface{}{
		"amount":   amountPaise,
		"currency": currency,
		"receipt":  receipt,
		"payment_capture": 1, // Auto-capture
	}
	data, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", "https://api.razorpay.com/v1/orders", bytes.NewBuffer(data))
	if err != nil {
		return "", fmt.Errorf("request creation failed: %w", err)
	}
	req.SetBasicAuth(h.cfg.RazorpayKeyID, h.cfg.RazorpaySecret)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("razorpay API call failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		log.Error("RAZORPAY", "Razorpay order creation failed", "status", resp.StatusCode, "body", string(body))
		return "", fmt.Errorf("razorpay returned status %d: %s", resp.StatusCode, string(body))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("failed to parse razorpay response: %w", err)
	}

	orderID, ok := result["id"].(string)
	if !ok || orderID == "" {
		return "", fmt.Errorf("razorpay order ID not found in response")
	}

	log.Info("RAZORPAY", "Order created on Razorpay", "razorpay_order_id", orderID, "amount", amountPaise)
	return orderID, nil
}

// ==================== HANDLERS ====================

// Create — POST /payment/create
func (h *Handler) Create(c *gin.Context) {
	userID := c.GetString("user_id")
	var req struct {
		OrderID string `json:"order_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "order_id required"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var order models.Order
	if err := h.db.Collection("orders").FindOne(ctx, bson.M{"_id": req.OrderID, "user_id": userID}).Decode(&order); err != nil {
		log.Warn("CREATE", "Order not found for payment", "order_id", req.OrderID, "user_id", userID)
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found"})
		return
	}
	if order.PaymentStatus == "paid" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Already paid"})
		return
	}

	// Try to create real Razorpay order
	razorpayOrderID, err := h.createRazorpayOrder(order.Total, "INR", order.OrderNumber)
	if err != nil {
		log.Warn("CREATE", "Razorpay order creation failed, using mock", "err", err.Error(), "order_id", req.OrderID)
		// Fallback to mock order ID for dev mode
		razorpayOrderID = "order_mock_" + uuid.New().String()[:16]
	}

	payment := models.Payment{
		ID: uuid.New().String(), OrderID: order.ID, UserID: userID,
		RazorpayOrderID: razorpayOrderID, Amount: order.Total, Currency: "INR",
		Status: "created", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	h.db.Collection("payments").InsertOne(ctx, payment)

	log.Info("CREATE", "Payment created", "payment_id", payment.ID, "order_id", order.ID, "razorpay_order_id", razorpayOrderID, "amount", order.Total)

	c.JSON(http.StatusOK, gin.H{
		"payment_id":       payment.ID,
		"razorpay_order_id": razorpayOrderID,
		"razorpay_key_id":  h.cfg.RazorpayKeyID,
		"amount":           order.Total,
		"currency":         "INR",
		"order_id":         order.ID,
		"order_number":     order.OrderNumber,
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
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "All fields required"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var payment models.Payment
	if err := h.db.Collection("payments").FindOne(ctx, bson.M{"razorpay_order_id": req.RazorpayOrderID, "user_id": userID}).Decode(&payment); err != nil {
		log.Warn("VERIFY", "Payment not found", "razorpay_order_id", req.RazorpayOrderID, "user_id", userID)
		c.JSON(http.StatusNotFound, gin.H{"error": "Payment not found"})
		return
	}

	// Verify signature (skip if no secret — dev mode)
	if h.cfg.RazorpaySecret != "" {
		expected := hmacSHA256(req.RazorpayOrderID+"|"+req.RazorpayPaymentID, h.cfg.RazorpaySecret)
		if expected != req.RazorpaySignature {
			log.WarnWithCode("VERIFY", errcodes.EPayVerifyFailed.Code, "Invalid Razorpay signature", "razorpay_order_id", req.RazorpayOrderID)
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid payment signature", "code": errcodes.EPayVerifyFailed.Code})
			return
		}
	}

	// Update payment record
	h.db.Collection("payments").UpdateOne(ctx, bson.M{"_id": payment.ID}, bson.M{"$set": bson.M{
		"razorpay_payment_id": req.RazorpayPaymentID, "razorpay_signature": req.RazorpaySignature,
		"status": "paid", "updated_at": time.Now(),
	}})

	// Update order status
	h.db.Collection("orders").UpdateOne(ctx, bson.M{"_id": payment.OrderID}, bson.M{"$set": bson.M{
		"payment_id": req.RazorpayPaymentID, "payment_status": "paid", "status": "confirmed", "updated_at": time.Now(),
	}})

	log.Info("VERIFY", "Payment verified successfully", "payment_id", payment.ID, "order_id", payment.OrderID, "razorpay_payment_id", req.RazorpayPaymentID)

	// Send payment confirmation SMS
	var order models.Order
	h.db.Collection("orders").FindOne(ctx, bson.M{"_id": payment.OrderID}).Decode(&order)
	if order.ShippingAddress.Phone != "" {
		utils.SendPaymentConfirmSMS(order.ShippingAddress.Phone, order.OrderNumber, order.Total)
	}

	c.JSON(http.StatusOK, gin.H{"message": "Payment verified", "order_id": payment.OrderID, "order_number": order.OrderNumber})
}

// Webhook — POST /payment/webhook (no auth required)
func (h *Handler) Webhook(c *gin.Context) {
	var payload struct {
		Event   string                 `json:"event"`
		Payload map[string]interface{} `json:"payload"`
	}
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	log.Info("WEBHOOK", "Received Razorpay webhook", "event", payload.Event)

	if payload.Event == "payment.captured" {
		if pe, ok := payload.Payload["payment"].(map[string]interface{}); ok {
			if e, ok := pe["entity"].(map[string]interface{}); ok {
				rid, _ := e["order_id"].(string)
				pid, _ := e["id"].(string)

				h.db.Collection("payments").UpdateOne(ctx,
					bson.M{"razorpay_order_id": rid},
					bson.M{"$set": bson.M{"razorpay_payment_id": pid, "status": "paid", "updated_at": time.Now()}})

				var p models.Payment
				h.db.Collection("payments").FindOne(ctx, bson.M{"razorpay_order_id": rid}).Decode(&p)
				if p.OrderID != "" {
					h.db.Collection("orders").UpdateOne(ctx, bson.M{"_id": p.OrderID},
						bson.M{"$set": bson.M{"payment_id": pid, "payment_status": "paid", "status": "confirmed", "updated_at": time.Now()}})
					log.Info("WEBHOOK", "Order confirmed via webhook", "order_id", p.OrderID, "razorpay_payment_id", pid)
				}
			}
		}
	}

	if payload.Event == "payment.failed" {
		if pe, ok := payload.Payload["payment"].(map[string]interface{}); ok {
			if e, ok := pe["entity"].(map[string]interface{}); ok {
				rid, _ := e["order_id"].(string)
				h.db.Collection("payments").UpdateOne(ctx,
					bson.M{"razorpay_order_id": rid},
					bson.M{"$set": bson.M{"status": "failed", "updated_at": time.Now()}})
				log.Warn("WEBHOOK", "Payment failed", "razorpay_order_id", rid)
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
