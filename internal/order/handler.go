package order

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"regexp"
	"strings"
	"time"

	"ecom-core-service/internal/config"
	"ecom-core-service/internal/models"
	"ecom-core-service/internal/notify"
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

// nonDigits strips everything but digits from a search query ("ORD 98264" → "98264").
var nonDigits = regexp.MustCompile(`\D`)

// Shipping thresholds in paise — single source of truth server-side.
// Must match storefront src/config/constants.ts (₹999 free-shipping, ₹49 charge).
const (
	freeShippingThreshold = 99900
	shippingCharge        = 4900
)

type Handler struct {
	db       *mongo.Database
	cfg      *config.Config
	useTxn   bool // true when the Mongo deployment supports multi-document transactions (replica set)
	notifier *notify.Notifier
}

func NewHandler(db *mongo.Database, cfg *config.Config) *Handler {
	return &Handler{db: db, cfg: cfg, useTxn: supportsTransactions(db)}
}

// WithNotifier injects the notification service (optional — nil means no notifications).
func (h *Handler) WithNotifier(n *notify.Notifier) *Handler { h.notifier = n; return h }

// supportsTransactions probes whether the connected deployment is a replica set / sharded
// cluster (a prerequisite for multi-document transactions). Standalone mongod — common in
// local dev — cannot do transactions, so we detect once at startup and fall back to manual
// compensating rollbacks instead of failing every checkout.
func supportsTransactions(db *mongo.Database) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var res bson.M
	if err := db.RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&res); err != nil {
		log.Warn("INIT", "hello command failed; assuming no transaction support", "err", err)
		return false
	}
	// A replica set reports setName; mongos reports msg=isdbgrid.
	if _, ok := res["setName"]; ok {
		log.Info("INIT", "Replica set detected — checkout will use transactions")
		return true
	}
	if msg, _ := res["msg"].(string); msg == "isdbgrid" {
		log.Info("INIT", "Sharded cluster detected — checkout will use transactions")
		return true
	}
	log.Warn("INIT", "Standalone MongoDB — checkout uses manual rollback (no transactions). Use a replica set in production.")
	return false
}

func genOrderNumber() string {
	return fmt.Sprintf("ORD-%d%04d", time.Now().Unix()%100000, rand.Intn(10000))
}

func (h *Handler) Create(c *gin.Context) {
	userID := c.GetString("user_id")
	var req struct {
		ShippingAddress models.Address `json:"shipping_address" binding:"required"`
		CouponCode      string         `json:"coupon_code,omitempty"`
		Notes           string         `json:"notes,omitempty"`
		PaymentPlan     string         `json:"payment_plan,omitempty"` // "full" or "partial" (default)
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

	log.Info("CREATE", "Starting order creation", "user_id", userID, "cart_items", len(cart.Items), "txn", h.useTxn)

	// Default payment plan
	paymentPlan := req.PaymentPlan
	if paymentPlan == "" {
		paymentPlan = "partial"
	}
	if paymentPlan != "full" && paymentPlan != "partial" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "payment_plan must be 'full' or 'partial'", "code": errcodes.EPartInvalidPlan.Code})
		return
	}

	order, stockErr, err := h.placeOrder(ctx, userID, cart.Items, req.ShippingAddress, req.CouponCode, req.Notes, paymentPlan, false)
	if stockErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Stock check failed", "code": errcodes.EOrdStockFailed.Code, "details": stockErr})
		return
	}
	if err != nil {
		log.ErrorWithCode("CREATE", errcodes.EOrdCreateFailed.Code, "Order creation failed", "user_id", userID, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create order", "code": errcodes.EOrdCreateFailed.Code})
		return
	}

	log.Info("CREATE", "Order placed successfully", "order_id", order.ID, "order_number", order.OrderNumber, "user_id", userID, "total", order.Total, "items_count", len(order.Items))

	// Send order confirmation SMS via MSG91
	go utils.SendOrderConfirmationSMS(req.ShippingAddress.Phone, order.OrderNumber, order.Total)

	c.JSON(http.StatusCreated, order)
}

// PlaceOrder is the exported entry point for placing an order. Used by both the authed
// Create handler and the guest checkout handler. cartItems is the list of items to
// order. isGuest=true marks the order as a guest order (no user record).
func (h *Handler) PlaceOrder(ctx context.Context, userID string, cartItems []models.CartItem, addr models.Address, couponCode, notes, paymentPlan string, isGuest bool) (*models.Order, []string, error) {
	return h.placeOrder(ctx, userID, cartItems, addr, couponCode, notes, paymentPlan, isGuest)
}

// placeOrder runs the checkout writes (stock decrement, coupon, order insert, cart clear)
// atomically. On a replica set it uses a real multi-document transaction — any failure
// aborts and rolls back everything. On standalone MongoDB it falls back to the original
// best-effort manual compensating rollback. Returns (order, stockDetails, err): stockDetails
// is non-nil only for an out-of-stock condition (client 400), err for any other failure.
func (h *Handler) placeOrder(ctx context.Context, userID string, cartItems []models.CartItem, addr models.Address, couponCode, notes, paymentPlan string, isGuest bool) (*models.Order, []string, error) {
	if h.useTxn {
		return h.placeOrderTxn(ctx, userID, cartItems, addr, couponCode, notes, paymentPlan, isGuest)
	}
	return h.placeOrderManual(ctx, userID, cartItems, addr, couponCode, notes, paymentPlan, isGuest)
}

// buildOrder performs the stock decrements and assembles the order. It returns the order,
// the products it decremented (for compensating rollback in the non-txn path), an
// out-of-stock detail slice, and a hard error. It does NOT insert the order or clear the
// cart — the caller sequences those so the txn/manual paths can share this logic.
//
// cartItems is the list of items to place — from the server cart (authed) or from the
// request body (guest). The isGuest flag marks the order so downstream code (e.g.
// markPaymentPaid) knows not to look for a user record or server cart.
func (h *Handler) buildOrder(ctx context.Context, userID string, cartItems []models.CartItem, addr models.Address, couponCode, notes, paymentPlan string, isGuest bool) (order *models.Order, decremented []models.OrderItem, stockDetails []string, err error) {
	items := make([]models.OrderItem, 0, len(cartItems))
	subtotal := 0
	maxAdvancePercent := 0

	for _, ci := range cartItems {
		var product models.Product
		decErr := h.db.Collection("products").FindOneAndUpdate(ctx,
			bson.M{"_id": ci.ProductID, "is_active": true, "stock": bson.M{"$gte": ci.Quantity}},
			bson.M{"$inc": bson.M{"stock": -ci.Quantity}},
			options.FindOneAndUpdate().SetReturnDocument(options.Before),
		).Decode(&product)

		if decErr != nil {
			log.WarnWithCode("CREATE", errcodes.EOrdStockFailed.Code, "Stock check failed for item", "user_id", userID, "product", ci.Name, "product_id", ci.ProductID, "requested_qty", ci.Quantity)
			return nil, items, []string{fmt.Sprintf("%s: insufficient stock or not found", ci.Name)}, nil
		}

		// Track max advance_percent across items
		if product.AdvancePercent != nil && *product.AdvancePercent > maxAdvancePercent {
			maxAdvancePercent = *product.AdvancePercent
		}

		items = append(items, models.OrderItem{
			ProductID: ci.ProductID, VariantID: ci.VariantID,
			Name: ci.Name, SKU: product.SKU, Price: ci.Price,
			Quantity: ci.Quantity, Image: ci.Image,
		})
		subtotal += ci.Price * ci.Quantity
	}

	discount := 0
	couponUsed := false
	if couponCode != "" {
		var coupon models.Coupon
		cErr := h.db.Collection("coupons").FindOne(ctx, bson.M{"code": couponCode, "is_active": true}).Decode(&coupon)
		if cErr == nil && subtotal >= coupon.MinOrder && coupon.UsedCount < coupon.UsageLimit && time.Now().Before(coupon.ExpiresAt) {
			if coupon.Type == "percentage" {
				discount = subtotal * coupon.Value / 100
				if rem := discount % 100; rem != 0 {
					discount += 100 - rem // whole rupees, rounded in customer's favor
				}
				if coupon.MaxDiscount > 0 && discount > coupon.MaxDiscount {
					discount = coupon.MaxDiscount
				}
			} else {
				discount = coupon.Value
				if discount > subtotal {
					discount = subtotal
				}
			}
			if _, uErr := h.db.Collection("coupons").UpdateOne(ctx, bson.M{"_id": coupon.ID}, bson.M{"$inc": bson.M{"used_count": 1}}); uErr != nil {
				return nil, items, nil, fmt.Errorf("coupon update failed: %w", uErr)
			}
			couponUsed = true
			log.Info("CREATE", "Coupon applied", "user_id", userID, "coupon", couponCode, "discount", discount)
		} else {
			log.Debug("CREATE", "Coupon not applicable", "user_id", userID, "coupon", couponCode)
		}
	}

	// Shipping: free above ₹999, else ₹49 — must match storefront src/config/constants.ts
	shippingCost := 0
	if subtotal < freeShippingThreshold {
		shippingCost = shippingCharge
	}
	total := subtotal - discount + shippingCost
	if total < 0 {
		total = 0
	}

	// Compute advance/COD split
	advanceAmount := total
	codAmount := 0
	if paymentPlan == "partial" && h.cfg != nil && h.cfg.PartialPaymentEnabled {
		effectivePercent := h.cfg.PartialPaymentDefaultPercent
		if maxAdvancePercent > effectivePercent {
			effectivePercent = maxAdvancePercent
		}
		if effectivePercent < 0 {
			effectivePercent = 0
		}
		if effectivePercent > 100 {
			effectivePercent = 100
		}
		// Whole-rupee split: advance rounds UP to the nearest rupee (never a
		// decimal like ₹89.40 in the payment modal), COD gets the remainder —
		// the two always sum exactly to the total.
		advanceAmount = (total*effectivePercent + 99) / 100 // ceil to paise
		if rem := advanceAmount % 100; rem != 0 {
			advanceAmount += 100 - rem // ceil to whole rupee
		}
		if advanceAmount > total {
			advanceAmount = total
		}
		codAmount = total - advanceAmount
	}

	order = &models.Order{
		ID: uuid.New().String(), UserID: userID, OrderNumber: genOrderNumber(),
		Items: items, Subtotal: subtotal, ShippingCost: shippingCost, Discount: discount,
		Total: total, PaymentPlan: paymentPlan, AdvanceAmount: advanceAmount, CODAmount: codAmount,
		CouponCode: couponCode, CouponUsed: couponUsed, Status: "placed", PaymentStatus: "pending",
		ShippingAddress: addr, Notes: notes, IsGuest: isGuest,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	return order, items, nil, nil
}

// placeOrderTxn runs the whole checkout inside a multi-document transaction. Any error
// returned from the callback aborts the transaction, atomically rolling back every write.
func (h *Handler) placeOrderTxn(ctx context.Context, userID string, cartItems []models.CartItem, addr models.Address, couponCode, notes, paymentPlan string, isGuest bool) (*models.Order, []string, error) {
	session, err := h.db.Client().StartSession()
	if err != nil {
		return nil, nil, fmt.Errorf("start session: %w", err)
	}
	defer session.EndSession(ctx)

	var order *models.Order
	var stockDetails []string

	// errStockAbort is a sentinel used to abort the transaction on an out-of-stock
	// condition (a client error, not a server failure) while still rolling back.
	errStockAbort := fmt.Errorf("stock unavailable")

	_, txnErr := session.WithTransaction(ctx, func(sessCtx mongo.SessionContext) (interface{}, error) {
		o, _, details, bErr := h.buildOrder(sessCtx, userID, cartItems, addr, couponCode, notes, paymentPlan, isGuest)
		if bErr != nil {
			return nil, bErr
		}
		if details != nil {
			stockDetails = details
			return nil, errStockAbort
		}
		if _, iErr := h.db.Collection("orders").InsertOne(sessCtx, o); iErr != nil {
			return nil, fmt.Errorf("order insert: %w", iErr)
		}
		// NOTE: cart is intentionally NOT cleared here. It clears on payment success
		// (markPaymentPaid) so a dismissed/failed payment returns the customer to an
		// unchanged checkout — all-or-nothing order UX.
		order = o
		return nil, nil
	})

	if txnErr == errStockAbort {
		return nil, stockDetails, nil
	}
	if txnErr != nil {
		return nil, nil, txnErr
	}
	return order, nil, nil
}

// placeOrderManual is the standalone-MongoDB fallback: it performs the same writes without
// a transaction and compensates by restoring stock if the order insert fails.
func (h *Handler) placeOrderManual(ctx context.Context, userID string, cartItems []models.CartItem, addr models.Address, couponCode, notes, paymentPlan string, isGuest bool) (*models.Order, []string, error) {
	order, decremented, stockDetails, err := h.buildOrder(ctx, userID, cartItems, addr, couponCode, notes, paymentPlan, isGuest)
	if err != nil {
		h.restoreStock(ctx, decremented) // some items may already be decremented
		return nil, nil, err
	}
	if stockDetails != nil {
		h.restoreStock(ctx, decremented)
		log.Warn("CREATE", "Stock rollback completed", "user_id", userID, "rolled_back_items", len(decremented))
		return nil, stockDetails, nil
	}

	if _, iErr := h.db.Collection("orders").InsertOne(ctx, order); iErr != nil {
		log.ErrorWithCode("CREATE", errcodes.EOrdCreateFailed.Code, "Order insert failed, rolling back stock", "user_id", userID, "err", iErr)
		h.restoreStock(ctx, decremented)
		return nil, nil, iErr
	}

	h.db.Collection("carts").UpdateOne(ctx, bson.M{"user_id": userID},
		bson.M{"$set": bson.M{"items": []models.CartItem{}, "updated_at": time.Now()}})

	return order, nil, nil
}

// restoreStock adds back the quantities for a set of order items (compensating rollback).
func (h *Handler) restoreStock(ctx context.Context, items []models.OrderItem) {
	for _, item := range items {
		h.db.Collection("products").UpdateOne(ctx, bson.M{"_id": item.ProductID}, bson.M{"$inc": bson.M{"stock": item.Quantity}})
	}
}

func (h *Handler) Get(c *gin.Context) {
	userID := c.GetString("user_id")
	id := c.Param("id")
	isAdmin, _ := c.Get("is_admin")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	filter := bson.M{"$or": []bson.M{{"_id": id}, {"order_number": id}}}
	if isAdmin == nil || !isAdmin.(bool) {
		filter["user_id"] = userID
	}

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
	// Abandoned/expired orders never became real orders — hide them from the customer.
	cursor, err := h.db.Collection("orders").Find(ctx, bson.M{
		"user_id": userID,
		"status":  bson.M{"$nin": []string{"abandoned", "expired"}},
	}, opts)
	if err != nil {
		log.ErrorWithCode("LIST", errcodes.EOrdListFailed.Code, "Failed to list orders", "user_id", userID, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed"})
		return
	}
	defer cursor.Close(ctx)
	var orders []models.Order
	cursor.All(ctx, &orders)
	if orders == nil {
		orders = []models.Order{}
	}
	c.JSON(http.StatusOK, gin.H{"orders": orders, "total": len(orders)})
}

// Abandon is called by the storefront when the payment window is dismissed or the
// payment fails. All-or-nothing UX: the order disappears (stock + coupon released,
// hidden from the user, purged by the sweeper after 24h). If the payment actually
// completed concurrently (late UPI approval), it reports paid=true instead so the
// frontend can show the success screen.
func (h *Handler) Abandon(c *gin.Context) {
	userID := c.GetString("user_id")
	orderID := c.Param("id")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var order models.Order
	if err := h.db.Collection("orders").FindOne(ctx, bson.M{"_id": orderID, "user_id": userID}).Decode(&order); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found", "code": errcodes.EOrdNotFound.Code})
		return
	}

	if order.PaymentStatus == "paid" {
		// Race: payment captured (verify/webhook) before the dismiss reached us.
		c.JSON(http.StatusOK, gin.H{"paid": true, "status": order.Status})
		return
	}

	if AbandonOrder(ctx, h.db, &order) {
		c.JSON(http.StatusOK, gin.H{"paid": false, "abandoned": true})
		return
	}

	// Transition lost a race — re-read and report the truth.
	h.db.Collection("orders").FindOne(ctx, bson.M{"_id": orderID}).Decode(&order)
	c.JSON(http.StatusOK, gin.H{"paid": order.PaymentStatus == "paid", "status": order.Status})
}

func (h *Handler) Cancel(c *gin.Context) {
	userID := c.GetString("user_id")
	orderID := c.Param("id")
	var req struct {
		Reason string `json:"reason"`
	}
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

	// Idempotent stock and coupon release
	ReleaseStock(ctx, h.db, &order)
	ReleaseCoupon(ctx, h.db, &order)

	// P1-2: Auto-refund for paid orders via Razorpay
	if order.PaymentStatus == "paid" && order.PaymentID != "" {
		h.attemptAutoRefund(ctx, &order, order.Total)
	}

	log.Info("CANCEL", "Order cancelled", "order_id", orderID, "order_number", order.OrderNumber, "user_id", userID, "reason", req.Reason, "refund_pending", order.PaymentStatus == "paid")

	// Send cancellation SMS via MSG91
	go utils.SendOrderCancelledSMS(order.ShippingAddress.Phone, order.OrderNumber)

	c.JSON(http.StatusOK, gin.H{"message": "Order cancelled", "refund_amount": order.Total})
}

// ==================== ADMIN ENDPOINTS ====================

func (h *Handler) AdminList(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	filter := bson.M{}
	if status := c.Query("status"); status != "" {
		filter["status"] = status
	}
	if ps := c.Query("payment_status"); ps != "" {
		filter["payment_status"] = ps
	}
	// Server-side search: order number (customer-facing id), internal id,
	// customer name or phone. Searches ALL orders, not just the loaded page.
	// Accepts every shape support staff will paste: "ORD-982640068", "982640068",
	// "ord-98264", "#9339f9e4", "9339f9e4", "ORD 982640068", "ORD982640068".
	if q := strings.TrimSpace(c.Query("q")); q != "" {
		q = strings.TrimSpace(strings.TrimPrefix(q, "#")) // "#9339f9e4" → "9339f9e4"
		safe := regexp.QuoteMeta(q)
		ors := []bson.M{
			{"order_number": bson.M{"$regex": safe, "$options": "i"}}, // ORD- prefix optional, case-insensitive
			{"_id": bson.M{"$regex": safe, "$options": "i"}},          // any UUID fragment
			{"shipping_address.name": bson.M{"$regex": safe, "$options": "i"}},
			{"shipping_address.phone": bson.M{"$regex": safe}},
		}
		// "ORD982640068" / "ord 982640068" — no dash, so the raw regex misses the
		// stored "ORD-982640068". Fall back to matching just the digits.
		if digits := nonDigits.ReplaceAllString(q, ""); digits != "" && digits != q {
			ors = append(ors, bson.M{"order_number": bson.M{"$regex": regexp.QuoteMeta(digits), "$options": "i"}})
		}
		filter["$or"] = ors
	}

	page, limit := 1, 50
	if p := c.Query("page"); p != "" {
		fmt.Sscanf(p, "%d", &page)
	}
	if l := c.Query("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
	}
	if page < 1 {
		page = 1
	}
	skip := int64((page - 1) * limit)

	total, _ := h.db.Collection("orders").CountDocuments(ctx, filter)
	opts := options.Find().SetSort(bson.M{"created_at": -1}).SetSkip(skip).SetLimit(int64(limit))
	cursor, _ := h.db.Collection("orders").Find(ctx, filter, opts)
	defer cursor.Close(ctx)
	var orders []models.Order
	cursor.All(ctx, &orders)
	if orders == nil {
		orders = []models.Order{}
	}

	totalPages := int(total) / limit
	if int(total)%limit > 0 {
		totalPages++
	}

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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Fetch current order to validate transition
	var order models.Order
	if err := h.db.Collection("orders").FindOne(ctx, bson.M{"_id": id}).Decode(&order); err != nil {
		log.Warn("UPDATE_STATUS", "Order not found", "order_id", id)
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found"})
		return
	}

	// P0-4: Validate status transition
	allowed, nextStates := ValidateTransition(order.Status, req.Status)
	if !allowed {
		log.WarnWithCode("UPDATE_STATUS", errcodes.EOrdIllegalTransit.Code,
			"Illegal status transition", "order_id", id, "from", order.Status, "to", req.Status)
		c.JSON(http.StatusBadRequest, gin.H{
			"error":          fmt.Sprintf("Cannot transition from '%s' to '%s'", order.Status, req.Status),
			"code":           errcodes.EOrdIllegalTransit.Code,
			"allowed_states": nextStates,
		})
		return
	}

	now := time.Now()
	setFields := bson.M{"status": req.Status, "updated_at": now}
	if req.TrackingID != "" {
		setFields["tracking_id"] = req.TrackingID
	}

	// P1-1: Set delivered_at when transitioning to delivered
	if req.Status == "delivered" {
		setFields["delivered_at"] = now
	}

	// For cancellation via admin, set cancel fields and handle refund
	if req.Status == "cancelled" {
		setFields["cancelled_at"] = now
		if order.PaymentStatus == "paid" {
			setFields["payment_status"] = "refund_pending"
			setFields["refund_amount"] = order.Total
		}
	}

	result, err := h.db.Collection("orders").UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": setFields})
	if err != nil || result.MatchedCount == 0 {
		log.Warn("UPDATE_STATUS", "Order update failed", "order_id", id)
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found"})
		return
	}

	// Re-fetch order after update for notifications
	h.db.Collection("orders").FindOne(ctx, bson.M{"_id": id}).Decode(&order)

	if req.Status == "cancelled" || req.Status == "returned" {
		// Idempotent stock and coupon release
		ReleaseStock(ctx, h.db, &order)
		ReleaseCoupon(ctx, h.db, &order)
		log.Info("UPDATE_STATUS", "Stock/coupon restored for cancelled/returned order", "order_id", id, "items", len(order.Items))

		// P1-2: Auto-refund for paid cancelled orders
		if req.Status == "cancelled" && order.PaymentStatus == "refund_pending" && order.PaymentID != "" {
			h.attemptAutoRefund(ctx, &order, order.Total)
		}
	}

	// Send status-specific SMS via MSG91
	if order.ShippingAddress.Phone != "" {
		switch req.Status {
		case "confirmed":
			go utils.SendOrderSMS(order.ShippingAddress.Phone,
				fmt.Sprintf("Your order #%s has been confirmed and is being prepared!", order.OrderNumber))
		case "shipped":
			trackingInfo := req.TrackingID
			if trackingInfo == "" {
				trackingInfo = "N/A"
			}
			go utils.SendOrderShippedSMS(order.ShippingAddress.Phone, order.OrderNumber, trackingInfo, "")
		case "delivered":
			go utils.SendOrderDeliveredSMS(order.ShippingAddress.Phone, order.OrderNumber)
		case "cancelled":
			go utils.SendOrderCancelledSMS(order.ShippingAddress.Phone, order.OrderNumber)
		}
	}

	log.Info("UPDATE_STATUS", "Order status updated", "order_id", id, "from", order.Status, "to", req.Status)
	c.JSON(http.StatusOK, gin.H{"message": "Status updated to " + req.Status})
}

func (h *Handler) ProcessRefund(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Amount int    `json:"amount"`
		Notes  string `json:"notes"`
	}
	c.ShouldBindJSON(&req)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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

	// Max refundable = what was actually paid online. Partial-plan orders only
	// charged the advance; the COD remainder was never collected by the gateway.
	maxRefundable := order.Total
	if order.PaymentPlan == "partial" && order.AdvanceAmount > 0 {
		maxRefundable = order.AdvanceAmount
	}
	// Subtract any previously refunded amount
	remainingRefundable := maxRefundable - order.RefundAmount
	if remainingRefundable <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Order has already been fully refunded", "code": errcodes.EOrdRefundAmount.Code})
		return
	}

	refundAmount := remainingRefundable
	if req.Amount > 0 {
		if req.Amount > remainingRefundable {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":               fmt.Sprintf("Refund amount %d exceeds remaining refundable %d", req.Amount, remainingRefundable),
				"code":                errcodes.EOrdRefundAmount.Code,
				"max_refundable":      maxRefundable,
				"already_refunded":    order.RefundAmount,
				"remaining_refundable": remainingRefundable,
			})
			return
		}
		refundAmount = req.Amount
	}
	now := time.Now()
	refundID := "REFUND-" + uuid.New().String()[:8]

	// Attempt gateway-level refund for Stripe payments (pi_ prefix on payment_id)
	if h.cfg.StripeSecretKey != "" && order.PaymentID != "" && len(order.PaymentID) > 3 && order.PaymentID[:3] == "pi_" {
		stripeRefundID, err := h.processStripeRefund(order.PaymentID, refundAmount)
		if err != nil {
			log.Error("REFUND", "Stripe refund API failed", "order_id", id, "err", err.Error())
			c.JSON(http.StatusBadGateway, gin.H{"error": "Stripe refund failed: " + err.Error(), "code": errcodes.EOrdRefundFailed.Code})
			return
		}
		refundID = stripeRefundID
	}

	h.db.Collection("orders").UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{
		"payment_status": "refunded", "refund_id": refundID,
		"refund_amount": refundAmount, "refunded_at": now, "updated_at": now,
	}})

	log.Info("REFUND", "Refund processed", "refund_id", refundID, "order_id", id, "order_number", order.OrderNumber, "amount", refundAmount)

	go utils.SendSMS(order.ShippingAddress.Phone,
		fmt.Sprintf("Refund of Rs.%d processed for order %s. RefID: %s", refundAmount/100, order.OrderNumber, refundID), "HIGH")

	if h.notifier != nil {
		var user models.User
		if err := h.db.Collection("users").FindOne(ctx, bson.M{"_id": order.UserID}).Decode(&user); err == nil {
			h.notifier.RefundProcessed(&order, &user, refundAmount)
		}
	}

	c.JSON(http.StatusOK, gin.H{"message": "Refund processed", "refund_id": refundID, "refund_amount": refundAmount, "order_number": order.OrderNumber})
}

// attemptAutoRefund tries to refund a paid order via the active gateway.
// If the refund API call fails, the order stays cancelled with refund_pending status.
func (h *Handler) attemptAutoRefund(ctx context.Context, order *models.Order, amount int) {
	// Max refundable for partial plan
	if order.PaymentPlan == "partial" && order.AdvanceAmount > 0 {
		if amount > order.AdvanceAmount {
			amount = order.AdvanceAmount
		}
	}

	var refundID string
	var refundErr error

	// Try Razorpay refund
	if h.cfg.RazorpayKeyID != "" && order.PaymentID != "" {
		refundID, refundErr = h.processRazorpayRefund(order.PaymentID, amount)
	} else if h.cfg.StripeSecretKey != "" && order.PaymentID != "" && len(order.PaymentID) > 3 && order.PaymentID[:3] == "pi_" {
		refundID, refundErr = h.processStripeRefund(order.PaymentID, amount)
	}

	now := time.Now()
	if refundErr != nil {
		log.Error("AUTO_REFUND", "Gateway refund failed — order stays refund_pending", "order_id", order.ID, "err", refundErr)
		return
	}

	if refundID != "" {
		h.db.Collection("orders").UpdateOne(ctx, bson.M{"_id": order.ID}, bson.M{"$set": bson.M{
			"payment_status": "refunded",
			"refund_id":      refundID,
			"refund_amount":  amount,
			"refunded_at":    now,
			"updated_at":     now,
		}})
		log.Info("AUTO_REFUND", "Refund processed", "order_id", order.ID, "refund_id", refundID, "amount", amount)
	}
}

// processRazorpayRefund calls Razorpay's POST /v1/payments/:id/refund endpoint.
func (h *Handler) processRazorpayRefund(razorpayPaymentID string, amountPaise int) (string, error) {
	payload := fmt.Sprintf(`{"amount":%d}`, amountPaise)
	url := fmt.Sprintf("https://api.razorpay.com/v1/payments/%s/refund", razorpayPaymentID)

	req, err := http.NewRequest("POST", url, strings.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(h.cfg.RazorpayKeyID, h.cfg.RazorpaySecret)
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("razorpay API unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("razorpay refund status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		ID string `json:"id"`
	}
	json.Unmarshal(body, &result)
	if result.ID == "" {
		return "", fmt.Errorf("no refund id in razorpay response")
	}
	return result.ID, nil
}

// processStripeRefund calls Stripe's POST /v1/refunds endpoint (legacy path).
func (h *Handler) processStripeRefund(paymentIntentID string, amountPaise int) (string, error) {
	form := "payment_intent=" + paymentIntentID + "&amount=" + fmt.Sprintf("%d", amountPaise)
	req, err := http.NewRequest("POST", "https://api.stripe.com/v1/refunds", strings.NewReader(form))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+h.cfg.StripeSecretKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("stripe API unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("stripe refund status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		ID string `json:"id"`
	}
	json.Unmarshal(body, &result)
	if result.ID == "" {
		return "", fmt.Errorf("no refund id in stripe response")
	}
	return result.ID, nil
}
