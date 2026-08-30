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
	"ecom-core-service/internal/notify"
	orderPkg "ecom-core-service/internal/order"
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
	db       *mongo.Database
	cfg      *config.Config
	notifier *notify.Notifier
}

func NewHandler(db *mongo.Database, cfg *config.Config) *Handler { return &Handler{db: db, cfg: cfg} }

// WithNotifier injects the notification service (optional — nil means no notifications).
func (h *Handler) WithNotifier(n *notify.Notifier) *Handler { h.notifier = n; return h }

// ==================== GATEWAY ROUTER ====================

func (h *Handler) activeGateway() string {
	gw := h.cfg.PaymentGateway

	// Honor the explicitly configured gateway when its credentials are present.
	if gw == "stripe" && h.cfg.StripeSecretKey != "" {
		return "stripe"
	}
	if gw == "cashfree" && h.cfg.CashfreeAppID != "" {
		return "cashfree"
	}
	if gw == "razorpay" && h.cfg.RazorpayKeyID != "" {
		return "razorpay"
	}

	// Configured gateway is missing credentials. In production this is a
	// misconfiguration we must surface rather than silently swap gateways.
	if h.cfg.Environment == "production" {
		log.ErrorWithCode("GATEWAY", errcodes.EPayCreateFailed.Code,
			"Configured payment gateway has no credentials in production", "configured_gateway", gw)
	}

	// Dev/auto-detect: prefer whichever has credentials, else mock for local testing.
	if h.cfg.StripeSecretKey != "" {
		return "stripe"
	}
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
	// Receipt max 40 chars per Razorpay Orders API.
	if len(receipt) > 40 {
		receipt = receipt[:40]
	}
	payload := map[string]interface{}{
		"amount":   amountPaise, // paise — Orders API minimum is 100 (₹1)
		"currency": currency,
		"receipt":  receipt,
		// NOTE: docs show a `capture` param but the live API rejects it
		// ("extra_field_sent", verified 2026-08-30). Auto-capture must be enabled
		// in Dashboard -> Account & Settings -> Payment Capture instead.
		"notes": map[string]string{"source": "ecom-core-service", "receipt": receipt},
	}
	data, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", "https://api.razorpay.com/v1/orders", bytes.NewBuffer(data))
	if err != nil {
		return "", fmt.Errorf("request error: %w", err)
	}
	req.SetBasicAuth(h.cfg.RazorpayKeyID, h.cfg.RazorpaySecret)
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("razorpay API error: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("razorpay status %d: %s", resp.StatusCode, string(body))
	}
	var result map[string]interface{}
	json.Unmarshal(body, &result)
	orderID, _ := result["id"].(string)
	if orderID == "" {
		return "", fmt.Errorf("no order_id in razorpay response")
	}
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
			"return_url": fmt.Sprintf("%s?order_id=%s", h.cfg.CashfreeReturnURL, orderID),
		},
	}
	data, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", h.cashfreeBaseURL()+"/orders", bytes.NewBuffer(data))
	if err != nil {
		return "", "", fmt.Errorf("request error: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-client-id", h.cfg.CashfreeAppID)
	req.Header.Set("x-client-secret", h.cfg.CashfreeSecret)
	req.Header.Set("x-api-version", "2023-08-01")

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", "", fmt.Errorf("cashfree API error: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("cashfree status %d: %s", resp.StatusCode, string(body))
	}

	var result map[string]interface{}
	json.Unmarshal(body, &result)

	cfOrderID, _ := result["cf_order_id"].(float64)
	paySessionID, _ := result["payment_session_id"].(string)
	if paySessionID == "" {
		return "", "", fmt.Errorf("no payment_session_id in cashfree response")
	}

	log.Info("CASHFREE", "Order created", "cf_order_id", cfOrderID, "payment_session_id", paySessionID[:20]+"...", "amount", amountRupees)
	return fmt.Sprintf("%.0f", cfOrderID), paySessionID, nil
}

func (h *Handler) verifyCashfreePayment(orderID string) (string, error) {
	req, err := http.NewRequest("GET", h.cashfreeBaseURL()+"/orders/"+orderID, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("x-client-id", h.cfg.CashfreeAppID)
	req.Header.Set("x-client-secret", h.cfg.CashfreeSecret)
	req.Header.Set("x-api-version", "2023-08-01")

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
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

	// P0-3: Reject payment for cancelled/expired/abandoned orders.
	// (Verify deliberately still accepts abandoned — late-payment resurrection path.)
	if order.Status == "cancelled" || order.Status == "expired" || order.Status == "abandoned" {
		c.JSON(http.StatusConflict, gin.H{
			"error": fmt.Sprintf("Cannot create payment for %s order", order.Status),
			"code":  errcodes.EOrdExpired.Code,
		})
		return
	}

	if order.PaymentStatus == "paid" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Already paid"})
		return
	}

	// P0-1: Lazy expiry check — expire the order if TTL exceeded
	ttlMinutes := 30
	if h.cfg != nil && h.cfg.PendingOrderTTLMinutes > 0 {
		ttlMinutes = h.cfg.PendingOrderTTLMinutes
	}
	if order.Status == "placed" && order.PaymentStatus == "pending" {
		ttl := time.Duration(ttlMinutes) * time.Minute
		if time.Since(order.CreatedAt) > ttl {
			// Expire inline
			orderPkg.ExpireOrder(ctx, h.db, &order)
			c.JSON(http.StatusConflict, gin.H{
				"error": "Order has expired due to pending payment timeout",
				"code":  errcodes.EOrdExpired.Code,
			})
			return
		}
	}

	// For partial payment, charge only the advance_amount
	chargeAmount := order.Total
	if order.PaymentPlan == "partial" && order.AdvanceAmount > 0 {
		chargeAmount = order.AdvanceAmount
	}

	gateway := h.activeGateway()
	log.Info("CREATE", "Creating payment", "gateway", gateway, "order_id", req.OrderID, "charge_amount", chargeAmount, "total", order.Total)

	var gatewayOrderID, paymentSessionID, gatewayKeyID, stripeClientSecret, stripePaymentIntentID string

	switch gateway {
	case "stripe":
		paymentID := uuid.New().String()
		pi, err := h.CreatePaymentIntent(chargeAmount, "INR", req.OrderID, paymentID, order.OrderNumber)
		if err != nil {
			log.Error("CREATE", "Stripe PaymentIntent failed", "err", err.Error())
			c.JSON(http.StatusBadGateway, gin.H{"error": "Payment gateway error: " + err.Error()})
			return
		}
		stripePaymentIntentID = pi.ID
		stripeClientSecret = pi.ClientSecret
		gatewayOrderID = pi.ID

		payment := models.Payment{
			ID: paymentID, OrderID: order.ID, UserID: userID,
			RazorpayOrderID: stripePaymentIntentID, Amount: chargeAmount, Currency: "INR",
			Status: "created", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
		h.db.Collection("payments").InsertOne(ctx, payment)

		log.Info("CREATE", "Stripe payment created", "payment_id", paymentID, "pi_id", stripePaymentIntentID)
		c.JSON(http.StatusOK, gin.H{
			"gateway":         "stripe",
			"payment_id":      paymentID,
			"order_id":        order.ID,
			"amount":          chargeAmount,
			"currency":        "inr",
			"client_secret":   stripeClientSecret,
			"publishable_key": h.cfg.StripePublishableKey,
			"order_number":    order.OrderNumber,
		})
		return

	case "razorpay":
		rzpOrderID, err := h.createRazorpayOrder(chargeAmount, "INR", order.OrderNumber)
		if err != nil {
			log.Error("CREATE", "Razorpay order failed", "err", err.Error())
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Payment gateway error: " + err.Error()})
			return
		}
		gatewayOrderID = rzpOrderID
		gatewayKeyID = h.cfg.RazorpayKeyID

	case "cashfree":
		cfOrderID, sessionID, err := h.createCashfreeOrder(chargeAmount, "INR", order.ID, order.ShippingAddress.Phone, order.ShippingAddress.Name)
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
		RazorpayOrderID: gatewayOrderID, Amount: chargeAmount, Currency: "INR",
		Status: "created", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	h.db.Collection("payments").InsertOne(ctx, payment)

	response := gin.H{
		"payment_id":   payment.ID,
		"gateway":      gateway,
		"amount":       chargeAmount,
		"currency":     "INR",
		"order_id":     order.ID,
		"order_number": order.OrderNumber,
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
		// Stripe fields
		PaymentIntentID string `json:"payment_intent_id"`
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
	if gateway == "" {
		gateway = h.activeGateway()
	}

	switch gateway {
	case "stripe":
		h.verifyStripe(c, ctx, userID, req.OrderID, req.PaymentIntentID)
	case "razorpay":
		h.verifyRazorpay(c, ctx, userID, req.RazorpayOrderID, req.RazorpayPaymentID, req.RazorpaySignature)
	case "cashfree":
		h.verifyCashfree(c, ctx, userID, req.OrderID)
	case "mock":
		// Dev-only: trust the client and mark paid. Never allowed in production.
		if h.cfg.Environment == "production" {
			log.WarnWithCode("VERIFY", errcodes.EPayVerifyFailed.Code, "Mock gateway verify rejected in production")
			c.JSON(http.StatusBadRequest, gin.H{"error": "Unknown gateway"})
			return
		}
		h.verifyMock(c, ctx, userID, req.OrderID)
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "Unknown gateway"})
	}
}

// verifyMock marks the payment paid without gateway checks. Development only.
func (h *Handler) verifyMock(c *gin.Context, ctx context.Context, userID, orderID string) {
	if orderID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "order_id required"})
		return
	}
	var payment models.Payment
	if err := h.db.Collection("payments").FindOne(ctx, bson.M{"order_id": orderID, "user_id": userID}).Decode(&payment); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Payment not found"})
		return
	}
	// P0-3: Reject verify for cancelled/expired orders
	if rejected := h.rejectIfOrderInvalid(c, ctx, payment.OrderID); rejected {
		return
	}
	firstTransition := h.markPaymentPaid(ctx, payment.ID, payment.OrderID, "mock-"+payment.ID)
	log.Info("VERIFY", "Mock payment verified (dev only)", "payment_id", payment.ID, "order_id", payment.OrderID, "first_transition", firstTransition)
	c.JSON(http.StatusOK, gin.H{"message": "Payment verified (mock)", "order_id": payment.OrderID, "gateway": "mock"})
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

	// P0-3: Reject verify for cancelled/expired orders
	if rejected := h.rejectIfOrderInvalid(c, ctx, payment.OrderID); rejected {
		return
	}

	if h.cfg.RazorpaySecret == "" {
		// Fail closed in production: skipping signature verification would let
		// anyone mark orders paid. Dev/mock flows use the mock gateway instead.
		if h.cfg.Environment == "production" {
			log.ErrorWithCode("VERIFY", errcodes.EPayVerifyFailed.Code, "RAZORPAY_KEY_SECRET not set in production — rejecting verify")
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Payment verification not configured"})
			return
		}
	} else {
		// Per Razorpay docs: sign the order_id from OUR database (not the client
		// callback) and compare timing-safely.
		expected := hmacSHA256(payment.RazorpayOrderID+"|"+rzpPaymentID, h.cfg.RazorpaySecret)
		if !hmac.Equal([]byte(expected), []byte(rzpSignature)) {
			log.WarnWithCode("VERIFY", errcodes.EPayVerifyFailed.Code, "Invalid Razorpay signature")
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid payment signature", "code": errcodes.EPayVerifyFailed.Code})
			return
		}
	}

	firstTransition := h.markPaymentPaid(ctx, payment.ID, payment.OrderID, rzpPaymentID)
	log.Info("VERIFY", "Razorpay payment verified", "payment_id", payment.ID, "order_id", payment.OrderID, "first_transition", firstTransition)

	var order models.Order
	h.db.Collection("orders").FindOne(ctx, bson.M{"_id": payment.OrderID}).Decode(&order)
	if firstTransition && order.ShippingAddress.Phone != "" {
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

	// P0-3: Reject verify for cancelled/expired orders
	if rejected := h.rejectIfOrderInvalid(c, ctx, payment.OrderID); rejected {
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

	firstTransition := h.markPaymentPaid(ctx, payment.ID, payment.OrderID, "cf_"+orderID)
	log.Info("VERIFY", "Cashfree payment verified", "payment_id", payment.ID, "order_id", payment.OrderID, "first_transition", firstTransition)

	var order models.Order
	h.db.Collection("orders").FindOne(ctx, bson.M{"_id": payment.OrderID}).Decode(&order)
	if firstTransition && order.ShippingAddress.Phone != "" {
		utils.SendPaymentConfirmSMS(order.ShippingAddress.Phone, order.OrderNumber, order.Total)
	}

	c.JSON(http.StatusOK, gin.H{"message": "Payment verified", "order_id": payment.OrderID, "order_number": order.OrderNumber, "gateway": "cashfree"})
}

// rejectIfOrderInvalid checks if an order is in a terminal state (cancelled/expired)
// and returns a 409 if so. Returns true if the request was rejected.
func (h *Handler) rejectIfOrderInvalid(c *gin.Context, ctx context.Context, orderID string) bool {
	var order models.Order
	if err := h.db.Collection("orders").FindOne(ctx, bson.M{"_id": orderID}).Decode(&order); err != nil {
		return false // let the caller handle not-found
	}
	if order.Status == "cancelled" || order.Status == "expired" {
		c.JSON(http.StatusConflict, gin.H{
			"error": fmt.Sprintf("Cannot verify payment for %s order", order.Status),
			"code":  errcodes.EOrdExpired.Code,
		})
		return true
	}
	return false
}

// markPaymentPaid transitions a payment+order to paid. It is idempotent: the order is
// only updated while its payment_status is still "pending", so webhook retries and
// duplicate verify/webhook deliveries are safe and never regress an already-advanced
// order (e.g. one already "shipped"). Returns true only on the first transition, so
// callers can guard one-time side-effects like confirmation SMS.
//
// Abandoned orders (user dismissed the payment window, but a late UPI approval landed
// anyway): stock/coupon were already released, so they are atomically re-reserved
// first. If stock is gone, the payment is auto-refunded and the order stays dead.
func (h *Handler) markPaymentPaid(ctx context.Context, paymentID, orderID, gatewayPaymentID string) (firstTransition bool) {
	now := time.Now()

	// Resurrection path: late payment for an abandoned order.
	var pre models.Order
	if err := h.db.Collection("orders").FindOne(ctx, bson.M{"_id": orderID}).Decode(&pre); err == nil && pre.Status == "abandoned" {
		if !orderPkg.ReclaimStock(ctx, h.db, &pre) {
			// Stock got sold to someone else while the order was abandoned — refund.
			log.Warn("MARK_PAID", "Late payment for abandoned order but stock gone — refunding", "order_id", orderID)
			refundStatus := "refund_pending"
			var payRec models.Payment
			h.db.Collection("payments").FindOne(ctx, bson.M{"_id": paymentID}).Decode(&payRec)
			if payRec.Amount > 0 {
				if _, rErr := h.razorpayRefund(gatewayPaymentID, payRec.Amount); rErr == nil {
					refundStatus = "refunded"
				} else {
					log.Error("MARK_PAID", "Auto-refund of late payment failed — needs manual refund", "order_id", orderID, "payment_id", gatewayPaymentID, "err", rErr)
				}
			}
			h.db.Collection("orders").UpdateOne(ctx, bson.M{"_id": orderID, "status": "abandoned"},
				bson.M{"$set": bson.M{"status": "cancelled", "payment_status": refundStatus,
					"cancel_reason": "Late payment received but stock no longer available — amount refunded", "updated_at": now}})
			h.db.Collection("payments").UpdateOne(ctx, bson.M{"_id": paymentID},
				bson.M{"$set": bson.M{"razorpay_payment_id": gatewayPaymentID, "status": refundStatus, "updated_at": now}})
			return false
		}
		orderPkg.ReclaimCoupon(ctx, h.db, &pre)
		// Flip abandoned → placed so the standard transition below takes over.
		h.db.Collection("orders").UpdateOne(ctx, bson.M{"_id": orderID, "status": "abandoned"},
			bson.M{"$set": bson.M{"status": "placed", "updated_at": now}})
		log.Info("MARK_PAID", "Abandoned order resurrected by late payment", "order_id", orderID)
	}

	// Only flip the order if it hasn't already been paid. MatchedCount==0 means a
	// concurrent/duplicate delivery already handled it.
	res, err := h.db.Collection("orders").UpdateOne(ctx,
		bson.M{"_id": orderID, "payment_status": "pending"},
		bson.M{"$set": bson.M{
			"payment_id": gatewayPaymentID, "payment_status": "paid", "status": "confirmed", "updated_at": now,
		}})
	if err != nil {
		log.ErrorWithCode("MARK_PAID", errcodes.EPayWebhookFailed.Code, "Failed to update order to paid", "order_id", orderID, "err", err)
		return false
	}

	// Always keep the payment record in sync (safe to repeat).
	h.db.Collection("payments").UpdateOne(ctx, bson.M{"_id": paymentID}, bson.M{"$set": bson.M{
		"razorpay_payment_id": gatewayPaymentID, "status": "paid", "updated_at": now,
	}})

	if res.MatchedCount == 0 {
		log.Info("MARK_PAID", "Order already paid — skipping duplicate transition", "order_id", orderID)
		return false
	}

	// First transition to paid: clear the customer's cart (kept intact until now so a
	// dismissed payment returns them to an unchanged checkout) and fire the
	// order-confirmed notification exactly once.
	// markPaymentPaid is the single funnel for every success path (verify + webhooks).
	var order models.Order
	if err := h.db.Collection("orders").FindOne(ctx, bson.M{"_id": orderID}).Decode(&order); err == nil {
		// Guest orders have no server cart — skip cart-clear when UserID is empty.
		if order.UserID != "" {
			h.db.Collection("carts").UpdateOne(ctx, bson.M{"user_id": order.UserID},
				bson.M{"$set": bson.M{"items": []models.CartItem{}, "updated_at": now}})
		}
		if h.notifier != nil {
			// Guests have no user record: pass an empty user — the email channel
			// skips on empty address, WhatsApp uses order.ShippingAddress.Phone.
			var user models.User
			if order.UserID != "" {
				h.db.Collection("users").FindOne(ctx, bson.M{"_id": order.UserID}).Decode(&user)
			}
			h.notifier.OrderConfirmed(&order, &user)
		}
	}
	return true
}

// razorpayRefund issues a full/partial refund for a captured Razorpay payment.
func (h *Handler) razorpayRefund(razorpayPaymentID string, amountPaise int) (string, error) {
	payload, _ := json.Marshal(map[string]interface{}{"amount": amountPaise})
	req, err := http.NewRequest("POST",
		"https://api.razorpay.com/v1/payments/"+razorpayPaymentID+"/refund", bytes.NewBuffer(payload))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(h.cfg.RazorpayKeyID, h.cfg.RazorpaySecret)
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("razorpay refund API error: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("razorpay refund failed (%d): %s", resp.StatusCode, string(body))
	}
	var out struct {
		ID string `json:"id"`
	}
	json.Unmarshal(body, &out)
	return out.ID, nil
}

// Webhook — POST /payment/webhook (no auth — authenticated via gateway signature)
//
// Webhooks are unauthenticated at the network layer, so the gateway HMAC signature
// is the ONLY thing proving the request is genuine. We read the raw body once, verify
// the signature against it, and only then parse and act on it. Never trust an unverified
// webhook — a forged one could mark any order as paid.
func (h *Handler) Webhook(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		log.WarnWithCode("WEBHOOK", errcodes.EPayWebhookFailed.Code, "Failed to read webhook body")
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid body"})
		return
	}

	// Detect gateway: explicit ?gateway= wins, then sniff gateway-specific headers
	// (Stripe sends Stripe-Signature, Cashfree sends x-webhook-signature,
	// Razorpay sends X-Razorpay-Signature), finally fall back to the configured active gateway.
	gateway := c.Query("gateway")
	if gateway == "" {
		switch {
		case c.GetHeader("Stripe-Signature") != "":
			gateway = "stripe"
		case c.GetHeader("x-webhook-signature") != "":
			gateway = "cashfree"
		case c.GetHeader("X-Razorpay-Signature") != "":
			gateway = "razorpay"
		default:
			gateway = h.activeGateway()
		}
	}

	switch gateway {
	case "stripe":
		h.stripeWebhook(c, body)
	case "cashfree":
		h.cashfreeWebhook(c, body)
	default:
		h.razorpayWebhook(c, body)
	}
}

func (h *Handler) stripeWebhook(c *gin.Context, body []byte) {
	sigHeader := c.GetHeader("Stripe-Signature")

	// Fail closed in production if webhook secret is not configured.
	if h.cfg.StripeWebhookSecret == "" {
		if h.cfg.Environment == "production" {
			log.ErrorWithCode("WEBHOOK", errcodes.EPayWebhookFailed.Code, "STRIPE_WEBHOOK_SECRET not set in production — rejecting webhook")
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Webhook not configured"})
			return
		}
		// In development, allow unverified webhooks for testing
		log.Warn("WEBHOOK", "Stripe webhook secret not set — skipping signature verification (dev only)")
	} else {
		if _, err := VerifyStripeWebhookSignature(body, sigHeader, h.cfg.StripeWebhookSecret); err != nil {
			log.WarnWithCode("WEBHOOK", errcodes.EPayWebhookFailed.Code, "Invalid Stripe webhook signature", "err", err.Error(), "ip", c.ClientIP())
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid signature"})
			return
		}
	}

	var event struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &event); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid JSON"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	log.Info("WEBHOOK", "Stripe webhook received", "type", event.Type)

	switch event.Type {
	case "payment_intent.succeeded":
		var data struct {
			Object struct {
				ID       string            `json:"id"`
				Metadata map[string]string `json:"metadata"`
			} `json:"object"`
		}
		if err := json.Unmarshal(event.Data, &data); err == nil && data.Object.ID != "" {
			orderID := data.Object.Metadata["order_id"]
			paymentID := data.Object.Metadata["payment_id"]
			if orderID != "" {
				var p models.Payment
				filter := bson.M{"order_id": orderID}
				if paymentID != "" {
					filter["_id"] = paymentID
				}
				h.db.Collection("payments").FindOne(ctx, filter).Decode(&p)
				if p.OrderID != "" {
					h.markPaymentPaid(ctx, p.ID, p.OrderID, data.Object.ID)
					log.Info("WEBHOOK", "Stripe payment_intent.succeeded", "order_id", p.OrderID, "pi_id", data.Object.ID)
				}
			}
		}

	case "payment_intent.payment_failed":
		var data struct {
			Object struct {
				ID       string            `json:"id"`
				Metadata map[string]string `json:"metadata"`
			} `json:"object"`
		}
		if err := json.Unmarshal(event.Data, &data); err == nil && data.Object.ID != "" {
			orderID := data.Object.Metadata["order_id"]
			if orderID != "" {
				h.db.Collection("payments").UpdateOne(ctx,
					bson.M{"order_id": orderID, "razorpay_order_id": data.Object.ID},
					bson.M{"$set": bson.M{"status": "failed", "updated_at": time.Now()}})
				log.Warn("WEBHOOK", "Stripe payment_intent.payment_failed", "order_id", orderID, "pi_id", data.Object.ID)
			}
		}
	}

	// Return 200 for all event types (including unhandled)
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (h *Handler) razorpayWebhook(c *gin.Context, body []byte) {
	// Signature verification — required in production. If no secret is configured we
	// only allow it through in development (mock/local testing).
	signature := c.GetHeader("X-Razorpay-Signature")
	if h.cfg.RazorpayWebhookSecret != "" {
		expected := hmacSHA256(string(body), h.cfg.RazorpayWebhookSecret)
		if !hmac.Equal([]byte(expected), []byte(signature)) {
			log.WarnWithCode("WEBHOOK", errcodes.EPayWebhookFailed.Code, "Invalid Razorpay webhook signature", "ip", c.ClientIP())
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid signature"})
			return
		}
	} else if h.cfg.Environment == "production" {
		log.ErrorWithCode("WEBHOOK", errcodes.EPayWebhookFailed.Code, "RAZORPAY_WEBHOOK_SECRET not set in production — rejecting webhook")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Webhook not configured"})
		return
	}

	var payload struct {
		Event   string                 `json:"event"`
		Payload map[string]interface{} `json:"payload"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
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

func (h *Handler) cashfreeWebhook(c *gin.Context, body []byte) {
	// Cashfree signs base64( HMAC-SHA256( timestamp + rawBody, clientSecret ) ),
	// sent in x-webhook-signature with the timestamp in x-webhook-timestamp.
	signature := c.GetHeader("x-webhook-signature")
	timestamp := c.GetHeader("x-webhook-timestamp")
	if h.cfg.CashfreeSecret != "" {
		expected := hmacSHA256Base64(timestamp+string(body), h.cfg.CashfreeSecret)
		if !hmac.Equal([]byte(expected), []byte(signature)) {
			log.WarnWithCode("WEBHOOK", errcodes.EPayWebhookFailed.Code, "Invalid Cashfree webhook signature", "ip", c.ClientIP())
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid signature"})
			return
		}
	} else if h.cfg.Environment == "production" {
		log.ErrorWithCode("WEBHOOK", errcodes.EPayWebhookFailed.Code, "CASHFREE_SECRET_KEY not set in production — rejecting webhook")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Webhook not configured"})
		return
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
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

// verifyStripe validates a Stripe PaymentIntent server-side. The client sends the PI id
// and order_id; we retrieve the PI from Stripe and verify status+amount+metadata.
func (h *Handler) verifyStripe(c *gin.Context, ctx context.Context, userID, orderID, paymentIntentID string) {
	if orderID == "" || paymentIntentID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "order_id and payment_intent_id required"})
		return
	}

	var payment models.Payment
	if err := h.db.Collection("payments").FindOne(ctx, bson.M{"order_id": orderID, "user_id": userID}).Decode(&payment); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Payment not found"})
		return
	}

	// P0-3: Reject verify for cancelled/expired orders
	if rejected := h.rejectIfOrderInvalid(c, ctx, payment.OrderID); rejected {
		return
	}

	// Verify the payment_intent_id matches what we stored at create time
	if payment.RazorpayOrderID != paymentIntentID {
		log.WarnWithCode("VERIFY", errcodes.EPayVerifyFailed.Code, "Stripe PI mismatch", "stored", payment.RazorpayOrderID, "received", paymentIntentID)
		c.JSON(http.StatusBadRequest, gin.H{"error": "payment_intent_id mismatch", "code": errcodes.EPayVerifyFailed.Code})
		return
	}

	// Retrieve from Stripe and verify
	pi, err := h.RetrievePaymentIntent(paymentIntentID)
	if err != nil {
		log.Error("VERIFY", "Stripe retrieve failed", "err", err.Error())
		c.JSON(http.StatusBadGateway, gin.H{"error": "Failed to verify with Stripe"})
		return
	}

	if err := VerifyStripePaymentIntent(pi, payment.Amount, orderID); err != nil {
		log.WarnWithCode("VERIFY", errcodes.EPayVerifyFailed.Code, "Stripe verification failed", "reason", err.Error())
		c.JSON(http.StatusBadRequest, gin.H{"error": "Payment verification failed: " + err.Error(), "code": errcodes.EPayVerifyFailed.Code})
		return
	}

	firstTransition := h.markPaymentPaid(ctx, payment.ID, payment.OrderID, paymentIntentID)
	log.Info("VERIFY", "Stripe payment verified", "payment_id", payment.ID, "order_id", payment.OrderID, "first_transition", firstTransition)

	var order models.Order
	h.db.Collection("orders").FindOne(ctx, bson.M{"_id": payment.OrderID}).Decode(&order)
	if firstTransition && order.ShippingAddress.Phone != "" {
		utils.SendPaymentConfirmSMS(order.ShippingAddress.Phone, order.OrderNumber, order.Total)
	}

	c.JSON(http.StatusOK, gin.H{"message": "Payment verified", "order_id": payment.OrderID, "order_number": order.OrderNumber, "gateway": "stripe"})
}

// ==================== UTILS ====================

func hmacSHA256(data, secret string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))
}

// hmacSHA256Base64 computes a base64-encoded HMAC-SHA256 (used for Cashfree webhook verification).
func hmacSHA256Base64(data, secret string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(data))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}
