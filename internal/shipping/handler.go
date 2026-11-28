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

// PushOrder creates an unassigned Shipmozo order for manual courier selection.
func (h *Handler) PushOrder(c *gin.Context) {
	query := c.Request.URL.Query()
	query.Set("push_only", "true")
	c.Request.URL.RawQuery = query.Encode()
	h.CreateShipment(c)
}

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

	if h.cfg.Environment == "production" && (h.cfg.ShipmozoPublicKey == "" || h.cfg.ShipmozoPrivateKey == "") {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Shipping provider is not configured", "code": errcodes.EShipCreateFailed.Code})
		return
	}

	// Build billable weight from the products in the order snapshot. A guessed
	// 500 g parcel can undercharge shipping and produce carrier adjustments.
	productIDs := make([]string, 0, len(order.Items))
	for _, item := range order.Items {
		productIDs = append(productIDs, item.ProductID)
	}
	productsByID := make(map[string]models.Product, len(productIDs))
	cursor, productErr := h.db.Collection("products").Find(ctx, bson.M{"_id": bson.M{"$in": productIDs}})
	if productErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not load product shipping data", "code": errcodes.EShipCreateFailed.Code})
		return
	}
	var products []models.Product
	if err := cursor.All(ctx, &products); err != nil {
		cursor.Close(ctx)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not read product shipping data", "code": errcodes.EShipCreateFailed.Code})
		return
	}
	cursor.Close(ctx)
	for _, product := range products {
		productsByID[product.ID] = product
	}

	weightGrams, lengthCM, widthCM, heightCM := 0, 0, 0, 0
	for _, item := range order.Items {
		product := productsByID[item.ProductID]
		weight := item.Weight
		if weight <= 0 {
			weight = product.Weight
		}
		itemLength, itemWidth, itemHeight := item.LengthCM, item.WidthCM, item.HeightCM
		if itemLength <= 0 {
			itemLength = product.LengthCM
		}
		if itemWidth <= 0 {
			itemWidth = product.WidthCM
		}
		if itemHeight <= 0 {
			itemHeight = product.HeightCM
		}
		if weight <= 0 || itemLength <= 0 || itemWidth <= 0 || itemHeight <= 0 {
			if h.cfg.Environment == "production" {
				c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "Product weight and package dimensions are required before shipment creation", "product_id": item.ProductID, "code": errcodes.EShipCreateFailed.Code})
				return
			}
			weight, itemLength, itemWidth, itemHeight = 500, 30, 25, 5
		}
		weightGrams += weight * item.Quantity
		if itemLength > lengthCM {
			lengthCM = itemLength
		}
		if itemWidth > widthCM {
			widthCM = itemWidth
		}
		// Conservatively stack package heights. This avoids under-declaring the
		// parcel until a packing service supplies exact cartonization.
		heightCM += itemHeight * item.Quantity
	}

	// Claim shipment creation before calling the courier. This closes the
	// check-then-act race where two admin requests could buy two labels.
	lockCutoff := time.Now().Add(-10 * time.Minute)
	lockResult, lockErr := h.db.Collection("orders").UpdateOne(ctx, bson.M{
		"_id":                              order.ID,
		"status":                           bson.M{"$in": []string{"confirmed", "processing"}},
		"shipment_id":                      bson.M{"$in": []interface{}{"", nil}},
		"shipmozo_order_id":                bson.M{"$in": []interface{}{"", nil}},
		"shipment_reconciliation_required": bson.M{"$ne": true},
		"$or": []bson.M{
			{"shipment_booking": bson.M{"$ne": true}},
			{"shipment_booking_started_at": bson.M{"$lt": lockCutoff}},
		},
	}, bson.M{"$set": bson.M{"shipment_booking": true, "shipment_booking_started_at": time.Now()}})
	if lockErr != nil || lockResult.MatchedCount == 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "Shipment creation is already in progress or completed", "code": errcodes.EShprAlreadyShipped.Code})
		return
	}
	bookingComplete := false
	defer func() {
		if !bookingComplete {
			h.db.Collection("orders").UpdateOne(context.Background(), bson.M{"_id": order.ID, "shipment_booking": true}, bson.M{"$unset": bson.M{"shipment_booking": "", "shipment_booking_started_at": ""}})
		}
	}()

	request := ShipmentRequest{
		OrderNumber: order.OrderNumber, OrderDate: order.CreatedAt, PaymentType: paymentType, CODAmountPaise: codAmount,
		CustomerName: order.ShippingAddress.Name, CustomerPhone: order.ShippingAddress.Phone, Address: order.ShippingAddress,
		Items: order.Items, WeightGrams: weightGrams, LengthCM: lengthCM, WidthCM: widthCM, HeightCM: heightCM,
	}
	pushOnly := c.Query("push_only") == "true"
	var result ShipmentResult
	var err error
	if pushOnly {
		result, err = h.provider.PushOrder(ctx, request)
	} else {
		result, err = h.provider.CreateShipment(ctx, request)
	}
	if err != nil {
		// Provider timeouts are ambiguous: Shipmozo may have accepted the order.
		// Keep the claim locked until reconciliation by our order/reference ID.
		bookingComplete = true
		h.db.Collection("orders").UpdateOne(context.Background(), bson.M{"_id": order.ID}, bson.M{"$set": bson.M{"shipment_reconciliation_required": true, "shipment_reconciliation_error": err.Error(), "updated_at": time.Now()}})
		log.ErrorWithCode("SHIP", errcodes.EShipCreateFailed.Code, "Provider shipment creation failed; reconciliation required", "order_id", orderID, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errcodes.EShipCreateFailed.Message, "code": errcodes.EShipCreateFailed.Code})
		return
	}

	if pushOnly {
		updateResult, updateErr := h.db.Collection("orders").UpdateOne(ctx, bson.M{"_id": order.ID, "shipment_booking": true}, bson.M{"$set": bson.M{"shipmozo_order_id": result.ProviderOrderID, "shipmozo_push_status": "awaiting_courier", "status": "processing", "updated_at": time.Now()}, "$unset": bson.M{"shipment_booking": "", "shipment_booking_started_at": ""}})
		if updateErr != nil || updateResult.MatchedCount == 0 {
			bookingComplete = true
			h.db.Collection("orders").UpdateOne(context.Background(), bson.M{"_id": order.ID}, bson.M{"$set": bson.M{"shipment_reconciliation_required": true, "shipmozo_order_id": result.ProviderOrderID, "updated_at": time.Now()}})
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Order reached Shipmozo but local reconciliation is required", "provider_order_id": result.ProviderOrderID, "code": errcodes.EShipCreateFailed.Code})
			return
		}
		bookingComplete = true
		c.Set("audit_provider_reference", result.ProviderOrderID)
		c.JSON(http.StatusCreated, gin.H{"status": "awaiting_courier", "provider_order_id": result.ProviderOrderID, "message": "Order pushed to Shipmozo. Assign courier in Shipmozo dashboard."})
		return
	}

	shipment := models.Shipment{
		ID:                 uuid.New().String(),
		OrderID:            order.ID,
		Provider:           "shipmozo",
		AWB:                result.AWB,
		CourierName:        result.CourierName,
		TrackingURL:        result.TrackingURL,
		LabelURL:           result.LabelURL,
		Status:             "booked",
		ProviderStatus:     "booked",
		NextTrackingPollAt: time.Now().Add(60 * time.Minute),
		Weight:             weightGrams,
		Length:             lengthCM,
		Width:              widthCM,
		Height:             heightCM,
		CreatedAt:          time.Now(),
		UpdatedAt:          time.Now(),
	}
	if _, err := h.db.Collection("shipments").InsertOne(ctx, shipment); err != nil {
		bookingComplete = true // keep the lock: provider outcome must be reconciled before retry
		h.db.Collection("orders").UpdateOne(context.Background(), bson.M{"_id": order.ID}, bson.M{"$set": bson.M{"shipment_reconciliation_required": true, "shipment_reconciliation_awb": result.AWB, "updated_at": time.Now()}})
		log.ErrorWithCode("SHIP", errcodes.EShipCreateFailed.Code, "Shipment booked but local record failed", "order_id", order.ID, "awb", result.AWB, "err", err)
		c.Set("audit_provider_reference", result.AWB)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Shipment was booked but requires reconciliation", "awb": result.AWB, "code": errcodes.EShipCreateFailed.Code})
		return
	}

	// Update the claimed order and clear the booking lock in the same write.
	updateResult, updateErr := h.db.Collection("orders").UpdateOne(ctx,
		bson.M{"_id": order.ID, "shipment_booking": true},
		bson.M{
			"$set": bson.M{
				"tracking_id": shipment.AWB, "tracking_url": shipment.TrackingURL,
				"shipment_id": shipment.ID, "status": "shipped", "updated_at": time.Now(),
			},
			"$unset": bson.M{"shipment_booking": "", "shipment_booking_started_at": ""},
		})
	if updateErr != nil || updateResult.MatchedCount == 0 {
		bookingComplete = true // keep the lock: the booked AWB requires reconciliation
		h.db.Collection("orders").UpdateOne(context.Background(), bson.M{"_id": order.ID}, bson.M{"$set": bson.M{"shipment_reconciliation_required": true, "shipment_reconciliation_awb": result.AWB, "updated_at": time.Now()}})
		log.ErrorWithCode("SHIP", errcodes.EShipCreateFailed.Code, "Shipment booked but order update failed", "order_id", order.ID, "awb", result.AWB, "err", updateErr)
		c.Set("audit_provider_reference", result.AWB)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Shipment was booked but order reconciliation requires attention", "awb": result.AWB, "code": errcodes.EShipCreateFailed.Code})
		return
	}
	bookingComplete = true

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

	c.Set("audit_provider_reference", result.AWB)
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
	// Customer refreshes never call Shipmozo synchronously. A stale snapshot is
	// marked due for the leased poller, preventing browser-driven API stampedes.
	if !shipment.TrackingTerminal && (shipment.LastTrackingPollAt == nil || time.Since(*shipment.LastTrackingPollAt) > 15*time.Minute) {
		h.db.Collection("shipments").UpdateOne(ctx, bson.M{"_id": shipment.ID}, bson.M{"$set": bson.M{"next_tracking_poll_at": time.Now()}})
	}

	events := shipment.Events
	if events == nil {
		events = []models.TrackingEvent{}
	}
	c.JSON(http.StatusOK, gin.H{
		"status": shipment.Status, "provider_status": shipment.ProviderStatus,
		"awb": shipment.AWB, "courier": shipment.CourierName,
		"tracking_url": shipment.TrackingURL, "expected_delivery_date": shipment.EstDelivery,
		"last_updated": shipment.LastTrackingPollAt, "events": events,
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
