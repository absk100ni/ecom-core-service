package payment

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
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

// ==================== GATEWAY ROUTER ====================

func (h *Handler) activeGateway() string {
	gw := h.cfg.PaymentGateway
	if gw == "cashfree" && h.cfg.CashfreeAppID != "" {
		return "cashfree"
	}
	if gw == "razorpay" && h.cfg.RazorpayKeyID != "" {
		return "razorpay"
	}
	// Auto-detect: prefer whichever has credentials
	if h.cfg.CashfreeAppID != "" {
		return "cashfree"
	}
	if h.cfg.RazorpayKeyID != "" {
		return "razorpay"
	}
	return "mock"
}

// ==================== RAZORPAY ====================

func (h *Handler) createRazorpayOrder(amountPaise int, currency, receipt string) (string, error) {
	payload := map[string]interface{}{
		"amount": amountPaise, "currency": currency, "receipt": receipt, "payment_capture": 1,
	}
	data, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", "https://api.razorpay.com/v1/orders", bytes.NewBuffer(data))
	if err != nil { return "", fmt.Errorf("request error: %w", err) }
	req.SetBasicAuth(h.cfg.RazorpayKeyID, h.cfg.RazorpaySecret)
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil { return "", fmt.Errorf("razorpay API error: %w", err) }
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("razorpay status %d: %s", resp.StatusCode, string(body))
	}
	var result map[string]interface{}
	json.Unmarshal(body, &result)
	orderID, _ := result["id"].(string)
	if orderID == "" { return "", fmt.Errorf("no order_id in razorpay response") }
	log.Info("RAZORPAY", "Order created", "razorpay_order_id", orderID, "amount", amountPaise)
	return orderID, nil
}

// ==================== CASHFREE ====================

func (h *Handler) cashfreeBaseURL() string {
	if h.cfg.CashfreeEnv == "production" {
		return "https://api.cashfree.com/pg"
	}
	return "https://sandbox.cashfree.com/pg"
}

func (h *Handler) createCashfreeOrder(amountPaise int, currency, orderID, customerPhone, customerName string) (string, string, error) {
	amountRupees := float64(amountPaise) / 100.0

	payload := map[string]interface{}{
		"order_id":       orderID,
		"order_amount":   amountRupees,
		"order_currency": currency,
		"customer_details": map[string]interface{}{
			"customer_id":    "cust_" + orderID[:8],
			"customer_phone": customerPhone,
			"customer_name":  customerName,
		},
		"order_meta": map[string]interface{}{
			"return_url": fmt.Sprintf("https://yourstore.com/orders?order_id=%s", orderID),
		},
	}
	data, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", h.cashfreeBaseURL()+"/orders", bytes.NewBuffer(data))
	if err != nil { return "", "", fmt.Errorf("request error: %w", err) }
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-client-id", h.cfg.CashfreeAppID)
	req.Header.Set("x-client-secret", h.cfg.CashfreeSecret)
	req.Header.Set("x-api-version", "2023-08-01")

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil { return "", "", fmt.Errorf("cashfree API error: %w", err) }
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("cashfree status %d: %s", resp.StatusCode, string(body))
	}

	var result map[string]interface{}
	json.Unmarshal(body, &result)

	cfOrderID, _ := result["cf_order_id"].(float64)
	paySessionID, _ := result["payment_session_id"].(string)
	if paySessionID == "" { return "", "", fmt.Errorf("no payment_session_id in cashfree response") }

	log.Info("CASHFREE", "Order created", "cf_order_id", cfOrderID, "payment_session_id", paySessionID[:20]+"...", "amount", amountRupees)
	return fmt.Sprintf("%.0f", cfOrderID), paySessionID, nil
}

func (h *Handler) verifyCashfreePayment(orderID string) (string, error) {
	req, err := http.NewRequest("GET", h.cashfreeBaseURL()+"/orders/"+orderID, nil)
	if err != nil { return "", err }
	req.Header.Set("x-client-id", h.cfg.CashfreeAppID)
	req.Header.Set("x-client-secret", h.cfg.CashfreeSecret)
	req.Header.Set("x-api-version", "2023-08-01")

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil { return "", err }
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result map[string]interface{}
	json.Unmarshal(body, &result)
	status, _ := result["order_status"].(string)
	return status, nil
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
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found"})
		return
	}
	if order.PaymentStatus == "paid" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Already paid"})
		return
	}

	gateway := h.activeGateway()
	log.Info("CREATE", "Creating payment", "gateway", gateway, "order_id", req.OrderID, "amount", order.Total)

	var gatewayOrderID, paymentSessionID, gatewayKeyID string

	switch gateway {
	case "razorpay":
		rzpOrderID, err := h.createRazorpayOrder(order.Total, "INR", order.OrderNumber)
		if err != nil {
			log.Error("CREATE", "Razorpay order failed", "err", err.Error())
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Payment gateway error: " + err.Error()})
			return
		}
		gatewayOrderID = rzpOrderID
		gatewayKeyID = h.cfg.RazorpayKeyID

	case "cashfree":
		cfOrderID, sessionID, err := h.createCashfreeOrder(order.Total, "INR", order.ID, order.ShippingAddress.Phone, order.ShippingAddress.Name)
		if err != nil {
			log.Error("CREATE", "Cashfree order failed", "err", err.Error())
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Payment gateway error: " + err.Error()})
			return
		}
		gatewayOrderID = cfOrderID
		paymentSessionID = sessionID
		gatewayKeyID = h.cfg.CashfreeAppID

	default: // mock
		gatewayOrderID = "order_mock_" + uuid.New().String()[:16]
		gatewayKeyID = ""
	}

	payment := models.Payment{
		ID: uuid.New().String(), OrderID: order.ID, UserID: userID,
		RazorpayOrderID: gatewayOrderID, Amount: order.Total, Currency: "INR",
		Status: "created", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	h.db.Collection("payments").InsertOne(ctx, payment)

	response := gin.H{
		"payment_id":       payment.ID,
		"gateway":          gateway,
		"amount":           order.Total,
		"currency":         "INR",
		"order_id":         order.ID,
		"order_number":     order.OrderNumber,
	}

	switch gateway {
	case "razorpay":
		response["razorpay_order_id"] = gatewayOrderID
		response["razorpay_key_id"] = gatewayKeyID
	case "cashfree":
		response["cf_order_id"] = gatewayOrderID
		response["payment_session_id"] = paymentSessionID
		response["cashfree_app_id"] = gatewayKeyID
		response["cashfree_env"] = h.cfg.CashfreeEnv
	default:
		response["razorpay_key_id"] = "" // signals dev mode to frontend
		response["razorpay_order_id"] = gatewayOrderID
	}

	log.Info("CREATE", "Payment created", "payment_id", payment.ID, "gateway", gateway, "gateway_order_id", gatewayOrderID)
	c.JSON(http.StatusOK, response)
}

// Verify — POST /payment/verify
func (h *Handler) Verify(c *gin.Context) {
	userID := c.GetString("user_id")
	var req struct {
		// Razorpay fields
		RazorpayOrderID   string `json:"razorpay_order_id"`
		RazorpayPaymentID string `json:"razorpay_payment_id"`
		RazorpaySignature string `json:"razorpay_signature"`
		// Cashfree fields
		CfOrderID string `json:"cf_order_id"`
		OrderID   string `json:"order_id"`
		// Gateway hint
		Gateway string `json:"gateway"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Payment verification data required"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	gateway := req.Gateway
	if gateway == "" { gateway = h.activeGateway() }

	switch gateway {
	case "razorpay":
		h.verifyRazorpay(c, ctx, userID, req.RazorpayOrderID, req.RazorpayPaymentID, req.RazorpaySignature)
	case "cashfree":
		h.verifyCashfree(c, ctx, userID, req.OrderID)
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "Unknown gateway"})
	}
}

func (h *Handler) verifyRazorpay(c *gin.Context, ctx context.Context, userID, rzpOrderID, rzpPaymentID, rzpSignature string) {
	if rzpOrderID == "" || rzpPaymentID == "" || rzpSignature == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "razorpay_order_id, razorpay_payment_id, razorpay_signature required"})
		return
	}

	var payment models.Payment
	if err := h.db.Collection("payments").FindOne(ctx, bson.M{"razorpay_order_id": rzpOrderID, "user_id": userID}).Decode(&payment); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Payment not found"})
		return
	}

	if h.cfg.RazorpaySecret != "" {
		expected := hmacSHA256(rzpOrderID+"|"+rzpPaymentID, h.cfg.RazorpaySecret)
		if expected != rzpSignature {
			log.WarnWithCode("VERIFY", errcodes.EPayVerifyFailed.Code, "Invalid Razorpay signature")
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid payment signature", "code": errcodes.EPayVerifyFailed.Code})
			return
		}
	}

	h.markPaymentPaid(ctx, payment.ID, payment.OrderID, rzpPaymentID)
	log.Info("VERIFY", "Razorpay payment verified", "payment_id", payment.ID, "order_id", payment.OrderID)

	var order models.Order
	h.db.Collection("orders").FindOne(ctx, bson.M{"_id": payment.OrderID}).Decode(&order)
	if order.ShippingAddress.Phone != "" {
		utils.SendPaymentConfirmSMS(order.ShippingAddress.Phone, order.OrderNumber, order.Total)
	}

	c.JSON(http.StatusOK, gin.H{"message": "Payment verified", "order_id": payment.OrderID, "order_number": order.OrderNumber, "gateway": "razorpay"})
}

func (h *Handler) verifyCashfree(c *gin.Context, ctx context.Context, userID, orderID string) {
	if orderID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "order_id required for cashfree verification"})
		return
	}

	var payment models.Payment
	if err := h.db.Collection("payments").FindOne(ctx, bson.M{"order_id": orderID, "user_id": userID}).Decode(&payment); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Payment not found"})
		return
	}

	// Verify with Cashfree API
	status, err := h.verifyCashfreePayment(orderID)
	if err != nil {
		log.Error("VERIFY", "Cashfree verification API failed", "err", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Verification failed"})
		return
	}

	if status != "PAID" {
		log.Warn("VERIFY", "Cashfree payment not paid", "status", status, "order_id", orderID)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Payment not completed", "cashfree_status": status})
		return
	}

	h.markPaymentPaid(ctx, payment.ID, payment.OrderID, "cf_"+orderID)
	log.Info("VERIFY", "Cashfree payment verified", "payment_id", payment.ID, "order_id", payment.OrderID)

	var order models.Order
	h.db.Collection("orders").FindOne(ctx, bson.M{"_id": payment.OrderID}).Decode(&order)
	if order.ShippingAddress.Phone != "" {
		utils.SendPaymentConfirmSMS(order.ShippingAddress.Phone, order.OrderNumber, order.Total)
	}

	c.JSON(http.StatusOK, gin.H{"message": "Payment verified", "order_id": payment.OrderID, "order_number": order.OrderNumber, "gateway": "cashfree"})
}

func (h *Handler) markPaymentPaid(ctx context.Context, paymentID, orderID, gatewayPaymentID string) {
	now := time.Now()
	h.db.Collection("payments").UpdateOne(ctx, bson.M{"_id": paymentID}, bson.M{"$set": bson.M{
		"razorpay_payment_id": gatewayPaymentID, "status": "paid", "updated_at": now,
	}})
	h.db.Collection("orders").UpdateOne(ctx, bson.M{"_id": orderID}, bson.M{"$set": bson.M{
		"payment_id": gatewayPaymentID, "payment_status": "paid", "status": "confirmed", "updated_at": now,
	}})
}

// Webhook — POST /payment/webhook (no auth)
func (h *Handler) Webhook(c *gin.Context) {
	gateway := c.Query("gateway")
	if gateway == "" { gateway = h.activeGateway() }

	switch gateway {
	case "cashfree":
		h.cashfreeWebhook(c)
	default:
		h.razorpayWebhook(c)
	}
}

func (h *Handler) razorpayWebhook(c *gin.Context) {
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

	log.Info("WEBHOOK", "Razorpay webhook", "event", payload.Event)

	if payload.Event == "payment.captured" {
		if pe, ok := payload.Payload["payment"].(map[string]interface{}); ok {
			if e, ok := pe["entity"].(map[string]interface{}); ok {
				rid, _ := e["order_id"].(string)
				pid, _ := e["id"].(string)
				var p models.Payment
				h.db.Collection("payments").FindOne(ctx, bson.M{"razorpay_order_id": rid}).Decode(&p)
				if p.OrderID != "" {
					h.markPaymentPaid(ctx, p.ID, p.OrderID, pid)
					log.Info("WEBHOOK", "Razorpay payment captured", "order_id", p.OrderID)
				}
			}
		}
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (h *Handler) cashfreeWebhook(c *gin.Context) {
	var payload map[string]interface{}
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	eventType, _ := payload["type"].(string)
	log.Info("WEBHOOK", "Cashfree webhook", "type", eventType)

	if eventType == "PAYMENT_SUCCESS_WEBHOOK" || eventType == "ORDER_PAID" {
		data, _ := payload["data"].(map[string]interface{})
		if data != nil {
			orderData, _ := data["order"].(map[string]interface{})
			if orderData != nil {
				oid, _ := orderData["order_id"].(string)
				if oid != "" {
					var p models.Payment
					h.db.Collection("payments").FindOne(ctx, bson.M{"order_id": oid}).Decode(&p)
					if p.OrderID != "" {
						h.markPaymentPaid(ctx, p.ID, p.OrderID, "cf_webhook_"+oid)
						log.Info("WEBHOOK", "Cashfree payment confirmed", "order_id", p.OrderID)
					}
				}
			}
		}
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// ==================== UTILS ====================

func hmacSHA256(data, secret string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))
}

// For Cashfree webhook signature verification (future use)
func hmacSHA256Base64(data, secret string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(data))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}
