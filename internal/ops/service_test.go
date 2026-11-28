package ops

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"ecom-core-service/internal/config"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestLiveDoesNotRequireDependencies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/live", Live)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/live", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("live status = %d", response.Code)
	}
}

func TestBoundedPagination(t *testing.T) {
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	context.Request = httptest.NewRequest(http.MethodGet, "/?page=-2&limit=900", nil)
	page, limit := boundedPagination(context)
	if page != 1 || limit != 100 {
		t.Fatalf("got page=%d limit=%d", page, limit)
	}
}

func TestAuditRoutesCoverCriticalMutations(t *testing.T) {
	required := []string{
		"PUT /api/v1/admin/products/:id", "PUT /api/v1/admin/order/:id/status",
		"POST /api/v1/admin/orders/:id/refund", "POST /api/v1/admin/orders/:id/ship",
		"PUT /api/v1/admin/returns/:id", "PUT /api/v1/admin/reviews/:id/approve",
	}
	for _, route := range required {
		if _, ok := auditRoutes[route]; !ok {
			t.Errorf("missing audit route %s", route)
		}
	}
}

func integrationDB(t *testing.T) *mongo.Database {
	uri := os.Getenv("OPS_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("set OPS_TEST_MONGO_URI for Mongo-backed ops tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	db := client.Database("ops_test_" + uuid.NewString())
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = db.Drop(cleanup)
		_ = client.Disconnect(cleanup)
	})
	return db
}

func TestMonitorLeaseAndAnomalyLifecycle(t *testing.T) {
	db := integrationDB(t)
	now := time.Now().UTC()
	cfg := &config.Config{PendingOrderTTLMinutes: 30, OpsShipmentStaleMinutes: 100000, OpsRefundStaleMinutes: 60}
	first := New(db, cfg, nil)
	second := New(db, cfg, nil)
	first.now = func() time.Time { return now }
	second.now = first.now
	ctx := context.Background()
	if !first.acquireLease(ctx) {
		t.Fatal("first monitor should acquire lease")
	}
	if second.acquireLease(ctx) {
		t.Fatal("second monitor must not acquire active lease")
	}
	_, err := db.Collection("orders").InsertOne(ctx, bson.M{"_id": "order-1", "status": "pending", "payment_status": "pending", "stock_released": false, "created_at": now.Add(-2 * time.Hour), "updated_at": now.Add(-2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	var anomaly Anomaly
	if err := db.Collection("ops_anomalies").FindOne(ctx, bson.M{"_id": EventPendingPaymentStale + ":order-1"}).Decode(&anomaly); err != nil {
		t.Fatal(err)
	}
	if !anomaly.Active || anomaly.Occurrences != 1 {
		t.Fatalf("unexpected anomaly: %+v", anomaly)
	}
	if err := first.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Collection("ops_anomalies").FindOne(ctx, bson.M{"_id": anomaly.Key}).Decode(&anomaly); err != nil {
		t.Fatal(err)
	}
	if anomaly.Occurrences != 2 {
		t.Fatalf("expected dedupe upsert occurrence=2, got %d", anomaly.Occurrences)
	}
	_, _ = db.Collection("orders").UpdateOne(ctx, bson.M{"_id": "order-1"}, bson.M{"$set": bson.M{"status": "confirmed", "shipment_id": "shipment-1", "stock_released": true}})
	if err := first.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Collection("ops_anomalies").FindOne(ctx, bson.M{"_id": anomaly.Key}).Decode(&anomaly); err != nil {
		t.Fatal(err)
	}
	if anomaly.Active {
		t.Fatal("anomaly should resolve when condition clears")
	}
}

func TestPaymentAndShipmentMismatchAnomalies(t *testing.T) {
	db := integrationDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	_, err := db.Collection("orders").InsertMany(ctx, []interface{}{
		bson.M{"_id": "payment-mismatch", "status": "placed", "payment_status": "pending", "payment_validation_mismatch": true, "created_at": now, "updated_at": now},
		bson.M{"_id": "shipment-mismatch", "status": "processing", "payment_status": "paid", "shipment_cancellation_blocked": true, "shipment_id": "shipment-1", "created_at": now, "updated_at": now},
	})
	if err != nil {
		t.Fatal(err)
	}
	service := New(db, &config.Config{PendingOrderTTLMinutes: 30, OpsShipmentStaleMinutes: 100000, OpsRefundStaleMinutes: 60}, nil)
	service.now = func() time.Time { return now }
	if err := service.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{EventPaymentAmountMismatch + ":payment-mismatch", EventShipmentCancelMismatch + ":shipment-mismatch"} {
		var anomaly Anomaly
		if err := db.Collection("ops_anomalies").FindOne(ctx, bson.M{"_id": key, "active": true}).Decode(&anomaly); err != nil {
			t.Fatalf("missing active anomaly %s: %v", key, err)
		}
	}
}

func TestAuditMiddlewareStoresOnlySafeSnapshot(t *testing.T) {
	db := integrationDB(t)
	ctx := context.Background()
	_, _ = db.Collection("products").InsertOne(ctx, bson.M{"_id": "product-1", "name": "Work light", "price": 100, "stock": 2, "shipping_address": bson.M{"phone": "secret"}})
	gin.SetMode(gin.TestMode)
	router := gin.New()
	group := router.Group("/api/v1/admin")
	group.Use(func(c *gin.Context) {
		c.Set("user_id", "admin-1")
		c.Set("email", "admin@example.com")
		c.Set("request_id", "req-1")
	})
	group.Use(AuditMiddleware(db))
	group.PUT("/products/:id", func(c *gin.Context) {
		_, _ = db.Collection("products").UpdateOne(c, bson.M{"_id": c.Param("id")}, bson.M{"$set": bson.M{"price": 200}})
		c.Status(http.StatusOK)
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/api/v1/admin/products/product-1", nil))
	var event AuditEvent
	if err := db.Collection("admin_audit_events").FindOne(ctx, bson.M{}).Decode(&event); err != nil {
		t.Fatal(err)
	}
	if event.Action != "product.update" || event.RequestID != "req-1" {
		t.Fatalf("unexpected audit event: %+v", event)
	}
	if _, leaked := event.NewState["shipping_address"]; leaked {
		t.Fatal("audit snapshot leaked unapproved field")
	}
}

func TestReadyResponseDoesNotExposeDependencyDetails(t *testing.T) {
	db := integrationDB(t)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/ready", New(db, &config.Config{Environment: "development"}, nil).Ready)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("ready status = %d", response.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, exposed := body["checks"]; exposed {
		t.Fatal("public readiness response must not expose dependency configuration")
	}
}
