package order

import (
	"context"
	"encoding/json"
	"errors"
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
var checkoutPhone = regexp.MustCompile(`^\d{10}$`)
var checkoutPincode = regexp.MustCompile(`^[1-9]\d{5}$`)

// Shipping thresholds in paise — single source of truth server-side.
// Must match storefront src/config/constants.ts (₹999 free-shipping, ₹49 charge).
const (
	freeShippingThreshold = 99900
	shippingCharge        = 4900
)

var (
	ErrDeliveryUnavailable = errors.New("delivery is not available for this pincode")
	ErrDeliveryCheckFailed = errors.New("delivery availability could not be confirmed")
	ErrPriceChanged        = errors.New("a product price changed")
	ErrCouponInvalid       = errors.New("coupon is no longer available")
)

type serviceabilityChecker interface {
	IsServiceable(ctx context.Context, order *models.Order) (bool, error)
}

type Handler struct {
	db            *mongo.Database
	cfg           *config.Config
	useTxn        bool // true when the Mongo deployment supports multi-document transactions (replica set)
	notifier      *notify.Notifier
	shipping      serviceabilityChecker
	refundPayment func(paymentID string, amount int) (string, error) // test hook; nil uses configured provider
}

func NewHandler(db *mongo.Database, cfg *config.Config) *Handler {
	return &Handler{db: db, cfg: cfg, useTxn: supportsTransactions(db)}
}

func (h *Handler) SupportsTransactions() bool { return h.useTxn }

// WithNotifier injects the notification service (optional — nil means no notifications).
func (h *Handler) WithNotifier(n *notify.Notifier) *Handler { h.notifier = n; return h }

// WithServiceabilityChecker makes the courier provider authoritative at checkout.
func (h *Handler) WithServiceabilityChecker(checker serviceabilityChecker) *Handler {
	h.shipping = checker
	return h
}

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

	if !checkoutPhone.MatchString(req.ShippingAddress.Phone) || !checkoutPincode.MatchString(req.ShippingAddress.Pincode) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Phone must be 10 digits and pincode must be a valid 6-digit Indian pincode", "code": errcodes.EOrdAddressInvalid.Code})
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

	idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if idempotencyKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Idempotency-Key header is required"})
		return
	}
	order, stockErr, err := h.placeOrder(ctx, userID, cart.Items, req.ShippingAddress, req.CouponCode, req.Notes, paymentPlan, false, idempotencyKey)
	if stockErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Stock check failed", "code": errcodes.EOrdStockFailed.Code, "details": stockErr})
		return
	}
	if err != nil {
		switch {
		case errors.Is(err, ErrDeliveryUnavailable):
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error(), "code": errcodes.EOrdAddressInvalid.Code})
		case errors.Is(err, ErrDeliveryCheckFailed):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Delivery availability could not be confirmed. Please try again.", "code": errcodes.EOrdCreateFailed.Code})
		case errors.Is(err, ErrPriceChanged):
			c.JSON(http.StatusConflict, gin.H{"error": "A product price changed. Refresh your cart and review the new total before paying.", "code": errcodes.EOrdCreateFailed.Code})
		case errors.Is(err, ErrCouponInvalid):
			c.JSON(http.StatusConflict, gin.H{"error": "The coupon is no longer available. Review the total and try again.", "code": errcodes.EOrdCreateFailed.Code})
		default:
			log.ErrorWithCode("CREATE", errcodes.EOrdCreateFailed.Code, "Order creation failed", "user_id", userID, "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create order", "code": errcodes.EOrdCreateFailed.Code})
		}
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
func (h *Handler) PlaceOrder(ctx context.Context, userID string, cartItems []models.CartItem, addr models.Address, couponCode, notes, paymentPlan string, isGuest bool, idempotencyKey string) (*models.Order, []string, error) {
	return h.placeOrder(ctx, userID, cartItems, addr, couponCode, notes, paymentPlan, isGuest, idempotencyKey)
}

// placeOrder runs the checkout writes (stock decrement, coupon, order insert, cart clear)
// atomically. On a replica set it uses a real multi-document transaction — any failure
// aborts and rolls back everything. On standalone MongoDB it falls back to the original
// best-effort manual compensating rollback. Returns (order, stockDetails, err): stockDetails
// is non-nil only for an out-of-stock condition (client 400), err for any other failure.
func (h *Handler) placeOrder(ctx context.Context, userID string, cartItems []models.CartItem, addr models.Address, couponCode, notes, paymentPlan string, isGuest bool, idempotencyKey string) (*models.Order, []string, error) {
	var existing models.Order
	if idempotencyKey != "" && h.db.Collection("orders").FindOne(ctx, bson.M{"idempotency_key": idempotencyKey}).Decode(&existing) == nil {
		return &existing, nil, nil
	}
	var created *models.Order
	var stock []string
	var err error
	if h.useTxn {
		created, stock, err = h.placeOrderTxn(ctx, userID, cartItems, addr, couponCode, notes, paymentPlan, isGuest, idempotencyKey)
	} else {
		created, stock, err = h.placeOrderManual(ctx, userID, cartItems, addr, couponCode, notes, paymentPlan, isGuest, idempotencyKey)
	}
	if err != nil && idempotencyKey != "" && h.db.Collection("orders").FindOne(ctx, bson.M{"idempotency_key": idempotencyKey}).Decode(&existing) == nil {
		return &existing, nil, nil
	}
	return created, stock, err
}

// buildOrder performs the stock decrements and assembles the order. It returns the order,
// the products it decremented (for compensating rollback in the non-txn path), an
// out-of-stock detail slice, and a hard error. It does NOT insert the order or clear the
// cart — the caller sequences those so the txn/manual paths can share this logic.
//
// cartItems is the list of items to place — from the server cart (authed) or from the
// request body (guest). The isGuest flag marks the order so downstream code (e.g.
// markPaymentPaid) knows not to look for a user record or server cart.
func (h *Handler) buildOrder(ctx context.Context, userID string, cartItems []models.CartItem, addr models.Address, couponCode, notes, paymentPlan string, isGuest bool, idempotencyKey string) (order *models.Order, decremented []models.OrderItem, stockDetails []string, err error) {
	items := make([]models.OrderItem, 0, len(cartItems))
	subtotal := 0
	maxAdvancePercent := 0

	for _, ci := range cartItems {
		filter := bson.M{"_id": ci.ProductID, "is_active": true, "stock": bson.M{"$gte": ci.Quantity}}
		increments := bson.M{"stock": -ci.Quantity}
		if ci.VariantID != "" {
			filter["variants"] = bson.M{"$elemMatch": bson.M{"id": ci.VariantID, "stock": bson.M{"$gte": ci.Quantity}}}
			increments["variants.$.stock"] = -ci.Quantity
		} else {
			filter["$or"] = []bson.M{{"variants": bson.M{"$exists": false}}, {"variants": bson.M{"$size": 0}}}
		}

		var product models.Product
		decErr := h.db.Collection("products").FindOneAndUpdate(ctx,
			filter,
			bson.M{"$inc": increments},
			options.FindOneAndUpdate().SetReturnDocument(options.Before),
		).Decode(&product)

		if decErr != nil {
			log.WarnWithCode("CREATE", errcodes.EOrdStockFailed.Code, "Stock or variant check failed for item", "user_id", userID, "product", ci.Name, "product_id", ci.ProductID, "variant_id", ci.VariantID, "requested_qty", ci.Quantity)
			return nil, items, []string{fmt.Sprintf("%s: insufficient stock, invalid variant, or unavailable product", ci.Name)}, nil
		}

		unitPrice := product.Price
		variantLabel := ""
		if ci.VariantID != "" {
			variantFound := false
			for _, variant := range product.Variants {
				if variant.ID == ci.VariantID {
					variantFound = true
					variantLabel = variant.Name + ": " + variant.Value
					if variant.Price > 0 {
						unitPrice = variant.Price
					}
					break
				}
			}
			if !variantFound {
				return nil, items, []string{fmt.Sprintf("%s: selected variant is unavailable", product.Name)}, nil
			}
		}

		// Track max advance_percent across items
		if product.AdvancePercent != nil && *product.AdvancePercent > maxAdvancePercent {
			maxAdvancePercent = *product.AdvancePercent
		}

		items = append(items, models.OrderItem{
			ProductID: product.ID, VariantID: ci.VariantID, VariantLabel: variantLabel,
			Name: product.Name, SKU: product.SKU, Price: unitPrice,
			Quantity: ci.Quantity, Image: product.Thumbnail, Weight: product.Weight,
			LengthCM: product.LengthCM, WidthCM: product.WidthCM, HeightCM: product.HeightCM,
		})
		if ci.Price > 0 && ci.Price != unitPrice {
			return nil, items, nil, fmt.Errorf("%w: %s", ErrPriceChanged, product.Name)
		}
		subtotal += unitPrice * ci.Quantity
	}

	discount := 0
	couponUsed := false
	couponCode = strings.ToUpper(strings.TrimSpace(couponCode))
	if couponCode != "" {
		var coupon models.Coupon
		couponErr := h.db.Collection("coupons").FindOneAndUpdate(ctx,
			bson.M{
				"code": couponCode, "is_active": true,
				"min_order":  bson.M{"$lte": subtotal},
				"expires_at": bson.M{"$gt": time.Now()},
				"$expr":      bson.M{"$lt": []interface{}{"$used_count", "$usage_limit"}},
			},
			bson.M{"$inc": bson.M{"used_count": 1}},
			options.FindOneAndUpdate().SetReturnDocument(options.Before),
		).Decode(&coupon)
		if couponErr == nil {
			if coupon.Type == "percentage" {
				discount = subtotal * coupon.Value / 100
				if rem := discount % 100; rem != 0 {
					discount += 100 - rem
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
			couponUsed = true
			log.Info("CREATE", "Coupon applied", "user_id", userID, "coupon", couponCode, "discount", discount)
		} else if couponErr != mongo.ErrNoDocuments {
			return nil, items, nil, fmt.Errorf("coupon update failed: %w", couponErr)
		} else {
			return nil, items, nil, ErrCouponInvalid
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
		ID: uuid.New().String(), UserID: userID, OrderNumber: genOrderNumber(), IdempotencyKey: idempotencyKey,
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
func (h *Handler) placeOrderTxn(ctx context.Context, userID string, cartItems []models.CartItem, addr models.Address, couponCode, notes, paymentPlan string, isGuest bool, idempotencyKey string) (*models.Order, []string, error) {
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
		o, _, details, bErr := h.buildOrder(sessCtx, userID, cartItems, addr, couponCode, notes, paymentPlan, isGuest, idempotencyKey)
		if bErr != nil {
			return nil, bErr
		}
		if details != nil {
			stockDetails = details
			return nil, errStockAbort
		}
		if h.shipping != nil {
			serviceable, serviceErr := h.shipping.IsServiceable(sessCtx, o)
			if serviceErr != nil {
				return nil, fmt.Errorf("%w: %v", ErrDeliveryCheckFailed, serviceErr)
			}
			if !serviceable {
				return nil, ErrDeliveryUnavailable
			}
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
func (h *Handler) placeOrderManual(ctx context.Context, userID string, cartItems []models.CartItem, addr models.Address, couponCode, notes, paymentPlan string, isGuest bool, idempotencyKey string) (*models.Order, []string, error) {
	order, decremented, stockDetails, err := h.buildOrder(ctx, userID, cartItems, addr, couponCode, notes, paymentPlan, isGuest, idempotencyKey)
	if err != nil {
		h.restoreStock(ctx, decremented) // some items may already be decremented
		return nil, nil, err
	}
	if stockDetails != nil {
		h.restoreStock(ctx, decremented)
		log.Warn("CREATE", "Stock rollback completed", "user_id", userID, "rolled_back_items", len(decremented))
		return nil, stockDetails, nil
	}

	if h.shipping != nil {
		serviceable, serviceErr := h.shipping.IsServiceable(ctx, order)
		if serviceErr != nil || !serviceable {
			h.restoreStock(ctx, decremented)
			if order.CouponUsed {
				h.db.Collection("coupons").UpdateOne(ctx,
					bson.M{"code": order.CouponCode, "used_count": bson.M{"$gt": 0}},
					bson.M{"$inc": bson.M{"used_count": -1}},
				)
			}
			if serviceErr != nil {
				return nil, nil, fmt.Errorf("%w: %v", ErrDeliveryCheckFailed, serviceErr)
			}
			return nil, nil, ErrDeliveryUnavailable
		}
	}

	if _, iErr := h.db.Collection("orders").InsertOne(ctx, order); iErr != nil {
		log.ErrorWithCode("CREATE", errcodes.EOrdCreateFailed.Code, "Order insert failed, rolling back stock and coupon", "user_id", userID, "err", iErr)
		h.restoreStock(ctx, decremented)
		if order.CouponUsed {
			h.db.Collection("coupons").UpdateOne(ctx,
				bson.M{"code": order.CouponCode, "used_count": bson.M{"$gt": 0}},
				bson.M{"$inc": bson.M{"used_count": -1}},
			)
		}
		return nil, nil, iErr
	}

	// Keep the cart until payment succeeds. This matches the transaction path and
	// preserves a customer's cart when the payment window is dismissed.
	return order, nil, nil
}

// restoreStock adds back the quantities for a set of order items (compensating rollback).
func stockIncrement(item models.OrderItem, quantity int) bson.M {
	increments := bson.M{"stock": quantity}
	if item.VariantID != "" {
		increments["variants.$[variant].stock"] = quantity
	}
	return increments
}

func stockArrayFilters(item models.OrderItem) *options.UpdateOptions {
	if item.VariantID == "" {
		return options.Update()
	}
	return options.Update().SetArrayFilters(options.ArrayFilters{Filters: []interface{}{bson.M{"variant.id": item.VariantID}}})
}

func (h *Handler) restoreStock(ctx context.Context, items []models.OrderItem) {
	for _, item := range items {
		h.db.Collection("products").UpdateOne(ctx,
			bson.M{"_id": item.ProductID},
			bson.M{"$inc": stockIncrement(item, item.Quantity)},
			stockArrayFilters(item),
		)
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
		"user_id":     userID,
		"status":      bson.M{"$nin": []string{"abandoned", "expired"}},
		"archived_at": bson.M{"$exists": false},
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
// hidden from the user). If the payment actually completed concurrently (late UPI
// approval), it reports paid=true instead so the frontend can show the success screen.
// The sweeper archives the record after 24 hours so late provider captures and
// inventory movements remain auditable.
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

func providerCancellationResolved(order *models.Order) bool {
	if strings.TrimSpace(order.ShipmozoOrderID) == "" {
		return true
	}
	status := strings.ToLower(strings.TrimSpace(order.ShipmozoPushStatus))
	switch status {
	case "cancelled", "canceled", "cancelled_at_provider", "cancellation_confirmed":
		return true
	default:
		return false
	}
}

func (h *Handler) recordShipmentCancellationBlock(ctx context.Context, order *models.Order) {
	now := time.Now()
	_, _ = h.db.Collection("orders").UpdateOne(ctx, bson.M{"_id": order.ID}, bson.M{"$set": bson.M{
		"shipment_cancellation_blocked":    true,
		"shipment_cancellation_blocked_at": now,
		"updated_at":                       now,
	}})
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

	if !providerCancellationResolved(&order) {
		h.recordShipmentCancellationBlock(ctx, &order)
		log.WarnWithCode("CANCEL", errcodes.EOrdCancelFailed.Code, "Local cancellation blocked until Shipmozo cancellation is confirmed", "order_id", orderID, "shipmozo_order_id", order.ShipmozoOrderID)
		c.JSON(http.StatusConflict, gin.H{
			"error":             "This order already exists in Shipmozo. Cancel it in Shipmozo first, wait for provider cancellation confirmation, then retry.",
			"code":              errcodes.EOrdCancelFailed.Code,
			"shipmozo_order_id": order.ShipmozoOrderID,
		})
		return
	}
	if order.Status != "placed" && order.Status != "confirmed" && order.Status != "processing" {
		log.WarnWithCode("CANCEL", errcodes.EOrdCancelFailed.Code, "Cannot cancel order with current status", "order_id", orderID, "status", order.Status, "user_id", userID)
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Cannot cancel order with status '%s'", order.Status), "code": errcodes.EOrdCancelFailed.Code})
		return
	}

	now := time.Now()
	refundableOnline := order.Total
	if order.PaymentPlan == "partial" && order.AdvanceAmount > 0 {
		refundableOnline = order.AdvanceAmount
	}
	update := bson.M{"status": "cancelled", "cancel_reason": req.Reason, "cancelled_at": now, "updated_at": now, "shipment_cancellation_blocked": false}
	if order.PaymentStatus == "paid" {
		update["refund_requested_amount"] = refundableOnline
		update["payment_status"] = "refund_pending"
	}
	cancelResult, cancelErr := h.db.Collection("orders").UpdateOne(ctx,
		bson.M{"_id": orderID, "user_id": userID, "status": order.Status, "payment_status": order.PaymentStatus},
		bson.M{"$set": update},
	)
	if cancelErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Order cancellation failed", "code": errcodes.EOrdCancelFailed.Code})
		return
	}
	if cancelResult.MatchedCount == 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "Order changed while cancellation was being processed. Refresh and try again.", "code": errcodes.EOrdCancelFailed.Code})
		return
	}

	// Idempotent stock and coupon release
	ReleaseStock(ctx, h.db, &order)
	ReleaseCoupon(ctx, h.db, &order)

	// Auto-refund only what the gateway collected. COD that was never collected
	// is not part of a pre-shipment cancellation refund.
	if order.PaymentStatus == "paid" && order.PaymentID != "" {
		h.attemptAutoRefund(ctx, &order, refundableOnline)
	}

	log.Info("CANCEL", "Order cancelled", "order_id", orderID, "order_number", order.OrderNumber, "user_id", userID, "reason", req.Reason, "refund_pending", order.PaymentStatus == "paid")

	// Send cancellation SMS via MSG91
	go utils.SendOrderCancelledSMS(order.ShippingAddress.Phone, order.OrderNumber)

	c.JSON(http.StatusOK, gin.H{"message": "Order cancelled", "refund_amount": refundableOnline})
}

// ==================== ADMIN ENDPOINTS ====================

func (h *Handler) AdminList(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	filter := bson.M{"archived_at": bson.M{"$exists": false}}
	if status := c.Query("status"); status != "" {
		filter["status"] = status
	}
	if ps := c.Query("payment_status"); ps != "" {
		filter["payment_status"] = ps
	}
	createdRange := bson.M{}
	if from := c.Query("from"); from != "" {
		if parsed, err := time.Parse("2006-01-02", from); err == nil {
			createdRange["$gte"] = parsed
		}
	}
	if to := c.Query("to"); to != "" {
		if parsed, err := time.Parse("2006-01-02", to); err == nil {
			createdRange["$lt"] = parsed.AddDate(0, 0, 1)
		}
	}
	if len(createdRange) > 0 {
		filter["created_at"] = createdRange
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
	if req.Status == "cancelled" && !providerCancellationResolved(&order) {
		h.recordShipmentCancellationBlock(ctx, &order)
		c.JSON(http.StatusConflict, gin.H{
			"error":             "Cancel this order in Shipmozo first. Retry after provider cancellation is confirmed.",
			"code":              errcodes.EOrdCancelFailed.Code,
			"shipmozo_order_id": order.ShipmozoOrderID,
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
	refundableOnline := order.Total
	if order.PaymentPlan == "partial" && order.AdvanceAmount > 0 {
		refundableOnline = order.AdvanceAmount
	}
	if req.Status == "cancelled" {
		setFields["cancelled_at"] = now
		setFields["shipment_cancellation_blocked"] = false
		if order.PaymentStatus == "paid" {
			setFields["payment_status"] = "refund_pending"
			setFields["refund_requested_amount"] = refundableOnline
		}
	}

	result, err := h.db.Collection("orders").UpdateOne(ctx,
		bson.M{"_id": id, "status": order.Status, "payment_status": order.PaymentStatus},
		bson.M{"$set": setFields},
	)
	if err != nil {
		log.Error("UPDATE_STATUS", "Order update failed", "order_id", id, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Order status update failed"})
		return
	}
	if result.MatchedCount == 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "Order changed while the status update was being processed. Refresh and try again."})
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
			h.attemptAutoRefund(ctx, &order, refundableOnline)
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
				"error":                fmt.Sprintf("Refund amount %d exceeds remaining refundable %d", req.Amount, remainingRefundable),
				"code":                 errcodes.EOrdRefundAmount.Code,
				"max_refundable":       maxRefundable,
				"already_refunded":     order.RefundAmount,
				"remaining_refundable": remainingRefundable,
			})
			return
		}
		refundAmount = req.Amount
	}
	if order.PaymentID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No gateway payment is recorded for this order", "code": errcodes.EOrdRefundFailed.Code})
		return
	}

	claim, claimErr := h.db.Collection("orders").UpdateOne(ctx,
		bson.M{"_id": id, "refund_processing": bson.M{"$ne": true}, "refund_reconciliation_required": bson.M{"$ne": true}},
		bson.M{"$set": bson.M{"refund_processing": true, "refund_processing_started_at": time.Now()}},
	)
	if claimErr != nil || claim.MatchedCount == 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "A refund is already in progress or requires reconciliation", "code": errcodes.EOrdRefundFailed.Code})
		return
	}
	providerConfirmed := false
	defer func() {
		if !providerConfirmed {
			h.db.Collection("orders").UpdateOne(context.Background(), bson.M{"_id": id, "refund_processing": true}, bson.M{"$unset": bson.M{"refund_processing": "", "refund_processing_started_at": ""}})
		}
	}()

	var refundID string
	var refundErr error
	switch {
	case strings.HasPrefix(order.PaymentID, "pay_"):
		if h.cfg.RazorpayKeyID == "" || h.cfg.RazorpaySecret == "" {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Razorpay refund credentials are not configured", "code": errcodes.EOrdRefundFailed.Code})
			return
		}
		refundID, refundErr = h.processRazorpayRefund(order.PaymentID, refundAmount)
	case strings.HasPrefix(order.PaymentID, "pi_"):
		if h.cfg.StripeSecretKey == "" {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Stripe refund credentials are not configured", "code": errcodes.EOrdRefundFailed.Code})
			return
		}
		refundID, refundErr = h.processStripeRefund(order.PaymentID, refundAmount)
	default:
		c.JSON(http.StatusNotImplemented, gin.H{"error": "Refunds are not supported for this payment provider", "code": errcodes.EOrdRefundFailed.Code})
		return
	}
	if refundErr != nil {
		// A timeout can mean the provider accepted the refund but the response was
		// lost. Keep the claim locked until provider reconciliation.
		providerConfirmed = true
		h.db.Collection("orders").UpdateOne(context.Background(), bson.M{"_id": id}, bson.M{"$set": bson.M{"refund_reconciliation_required": true, "refund_reconciliation_error": refundErr.Error(), "updated_at": time.Now()}})
		log.Error("REFUND", "Gateway refund failed or timed out; reconciliation required", "order_id", id, "err", refundErr.Error())
		c.JSON(http.StatusBadGateway, gin.H{"error": "Payment provider did not confirm the refund", "code": errcodes.EOrdRefundFailed.Code})
		return
	}

	providerConfirmed = true
	now := time.Now()
	newRefundTotal := order.RefundAmount + refundAmount
	paymentStatus := "paid"
	if newRefundTotal >= maxRefundable {
		paymentStatus = "refunded"
	}
	refundFilter := bson.M{"_id": id, "refund_amount": order.RefundAmount, "refund_processing": true}
	if order.RefundAmount == 0 {
		refundFilter = bson.M{"_id": id, "refund_processing": true, "$or": []bson.M{{"refund_amount": 0}, {"refund_amount": bson.M{"$exists": false}}}}
	}
	result, updateErr := h.db.Collection("orders").UpdateOne(ctx,
		refundFilter,
		bson.M{
			"$set": bson.M{
				"payment_status": paymentStatus, "refund_id": refundID,
				"refund_amount": newRefundTotal, "refund_requested_amount": 0, "refunded_at": now, "updated_at": now,
			},
			"$push":  bson.M{"refunds": bson.M{"refund_id": refundID, "amount": refundAmount, "processed_at": now}},
			"$unset": bson.M{"refund_processing": "", "refund_processing_started_at": ""},
		},
	)
	if updateErr != nil || result.MatchedCount == 0 {
		// The provider already accepted the refund. Keep the claim locked and mark
		// it for reconciliation so another click cannot refund the same amount.
		h.db.Collection("orders").UpdateOne(context.Background(), bson.M{"_id": id}, bson.M{"$set": bson.M{"refund_reconciliation_required": true, "refund_reconciliation_id": refundID, "updated_at": time.Now()}})
		log.Error("REFUND", "Gateway refund succeeded but order reconciliation failed", "order_id", id, "refund_id", refundID, "err", updateErr)
		c.Set("audit_provider_reference", refundID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Refund succeeded at the provider but order reconciliation requires attention", "refund_id": refundID, "code": errcodes.EOrdRefundFailed.Code})
		return
	}

	log.Info("REFUND", "Refund processed", "refund_id", refundID, "order_id", id, "order_number", order.OrderNumber, "amount", refundAmount)

	go utils.SendSMS(order.ShippingAddress.Phone,
		fmt.Sprintf("Refund of Rs.%d processed for order %s. RefID: %s", refundAmount/100, order.OrderNumber, refundID), "HIGH")

	if h.notifier != nil {
		var user models.User
		if err := h.db.Collection("users").FindOne(ctx, bson.M{"_id": order.UserID}).Decode(&user); err == nil {
			h.notifier.RefundProcessed(&order, &user, refundAmount)
		}
	}

	c.Set("audit_provider_reference", refundID)
	c.Set("audit_reason", req.Notes)
	c.JSON(http.StatusOK, gin.H{"message": "Refund processed", "refund_id": refundID, "refund_amount": refundAmount, "order_number": order.OrderNumber})
}

// attemptAutoRefund claims the refund before contacting the provider. Any
// provider error is ambiguous: the provider may have accepted the request and
// lost the response. In that case the claim stays locked until reconciliation.
func (h *Handler) attemptAutoRefund(ctx context.Context, order *models.Order, amount int) {
	if order.PaymentPlan == "partial" && order.AdvanceAmount > 0 && amount > order.AdvanceAmount {
		amount = order.AdvanceAmount
	}

	claim, claimErr := h.db.Collection("orders").UpdateOne(ctx, bson.M{
		"_id":                            order.ID,
		"payment_status":                 "refund_pending",
		"refund_processing":              bson.M{"$ne": true},
		"refund_reconciliation_required": bson.M{"$ne": true},
	}, bson.M{"$set": bson.M{"refund_processing": true, "refund_processing_started_at": time.Now()}})
	if claimErr != nil || claim.MatchedCount == 0 {
		log.Warn("AUTO_REFUND", "Refund already in progress or requires reconciliation", "order_id", order.ID, "err", claimErr)
		return
	}

	var refundID string
	var refundErr error
	if h.refundPayment != nil {
		refundID, refundErr = h.refundPayment(order.PaymentID, amount)
	} else {
		switch {
		case strings.HasPrefix(order.PaymentID, "pay_") && h.cfg.RazorpayKeyID != "" && h.cfg.RazorpaySecret != "":
			refundID, refundErr = h.processRazorpayRefund(order.PaymentID, amount)
		case strings.HasPrefix(order.PaymentID, "pi_") && h.cfg.StripeSecretKey != "":
			refundID, refundErr = h.processStripeRefund(order.PaymentID, amount)
		default:
			refundErr = fmt.Errorf("refund provider unavailable for payment %s", order.PaymentID)
		}
	}

	now := time.Now()
	if refundErr != nil || refundID == "" {
		if refundErr == nil {
			refundErr = fmt.Errorf("provider returned no refund id")
		}
		_, _ = h.db.Collection("orders").UpdateOne(context.Background(), bson.M{"_id": order.ID, "refund_processing": true}, bson.M{"$set": bson.M{
			"refund_reconciliation_required": true,
			"refund_reconciliation_error":    refundErr.Error(),
			"updated_at":                     now,
		}})
		log.Error("AUTO_REFUND", "Gateway refund outcome is ambiguous; reconciliation required", "order_id", order.ID, "err", refundErr)
		return
	}

	result, updateErr := h.db.Collection("orders").UpdateOne(ctx,
		bson.M{"_id": order.ID, "payment_status": "refund_pending", "refund_processing": true, "refund_reconciliation_required": bson.M{"$ne": true}},
		bson.M{
			"$set": bson.M{
				"payment_status": "refunded", "refund_id": refundID, "refund_amount": amount,
				"refund_requested_amount": 0, "refunded_at": now, "updated_at": now,
			},
			"$push":  bson.M{"refunds": bson.M{"refund_id": refundID, "amount": amount, "processed_at": now}},
			"$unset": bson.M{"refund_processing": "", "refund_processing_started_at": ""},
		},
	)
	if updateErr != nil || result.MatchedCount == 0 {
		_, _ = h.db.Collection("orders").UpdateOne(context.Background(), bson.M{"_id": order.ID}, bson.M{"$set": bson.M{
			"refund_reconciliation_required": true, "refund_reconciliation_id": refundID, "updated_at": time.Now(),
		}})
		log.Error("AUTO_REFUND", "Provider refund succeeded but order reconciliation failed", "order_id", order.ID, "refund_id", refundID, "err", updateErr)
		return
	}
	log.Info("AUTO_REFUND", "Refund processed", "order_id", order.ID, "refund_id", refundID, "amount", amount)
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
