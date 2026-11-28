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
