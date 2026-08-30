package order

import (
	"context"
	"time"

	"ecom-core-service/internal/config"
	"ecom-core-service/internal/models"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// StartExpirySweeper launches a background goroutine that periodically expires stale
// pending-payment orders, restoring their stock and coupon usage. It is idempotent:
// each order is only expired once (guarded by stock_released/coupon_released flags).
// The sweeper runs every 5 minutes and also on startup after a 30s delay.
func StartExpirySweeper(db *mongo.Database, cfg *config.Config) {
	ttl := time.Duration(cfg.PendingOrderTTLMinutes) * time.Minute
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}

	go func() {
		// Initial delay so the server finishes booting
		time.Sleep(30 * time.Second)
		sweep(db, ttl)

		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			sweep(db, ttl)
		}
	}()

	log.Info("EXPIRY_SWEEPER", "Started", "ttl_minutes", cfg.PendingOrderTTLMinutes)
}

func sweep(db *mongo.Database, ttl time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cutoff := time.Now().Add(-ttl)

	// Find orders that are placed+pending and older than the TTL
	filter := bson.M{
		"payment_status": "pending",
		"status":         bson.M{"$in": []string{"placed"}},
		"created_at":     bson.M{"$lt": cutoff},
	}

	cursor, err := db.Collection("orders").Find(ctx, filter, options.Find().SetLimit(100))
	if err != nil {
		log.Error("EXPIRY_SWEEPER", "Failed to query stale orders", "err", err)
		return
	}
	defer cursor.Close(ctx)

	expired := 0
	for cursor.Next(ctx) {
		var order models.Order
		if err := cursor.Decode(&order); err != nil {
			continue
		}
		if ExpireOrder(ctx, db, &order) {
			expired++
		}
	}

	if expired > 0 {
		log.Info("EXPIRY_SWEEPER", "Expired stale orders", "count", expired)
	}

	// Purge abandoned/expired orders older than 24h — they never became real orders
	// and no gateway will complete a payment that late. Stock/coupon were already
	// released at transition time, so a hard delete is safe.
	purgeCutoff := time.Now().Add(-24 * time.Hour)
	if res, pErr := db.Collection("orders").DeleteMany(ctx, bson.M{
		"status":     bson.M{"$in": []string{"abandoned", "expired"}},
		"updated_at": bson.M{"$lt": purgeCutoff},
	}); pErr == nil && res.DeletedCount > 0 {
		log.Info("EXPIRY_SWEEPER", "Purged dead orders", "count", res.DeletedCount)
	}
}

// ExpireOrder expires a single pending-payment order, restoring stock and coupon usage.
// It is idempotent -- safe to call multiple times on the same order. Returns true if
// the order was actually transitioned (first call), false if already expired/cancelled.
func ExpireOrder(ctx context.Context, db *mongo.Database, order *models.Order) bool {
	now := time.Now()

	// Atomically mark as expired only if still in placed+pending state
	res, err := db.Collection("orders").UpdateOne(ctx,
		bson.M{
			"_id":            order.ID,
			"status":         "placed",
			"payment_status": "pending",
		},
		bson.M{"$set": bson.M{
			"status":          "expired",
			"cancel_reason":   "Payment not completed within allowed time",
			"cancelled_at":    now,
			"updated_at":      now,
		}},
	)
	if err != nil || res.MatchedCount == 0 {
		return false // already transitioned
	}

	// Restore stock (idempotent via stock_released flag)
	ReleaseStock(ctx, db, order)

	// Release coupon (idempotent via coupon_released flag)
	ReleaseCoupon(ctx, db, order)

	log.Info("EXPIRE_ORDER", "Order expired", "order_id", order.ID, "order_number", order.OrderNumber, "age_minutes", int(time.Since(order.CreatedAt).Minutes()))
	return true
}

// ReleaseStock restores stock for an order's items. Idempotent via stock_released flag.
func ReleaseStock(ctx context.Context, db *mongo.Database, order *models.Order) {
	res, err := db.Collection("orders").UpdateOne(ctx,
		bson.M{"_id": order.ID, "stock_released": bson.M{"$ne": true}},
		bson.M{"$set": bson.M{"stock_released": true}},
	)
	if err != nil || res.MatchedCount == 0 {
		return // already released
	}

	for _, item := range order.Items {
		db.Collection("products").UpdateOne(ctx,
			bson.M{"_id": item.ProductID},
			bson.M{"$inc": bson.M{"stock": item.Quantity}},
		)
	}
}

// ReleaseCoupon decrements coupon usage for an order. Idempotent via coupon_released flag.
// Only decrements if coupon_used is true (coupon was actually applied).
func ReleaseCoupon(ctx context.Context, db *mongo.Database, order *models.Order) {
	if order.CouponCode == "" {
		return
	}

	res, err := db.Collection("orders").UpdateOne(ctx,
		bson.M{
			"_id":             order.ID,
			"coupon_used":     true,
			"coupon_released": bson.M{"$ne": true},
		},
		bson.M{"$set": bson.M{"coupon_released": true}},
	)
	if err != nil || res.MatchedCount == 0 {
		return // not applicable or already released
	}

	db.Collection("coupons").UpdateOne(ctx,
		bson.M{"code": order.CouponCode, "used_count": bson.M{"$gt": 0}},
		bson.M{"$inc": bson.M{"used_count": -1}},
	)
}

// AbandonOrder marks an unpaid order as abandoned (user dismissed/failed payment),
// releasing stock and coupon. Atomic: only transitions placed+pending orders.
// Returns true if this call performed the transition.
func AbandonOrder(ctx context.Context, db *mongo.Database, order *models.Order) bool {
	now := time.Now()
	res, err := db.Collection("orders").UpdateOne(ctx,
		bson.M{"_id": order.ID, "status": "placed", "payment_status": "pending"},
		bson.M{"$set": bson.M{
			"status":        "abandoned",
			"cancel_reason": "Payment window dismissed or payment failed",
			"updated_at":    now,
		}},
	)
	if err != nil || res.MatchedCount == 0 {
		return false // already paid / expired / abandoned
	}
	ReleaseStock(ctx, db, order)
	ReleaseCoupon(ctx, db, order)
	log.Info("ABANDON_ORDER", "Order abandoned", "order_id", order.ID, "order_number", order.OrderNumber)
	return true
}

// ReclaimStock atomically re-reserves stock for an abandoned order that got paid late
// (e.g. UPI approved after modal dismissal). All-or-nothing: rolls back partial
// reservations on failure. Only acts if stock was actually released.
func ReclaimStock(ctx context.Context, db *mongo.Database, order *models.Order) bool {
	// Flip the flag back first — mirrors ReleaseStock's idempotency guard.
	res, err := db.Collection("orders").UpdateOne(ctx,
		bson.M{"_id": order.ID, "stock_released": true},
		bson.M{"$set": bson.M{"stock_released": false}},
	)
	if err != nil {
		return false
	}
	if res.MatchedCount == 0 {
		return true // stock was never released — still reserved, nothing to do
	}

	reserved := make([]models.OrderItem, 0, len(order.Items))
	for _, item := range order.Items {
		r, dErr := db.Collection("products").UpdateOne(ctx,
			bson.M{"_id": item.ProductID, "is_active": true, "stock": bson.M{"$gte": item.Quantity}},
			bson.M{"$inc": bson.M{"stock": -item.Quantity}},
		)
		if dErr != nil || r.MatchedCount == 0 {
			// Roll back what we reserved and restore the released flag.
			for _, done := range reserved {
				db.Collection("products").UpdateOne(ctx,
					bson.M{"_id": done.ProductID},
					bson.M{"$inc": bson.M{"stock": done.Quantity}},
				)
			}
			db.Collection("orders").UpdateOne(ctx,
				bson.M{"_id": order.ID},
				bson.M{"$set": bson.M{"stock_released": true}},
			)
			log.Warn("RECLAIM_STOCK", "Stock no longer available for late-paid order", "order_id", order.ID, "product_id", item.ProductID, "qty", item.Quantity)
			return false
		}
		reserved = append(reserved, item)
	}
	log.Info("RECLAIM_STOCK", "Stock re-reserved for late-paid order", "order_id", order.ID)
	return true
}

// ReclaimCoupon re-consumes the coupon for an abandoned order that got paid late.
// Mirrors ReleaseCoupon's flags. Best-effort: a fully-used coupon does not block
// the order (customer already paid the discounted amount).
func ReclaimCoupon(ctx context.Context, db *mongo.Database, order *models.Order) {
	if order.CouponCode == "" {
		return
	}
	res, err := db.Collection("orders").UpdateOne(ctx,
		bson.M{"_id": order.ID, "coupon_used": true, "coupon_released": true},
		bson.M{"$set": bson.M{"coupon_released": false}},
	)
	if err != nil || res.MatchedCount == 0 {
		return // never released
	}
	db.Collection("coupons").UpdateOne(ctx,
		bson.M{"code": order.CouponCode},
		bson.M{"$inc": bson.M{"used_count": 1}},
	)
}

// IsOrderExpired checks if a pending-payment order has exceeded the TTL.
func IsOrderExpired(order *models.Order, ttlMinutes int) bool {
	if order.Status != "placed" || order.PaymentStatus != "pending" {
		return false
	}
	ttl := time.Duration(ttlMinutes) * time.Minute
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	return time.Since(order.CreatedAt) > ttl
}
