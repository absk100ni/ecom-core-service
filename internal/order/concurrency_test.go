package order

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ecom-core-service/internal/config"
	"ecom-core-service/internal/models"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func concurrencyDB(t *testing.T) *mongo.Database {
	uri := os.Getenv("OPS_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("set OPS_TEST_MONGO_URI for Mongo concurrency tests")
	}
	client, err := mongo.Connect(context.Background(), options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	db := client.Database("order_concurrency_" + uuid.NewString())
	t.Cleanup(func() { _ = db.Drop(context.Background()); _ = client.Disconnect(context.Background()) })
	return db
}

func TestOneStockAllowsExactlyOneConcurrentReservation(t *testing.T) {
	db := concurrencyDB(t)
	product := models.Product{ID: "product-1", Name: "Last Item", Category: "test", Price: 10000, Stock: 1, SKU: "LAST-1", IsActive: true, Variants: []models.Variant{}, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if _, err := db.Collection("products").InsertOne(context.Background(), product); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{db: db, cfg: &config.Config{PartialPaymentEnabled: false}}
	item := models.CartItem{ProductID: product.ID, Name: product.Name, Price: product.Price, Quantity: 1}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for index := 0; index < 20; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			order, _, stock, err := handler.buildOrder(context.Background(), "user", []models.CartItem{item}, models.Address{}, "", "", "full", false, uuid.NewString())
			if err == nil && stock == nil && order != nil {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("successful reservations = %d, want 1", successes.Load())
	}
	var stored models.Product
	if err := db.Collection("products").FindOne(context.Background(), bson.M{"_id": product.ID}).Decode(&stored); err != nil {
		t.Fatal(err)
	}
	if stored.Stock != 0 {
		t.Fatalf("remaining stock = %d, want 0", stored.Stock)
	}
}

func TestArchiveDeadOrdersRetainsReconciliationEvidence(t *testing.T) {
	db := concurrencyDB(t)
	ctx := context.Background()
	old := time.Now().Add(-48 * time.Hour)
	_, err := db.Collection("orders").InsertMany(ctx, []interface{}{
		bson.M{"_id": "expired-old", "status": "expired", "updated_at": old, "payment_id": "pay_late", "stock_released": true},
		bson.M{"_id": "abandoned-old", "status": "abandoned", "updated_at": old, "coupon_released": true},
		bson.M{"_id": "expired-new", "status": "expired", "updated_at": time.Now()},
	})
	if err != nil {
		t.Fatal(err)
	}
	count, err := archiveDeadOrders(ctx, db, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("archived count = %d, want 2", count)
	}
	var retained bson.M
	if err := db.Collection("orders").FindOne(ctx, bson.M{"_id": "expired-old"}).Decode(&retained); err != nil {
		t.Fatalf("archived order was deleted: %v", err)
	}
	if retained["payment_id"] != "pay_late" || retained["archived_at"] == nil {
		t.Fatalf("reconciliation evidence not retained: %#v", retained)
	}
	if again, err := archiveDeadOrders(ctx, db, time.Now().Add(-24*time.Hour)); err != nil || again != 0 {
		t.Fatalf("archive must be idempotent, count=%d err=%v", again, err)
	}
}

func TestAutoRefundTimeoutKeepsClaimLocked(t *testing.T) {
	db := concurrencyDB(t)
	ctx := context.Background()
	order := models.Order{ID: "refund-order", PaymentStatus: "refund_pending", PaymentID: "pay_123", Total: 5000, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if _, err := db.Collection("orders").InsertOne(ctx, order); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	handler := &Handler{db: db, cfg: &config.Config{}, refundPayment: func(string, int) (string, error) {
		calls.Add(1)
		return "", context.DeadlineExceeded
	}}
	handler.attemptAutoRefund(ctx, &order, order.Total)
	handler.attemptAutoRefund(ctx, &order, order.Total)
	if calls.Load() != 1 {
		t.Fatalf("provider calls = %d, want 1", calls.Load())
	}
	var stored bson.M
	if err := db.Collection("orders").FindOne(ctx, bson.M{"_id": order.ID}).Decode(&stored); err != nil {
		t.Fatal(err)
	}
	if stored["refund_processing"] != true || stored["refund_reconciliation_required"] != true {
		t.Fatalf("ambiguous refund claim was not locked: %#v", stored)
	}
}
