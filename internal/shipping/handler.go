package shipping

import (
	"context"
	"crypto/subtle"
	"net/http"
	"time"

	"ecom-core-service/internal/config"
	"ecom-core-service/internal/models"
	"ecom-core-service/internal/notify"
	"ecom-core-service/pkg/cache"
	"ecom-core-service/pkg/errcodes"
	"ecom-core-service/pkg/logger"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

var log = logger.New("SHIPPING", "LOGISTICS")
var _ = errcodes.EShipCreateFailed

type Handler struct {
	db       *mongo.Database
	cfg      *config.Config
	cache    *cache.Cache
	provider CourierProvider
	notifier *notify.Notifier
}

func NewHandler(db *mongo.Database, cfg *config.Config, c *cache.Cache) *Handler {
	return &Handler{db: db, cfg: cfg, cache: c, provider: ProviderFromConfig(cfg)}
}

// WithNotifier injects the notification service (optional — nil means no notifications).
func (h *Handler) WithNotifier(n *notify.Notifier) *Handler { h.notifier = n; return h }

// CreateShipment — POST /api/v1/admin/orders/:id/ship (IDEMPOTENT)
func (h *Handler) CreateShipment(c *gin.Context) {
	orderID := c.Param("id")
	if orderID == "" {
		// Fallback for old route /admin/shipping/create
		var req struct {
			OrderID string `json:"order_id" binding:"required"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "order_id required"})
			return
		}
		orderID = req.OrderID
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Check for existing shipment (idempotent)
	var existingShipment models.Shipment
	if err := h.db.Collection("shipments").FindOne(ctx, bson.M{"order_id": orderID}).Decode(&existingShipment); err == nil {
		log.Warn("SHIP", "Order already shipped — idempotent reject", "order_id", orderID, "awb", existingShipment.AWB)
		c.JSON(http.StatusConflict, gin.H{"error": errcodes.EShprAlreadyShipped.Message, "code": errcodes.EShprAlreadyShipped.Code, "shipment": existingShipment})
		return
	}

	var order models.Order
	if err := h.db.Collection("orders").FindOne(ctx, bson.M{"_id": orderID}).Decode(&order); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found", "code": errcodes.EOrdNotFound.Code})
		return
	}

	// Validate: order must be confirmed and advance paid
	if order.Status != "confirmed" && order.Status != "processing" {
		log.WarnWithCode("SHIP", errcodes.EShprNotReady.Code, "Order not ready to ship", "order_id", orderID, "status", order.Status)
		c.JSON(http.StatusBadRequest, gin.H{"error": errcodes.EShprNotReady.Message, "code": errcodes.EShprNotReady.Code})
		return
	}
	if order.PaymentStatus != "paid" && order.AdvanceAmount > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Advance payment not received", "code": errcodes.EShprNotReady.Code})
		return
	}

	// Determine payment type and COD amount
	paymentType := "PREPAID"
	codAmount := 0
	if order.CODAmount > 0 {
		paymentType = "COD"
		codAmount = order.CODAmount
	}

	// Call provider
	result, err := h.provider.CreateShipment(ctx, ShipmentRequest{
		OrderNumber:    order.OrderNumber,
		OrderDate:      order.CreatedAt,
		PaymentType:    paymentType,
		CODAmountPaise: codAmount,
		CustomerName:   order.ShippingAddress.Name,
		CustomerPhone:  order.ShippingAddress.Phone,
		Address:        order.ShippingAddress,
		Items:          order.Items,
		WeightGrams:    500, // default
		LengthCM:       30,
		WidthCM:        25,
		HeightCM:       5,
	})
	if err != nil {
		log.ErrorWithCode("SHIP", errcodes.EShipCreateFailed.Code, "Provider shipment creation failed", "order_id", orderID, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errcodes.EShipCreateFailed.Message, "code": errcodes.EShipCreateFailed.Code})
		return
	}

	shipment := models.Shipment{
		ID:          uuid.New().String(),
		OrderID:     order.ID,
		Provider:    "shipmozo",
		AWB:         result.AWB,
		CourierName: result.CourierName,
		TrackingURL: result.TrackingURL,
		LabelURL:    result.LabelURL,
		Status:      "booked",
		Weight:      500,
		Length:      30,
		Width:       25,
		Height:      5,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	h.db.Collection("shipments").InsertOne(ctx, shipment)

	// Update order to shipped state-machine-safely (only if still confirmed/processing)
	h.db.Collection("orders").UpdateOne(ctx,
		bson.M{"_id": order.ID, "status": bson.M{"$in": []string{"confirmed", "processing"}}},
		bson.M{"$set": bson.M{
			"tracking_id": shipment.AWB, "tracking_url": shipment.TrackingURL,
			"shipment_id": shipment.ID, "status": "shipped", "updated_at": time.Now(),
		}})

	log.Info("SHIP", "Shipment created", "order_id", order.ID, "awb", result.AWB, "courier", result.CourierName)

	if h.notifier != nil {
		// Guest orders have no user record — empty user is fine: email skips,
		// WhatsApp goes to order.ShippingAddress.Phone.
		var user models.User
		if order.UserID != "" {
			h.db.Collection("users").FindOne(ctx, bson.M{"_id": order.UserID}).Decode(&user)
		}
		h.notifier.OrderShipped(&order, &user, result.AWB, result.CourierName, result.TrackingURL)
	}

	c.JSON(http.StatusCreated, shipment)
}

// Track — GET /api/v1/orders/:id/tracking (authenticated, owner-only)
func (h *Handler) Track(c *gin.Context) {
	orderID := c.Param("id")
	if orderID == "" {
		orderID = c.Param("orderId")
	}
	userID := c.GetString("user_id")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Verify ownership
	var order models.Order
	if err := h.db.Collection("orders").FindOne(ctx, bson.M{"_id": orderID, "user_id": userID}).Decode(&order); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found", "code": errcodes.EOrdNotFound.Code})
		return
	}

	var shipment models.Shipment
	if err := h.db.Collection("shipments").FindOne(ctx, bson.M{"order_id": orderID}).Decode(&shipment); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Shipment not found", "code": errcodes.EShipNotFound.Code})
		return
	}

	events := shipment.Events
	if events == nil {
		events = []models.TrackingEvent{}
	}
	c.JSON(http.StatusOK, gin.H{
		"status":       shipment.Status,
		"awb":          shipment.AWB,
		"courier":      shipment.CourierName,
		"tracking_url": shipment.TrackingURL,
		"events":       events,
	})
}

// Webhook — POST /shipping/webhook (authenticated via x-api-key, updates shipment by AWB)
func (h *Handler) Webhook(c *gin.Context) {
	if h.cfg.ShippingWebhookToken != "" {
		got := c.GetHeader("x-api-key")
		if subtle.ConstantTimeCompare([]byte(got), []byte(h.cfg.ShippingWebhookToken)) != 1 {
			log.WarnWithCode("WEBHOOK", errcodes.EShipWebhookFailed.Code, "Invalid shipping webhook token", "ip", c.ClientIP())
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
			return
		}
	} else if h.cfg.Environment == "production" {
		log.ErrorWithCode("WEBHOOK", errcodes.EShipWebhookFailed.Code, "SHIPPING_WEBHOOK_TOKEN not set in production — rejecting webhook")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Webhook not configured"})
		return
	}

	var payload struct {
		AWB           string `json:"awb"`
		CurrentStatus string `json:"current_status"`
		Description   string `json:"description"`
		Timestamp     string `json:"timestamp"`
	}
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid payload"})
		return
	}
	if payload.AWB == "" || payload.CurrentStatus == "" {
		c.JSON(http.StatusOK, gin.H{"status": "ignored"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ts, _ := time.Parse(time.RFC3339, payload.Timestamp)
	if ts.IsZero() {
		ts = time.Now()
	}

	event := models.TrackingEvent{
		Status:      payload.CurrentStatus,
		Description: payload.Description,
		Timestamp:   ts,
	}

	// Update shipment status and append event
	res, err := h.db.Collection("shipments").UpdateOne(ctx,
		bson.M{"awb": payload.AWB},
		bson.M{
			"$set":  bson.M{"status": payload.CurrentStatus, "updated_at": time.Now()},
			"$push": bson.M{"events": event},
		})
	if err != nil {
		log.ErrorWithCode("WEBHOOK", errcodes.EShipWebhookFailed.Code, "Failed to update shipment", "awb", payload.AWB, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Update failed"})
		return
	}

	// On delivered: update order + mark COD collected + set delivered_at
	if payload.CurrentStatus == "delivered" && res.MatchedCount > 0 {
		var shipment models.Shipment
		h.db.Collection("shipments").FindOne(ctx, bson.M{"awb": payload.AWB}).Decode(&shipment)
		if shipment.OrderID != "" {
			now := time.Now()
			update := bson.M{"status": "delivered", "updated_at": now, "delivered_at": now}
			// Mark COD collected if there was a COD amount
			var order models.Order
			if err := h.db.Collection("orders").FindOne(ctx, bson.M{"_id": shipment.OrderID}).Decode(&order); err == nil && order.CODAmount > 0 {
				update["cod_collected"] = true
			}
			h.db.Collection("orders").UpdateOne(ctx, bson.M{"_id": shipment.OrderID}, bson.M{"$set": update})
		}
	}

	log.Info("WEBHOOK", "Shipment status updated", "awb", payload.AWB, "status", payload.CurrentStatus, "matched", res.MatchedCount)
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
