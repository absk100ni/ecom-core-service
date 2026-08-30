package guest

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
	"regexp"
	"time"

	"ecom-core-service/internal/config"
	"ecom-core-service/internal/models"
	"ecom-core-service/internal/order"
	"ecom-core-service/internal/payment"
	"ecom-core-service/pkg/errcodes"
	"ecom-core-service/pkg/logger"
	"ecom-core-service/pkg/utils"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

var log = logger.New("GUEST", "CHECKOUT")

var phoneRegex = regexp.MustCompile(`^\d{10}$`)
var pincodeRegex = regexp.MustCompile(`^\d{6}$`)

// Handler holds dependencies for guest checkout routes.
type Handler struct {
	db       *mongo.Database
	cfg      *config.Config
	orderH   *order.Handler
	paymentH *payment.Handler
}

// NewHandler creates a guest checkout handler.
func NewHandler(db *mongo.Database, cfg *config.Config, orderH *order.Handler, paymentH *payment.Handler) *Handler {
	return &Handler{db: db, cfg: cfg, orderH: orderH, paymentH: paymentH}
}

// ==================== GUEST TOKEN ====================

// GenerateGuestToken produces a stateless HMAC-SHA256 token: hex(HMAC(secret, orderID|phone)).
func GenerateGuestToken(secret, orderID, phone string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(orderID + "|" + phone))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyGuestToken checks the token is valid for the given order+phone pair.
func VerifyGuestToken(secret, orderID, phone, token string) bool {
	expected := GenerateGuestToken(secret, orderID, phone)
	return hmac.Equal([]byte(expected), []byte(token))
}

func (h *Handler) tokenSecret() string {
	s := h.cfg.GuestTokenSecret
	if s == "" || s == "dev-guest-secret" {
		if h.cfg.Environment == "production" {
			log.Error("TOKEN", "GUEST_TOKEN_SECRET not set in production — rejecting")
			return ""
		}
	}
	return s
}

// verifyTokenForOrder loads the order by ID, verifies the guest token against the
// order's shipping phone, and returns the order. Writes an error response on failure.
func (h *Handler) verifyTokenForOrder(c *gin.Context, ctx context.Context, orderID, guestToken string) (*models.Order, bool) {
	secret := h.tokenSecret()
	if secret == "" && h.cfg.Environment == "production" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Guest checkout not configured"})
		return nil, false
	}

	var o models.Order
	if err := h.db.Collection("orders").FindOne(ctx, bson.M{"_id": orderID, "is_guest": true}).Decode(&o); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found", "code": errcodes.EOrdNotFound.Code})
		return nil, false
	}

	if !VerifyGuestToken(secret, orderID, o.ShippingAddress.Phone, guestToken) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid guest token", "code": errcodes.EGstInvalidToken.Code})
		return nil, false
	}
	return &o, true
}

// ==================== 1. POST /guest/orders ====================

type guestOrderRequest struct {
	Items           []guestItem    `json:"items" binding:"required"`
	ShippingAddress models.Address `json:"shipping_address" binding:"required"`
	PaymentPlan     string         `json:"payment_plan,omitempty"`
	CouponCode      string         `json:"coupon_code,omitempty"`
}

type guestItem struct {
	ProductID string `json:"product_id" binding:"required"`
	Quantity  int    `json:"quantity" binding:"required,gte=1"`
}

// CreateOrder handles POST /api/v1/guest/orders
func (h *Handler) CreateOrder(c *gin.Context) {
	var req guestOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error(), "code": errcodes.EGstInvalidItems.Code})
		return
	}

	// Validate item count
	if len(req.Items) == 0 || len(req.Items) > 20 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Items must contain 1-20 products", "code": errcodes.EGstInvalidItems.Code})
		return
	}

	// Validate address
	addr := req.ShippingAddress
	if addr.Name == "" || addr.Line1 == "" || addr.City == "" || addr.State == "" || addr.Pincode == "" || addr.Phone == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Complete shipping address required (name, line1, city, state, pincode, phone)", "code": errcodes.EOrdAddressInvalid.Code})
		return
	}
	if !phoneRegex.MatchString(addr.Phone) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Phone must be exactly 10 digits", "code": errcodes.EGstInvalidPhone.Code})
		return
	}
	if !pincodeRegex.MatchString(addr.Pincode) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Pincode must be exactly 6 digits", "code": errcodes.EOrdAddressInvalid.Code})
		return
	}

	// Validate payment plan
	paymentPlan := req.PaymentPlan
	if paymentPlan == "" {
		paymentPlan = "partial"
	}
	if paymentPlan != "full" && paymentPlan != "partial" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "payment_plan must be 'full' or 'partial'", "code": errcodes.EPartInvalidPlan.Code})
		return
	}

	// Resolve product details to build CartItem slice (guests don't have a server cart)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cartItems := make([]models.CartItem, 0, len(req.Items))
	for _, gi := range req.Items {
		var prod models.Product
		if err := h.db.Collection("products").FindOne(ctx, bson.M{"_id": gi.ProductID, "is_active": true}).Decode(&prod); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Product %s not found or inactive", gi.ProductID), "code": errcodes.EGstInvalidItems.Code})
			return
		}
		img := ""
		if len(prod.Images) > 0 {
			img = prod.Images[0]
		}
		cartItems = append(cartItems, models.CartItem{
			ProductID: prod.ID,
			Name:      prod.Name,
			Price:     prod.Price,
			Quantity:  gi.Quantity,
			Image:     img,
		})
	}

	// Place the order using the shared core (same atomic stock reservation)
	o, stockErr, err := h.orderH.PlaceOrder(ctx, "", cartItems, addr, req.CouponCode, "", paymentPlan, true)
	if stockErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Stock check failed", "code": errcodes.EOrdStockFailed.Code, "details": stockErr})
		return
	}
	if err != nil {
		log.Error("CREATE", "Guest order creation failed", "err", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create order", "code": errcodes.EOrdCreateFailed.Code})
		return
	}

	// Generate stateless guest token
	secret := h.tokenSecret()
	token := GenerateGuestToken(secret, o.ID, addr.Phone)

	log.Info("CREATE", "Guest order placed", "order_id", o.ID, "order_number", o.OrderNumber, "phone", addr.Phone, "total", o.Total)

	// SMS confirmation (async, best-effort)
	go utils.SendOrderConfirmationSMS(addr.Phone, o.OrderNumber, o.Total)

	c.JSON(http.StatusCreated, gin.H{"order": o, "guest_token": token})
}

// ==================== 2. POST /guest/payment/create ====================

// CreatePayment handles POST /api/v1/guest/payment/create
func (h *Handler) CreatePayment(c *gin.Context) {
	var req struct {
		OrderID    string `json:"order_id" binding:"required"`
		GuestToken string `json:"guest_token" binding:"required"`
	}

	// Read raw body so we can rebuffer it for the payment handler.
	rawBody, readErr := c.GetRawData()
	if readErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	if err := json.Unmarshal(rawBody, &req); err != nil || req.OrderID == "" || req.GuestToken == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "order_id and guest_token required"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	o, ok := h.verifyTokenForOrder(c, ctx, req.OrderID, req.GuestToken)
	if !ok {
		return
	}

	// Rebuffer body for the downstream payment handler to re-read.
	c.Request.Body = io.NopCloser(bytes.NewReader(rawBody))
	c.Set("user_id", o.UserID)
	h.paymentH.Create(c)
}

// ==================== 3. POST /guest/payment/verify ====================

// VerifyPayment handles POST /api/v1/guest/payment/verify
func (h *Handler) VerifyPayment(c *gin.Context) {
	rawBody, readErr := c.GetRawData()
	if readErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	var peek struct {
		OrderID    string `json:"order_id"`
		GuestToken string `json:"guest_token"`
	}
	if err := json.Unmarshal(rawBody, &peek); err != nil || peek.GuestToken == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "guest_token required"})
		return
	}

	orderID := peek.OrderID
	if orderID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "order_id required for guest verify"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, ok := h.verifyTokenForOrder(c, ctx, orderID, peek.GuestToken)
	if !ok {
		return
	}

	c.Request.Body = io.NopCloser(bytes.NewReader(rawBody))
	c.Set("user_id", "")
	h.paymentH.Verify(c)
}

// ==================== 4. POST /guest/orders/:id/abandon ====================

// Abandon handles POST /api/v1/guest/orders/:id/abandon
func (h *Handler) Abandon(c *gin.Context) {
	orderID := c.Param("id")
	var req struct {
		GuestToken string `json:"guest_token" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "guest_token required"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	o, ok := h.verifyTokenForOrder(c, ctx, orderID, req.GuestToken)
	if !ok {
		return
	}

	if o.PaymentStatus == "paid" {
		c.JSON(http.StatusOK, gin.H{"paid": true, "status": o.Status})
		return
	}

	if order.AbandonOrder(ctx, h.db, o) {
		c.JSON(http.StatusOK, gin.H{"paid": false, "abandoned": true})
		return
	}

	// Re-read to report truth
	h.db.Collection("orders").FindOne(ctx, bson.M{"_id": orderID}).Decode(o)
	c.JSON(http.StatusOK, gin.H{"paid": o.PaymentStatus == "paid", "status": o.Status})
}

// ==================== 5. GET /guest/orders/track ====================

// Track handles GET /api/v1/guest/orders/track?order_number=X&phone=Y
func (h *Handler) Track(c *gin.Context) {
	orderNumber := c.Query("order_number")
	phone := c.Query("phone")

	if orderNumber == "" || phone == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "order_number and phone are required"})
		return
	}
	if !phoneRegex.MatchString(phone) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Phone must be exactly 10 digits", "code": errcodes.EGstInvalidPhone.Code})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var o models.Order
	if err := h.db.Collection("orders").FindOne(ctx, bson.M{
		"order_number":         orderNumber,
		"shipping_address.phone": phone,
	}).Decode(&o); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found or phone does not match", "code": errcodes.EGstTrackDenied.Code})
		return
	}

	// Safe subset — no user data, no internal IDs
	type safeItem struct {
		Name     string `json:"name"`
		Quantity int    `json:"quantity"`
		Image    string `json:"image,omitempty"`
		Price    int    `json:"price"`
	}
	items := make([]safeItem, len(o.Items))
	for i, it := range o.Items {
		items[i] = safeItem{Name: it.Name, Quantity: it.Quantity, Image: it.Image, Price: it.Price}
	}

	// Fetch tracking events if shipped
	var tracking *struct {
		AWB         string                `json:"awb,omitempty"`
		CourierName string                `json:"courier_name,omitempty"`
		Events      []models.TrackingEvent `json:"events,omitempty"`
	}
	if o.ShipmentID != "" {
		var shipment models.Shipment
		if err := h.db.Collection("shipments").FindOne(ctx, bson.M{"order_id": o.ID}).Decode(&shipment); err == nil {
			tracking = &struct {
				AWB         string                `json:"awb,omitempty"`
				CourierName string                `json:"courier_name,omitempty"`
				Events      []models.TrackingEvent `json:"events,omitempty"`
			}{
				AWB:         shipment.AWB,
				CourierName: shipment.CourierName,
				Events:      shipment.Events,
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"order_number":   o.OrderNumber,
		"status":         o.Status,
		"payment_status": o.PaymentStatus,
		"items":          items,
		"total":          o.Total,
		"advance_amount": o.AdvanceAmount,
		"cod_amount":     o.CODAmount,
		"created_at":     o.CreatedAt,
		"tracking":       tracking,
	})
}
