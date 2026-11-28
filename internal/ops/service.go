package ops

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"ecom-core-service/internal/config"
	"ecom-core-service/internal/sentry"
	"ecom-core-service/pkg/logger"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var log = logger.New("OPS", "MONITOR")

const (
	EventPendingPaymentStale    = "OPS_PAYMENT_PENDING_STALE"
	EventPaidStateMismatch      = "OPS_PAYMENT_ORDER_MISMATCH"
	EventShipmentMissing        = "OPS_SHIPMENT_MISSING"
	EventAWBMissing             = "OPS_SHIPMENT_AWB_MISSING"
	EventRefundPending          = "OPS_REFUND_PENDING_STALE"
	EventStockRollback          = "OPS_STOCK_ROLLBACK_INCOMPLETE"
	EventPaymentAmountMismatch  = "OPS_PAYMENT_AMOUNT_MISMATCH"
	EventShipmentCancelMismatch = "OPS_SHIPMENT_CANCELLATION_MISMATCH"
)

type Anomaly struct {
	Key            string    `json:"key" bson:"_id"`
	EventCode      string    `json:"event_code" bson:"event_code"`
	Severity       string    `json:"severity" bson:"severity"`
	ResourceType   string    `json:"resource_type" bson:"resource_type"`
	ResourceID     string    `json:"resource_id" bson:"resource_id"`
	RequestID      string    `json:"request_id,omitempty" bson:"request_id,omitempty"`
	Summary        string    `json:"summary" bson:"summary"`
	Recommendation string    `json:"recommendation" bson:"recommendation"`
	AgeSeconds     int64     `json:"age_seconds" bson:"age_seconds"`
	Active         bool      `json:"active" bson:"active"`
	Occurrences    int       `json:"occurrences" bson:"occurrences"`
	FirstSeen      time.Time `json:"first_seen" bson:"first_seen"`
	LastSeen       time.Time `json:"last_seen" bson:"last_seen"`
	ResolvedAt     time.Time `json:"resolved_at,omitempty" bson:"resolved_at,omitempty"`
}

type Service struct {
	db     *mongo.Database
	cfg    *config.Config
	sentry *sentry.Client
	owner  string
	now    func() time.Time
}

func New(db *mongo.Database, cfg *config.Config, sentryClient *sentry.Client) *Service {
	return &Service{db: db, cfg: cfg, sentry: sentryClient, owner: uuid.NewString(), now: time.Now}
}

func Live(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "live", "service": "ecom-core-service", "time": time.Now().UTC().Format(time.RFC3339)})
}

func (s *Service) Ready(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	checks := config.ProductionChecks(s.cfg)
	mongoReady := s.db.Client().Ping(ctx, nil) == nil
	checks = append(checks, config.Check{Name: "mongodb", Required: true, Ready: mongoReady, Detail: "MongoDB connection and critical indexes must be available"})
	ready := mongoReady
	if s.cfg.Environment == "production" {
		for _, check := range checks {
			if check.Required && !check.Ready {
				ready = false
			}
		}
	}
	status := http.StatusOK
	state := "ready"
	if !ready {
		status = http.StatusServiceUnavailable
		state = "not_ready"
	}
	c.JSON(status, gin.H{"status": state})
}

func (s *Service) Status(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	counts := bson.M{}
	cursor, err := s.db.Collection("ops_anomalies").Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.M{"active": true}}},
		{{Key: "$group", Value: bson.M{"_id": "$severity", "count": bson.M{"$sum": 1}}}},
	})
	if err == nil {
		defer cursor.Close(ctx)
		var rows []struct {
			Severity string `bson:"_id"`
			Count    int    `bson:"count"`
		}
		_ = cursor.All(ctx, &rows)
		for _, row := range rows {
			counts[row.Severity] = row.Count
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"service": "ecom-core-service", "environment": s.cfg.Environment,
		"configuration": config.ProductionChecks(s.cfg), "active_anomalies": counts,
		"monitor_interval_minutes": s.cfg.OpsMonitorIntervalMinutes,
	})
}

func (s *Service) ListAnomalies(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	page, limit := boundedPagination(c)
	filter := bson.M{}
	if c.Query("active") != "all" {
		filter["active"] = true
	}
	if value := c.Query("severity"); value != "" {
		filter["severity"] = value
	}
	if value := c.Query("event_code"); value != "" {
		filter["event_code"] = value
	}
	if value := c.Query("resource_id"); value != "" {
		filter["resource_id"] = value
	}
	total, _ := s.db.Collection("ops_anomalies").CountDocuments(ctx, filter)
	cursor, err := s.db.Collection("ops_anomalies").Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "last_seen", Value: -1}}).SetSkip(int64((page-1)*limit)).SetLimit(int64(limit)))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list operational anomalies"})
		return
	}
	defer cursor.Close(ctx)
	items := []Anomaly{}
	if err := cursor.All(ctx, &items); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to decode operational anomalies"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"anomalies": items, "total": total, "page": page, "limit": limit})
}

func boundedPagination(c *gin.Context) (int, int) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if limit < 1 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	return page, limit
}

func (s *Service) Start(ctx context.Context) {
	interval := time.Duration(s.cfg.OpsMonitorIntervalMinutes) * time.Minute
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	go func() {
		s.scanIfLeader(ctx)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.scanIfLeader(ctx)
			}
		}
	}()
}

func (s *Service) scanIfLeader(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	if !s.acquireLease(ctx) {
		return
	}
	if err := s.Scan(ctx); err != nil {
		log.Error("SCAN", "Operational scan failed", "err", err)
	}
}

func (s *Service) acquireLease(ctx context.Context) bool {
	now := s.now()
	result := s.db.Collection("ops_leases").FindOneAndUpdate(ctx,
		bson.M{"_id": "ops-monitor", "$or": []bson.M{{"expires_at": bson.M{"$lt": now}}, {"owner": s.owner}}},
		bson.M{"$set": bson.M{"owner": s.owner, "expires_at": now.Add(2 * time.Minute), "updated_at": now}},
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After),
	)
	return result.Err() == nil
}

func (s *Service) Scan(ctx context.Context) error {
	now := s.now()
	seen := map[string]bool{}
	orderChecks := []struct {
		code, severity, summary, recommendation string
		filter                                  bson.M
		cutoff                                  time.Time
	}{
		{EventPendingPaymentStale, "high", "Order remains pending beyond the payment window", "Confirm provider payment state before expiring or retrying the order", bson.M{"status": "pending", "payment_status": "pending", "created_at": bson.M{"$lt": now.Add(-time.Duration(s.cfg.PendingOrderTTLMinutes) * time.Minute)}}, now},
		{EventPaidStateMismatch, "critical", "Paid order is in a non-fulfillable state", "Compare the order with the payment provider before changing any state", bson.M{"payment_status": "paid", "status": bson.M{"$in": []string{"pending", "cancelled", "expired"}}}, now},
		{EventShipmentMissing, "high", "Confirmed order has no recorded shipment", "Check fulfillment readiness and Shipmozo before creating a shipment", bson.M{"status": bson.M{"$in": []string{"confirmed", "processing"}}, "created_at": bson.M{"$lt": now.Add(-time.Duration(s.cfg.OpsShipmentStaleMinutes) * time.Minute)}, "$or": []bson.M{{"shipment_id": ""}, {"shipment_id": bson.M{"$exists": false}}}}, now},
		{EventRefundPending, "critical", "Refund remains pending beyond the reconciliation window", "Compare the refund with the payment provider before retrying", bson.M{"payment_status": "refund_pending", "updated_at": bson.M{"$lt": now.Add(-time.Duration(s.cfg.OpsRefundStaleMinutes) * time.Minute)}}, now},
		{EventStockRollback, "high", "Cancelled or expired order has no completed stock release", "Inspect stock movement before manually correcting inventory", bson.M{"status": bson.M{"$in": []string{"cancelled", "expired"}}, "stock_released": bson.M{"$ne": true}}, now},
		{EventPaymentAmountMismatch, "critical", "Provider payment amount or currency disagrees with the order payment", "Do not fulfill or refund until the provider payment is reconciled", bson.M{"payment_validation_mismatch": true}, now},
		{EventShipmentCancelMismatch, "critical", "Local cancellation was blocked because the order still exists in Shipmozo", "Cancel the order in Shipmozo and wait for provider confirmation before retrying locally", bson.M{"shipment_cancellation_blocked": true}, now},
	}
	for _, check := range orderChecks {
		cursor, err := s.db.Collection("orders").Find(ctx, check.filter, options.Find().SetProjection(bson.M{"_id": 1, "created_at": 1, "updated_at": 1}))
		if err != nil {
			return err
		}
		var rows []struct {
			ID        string    `bson:"_id"`
			CreatedAt time.Time `bson:"created_at"`
			UpdatedAt time.Time `bson:"updated_at"`
		}
		if err := cursor.All(ctx, &rows); err != nil {
			cursor.Close(ctx)
			return err
		}
		cursor.Close(ctx)
		for _, row := range rows {
			basis := row.UpdatedAt
			if basis.IsZero() {
				basis = row.CreatedAt
			}
			age := int64(now.Sub(basis).Seconds())
			if age < 0 {
				age = 0
			}
			a := Anomaly{Key: check.code + ":" + row.ID, EventCode: check.code, Severity: check.severity, ResourceType: "order", ResourceID: row.ID, Summary: check.summary, Recommendation: check.recommendation, AgeSeconds: age, Active: true}
			if err := s.record(ctx, a); err != nil {
				return err
			}
			seen[a.Key] = true
		}
	}
	// Cross-check successful payment records against their orders. This is read-only;
	// remediation always requires provider verification.
	paymentCursor, paymentErr := s.db.Collection("payments").Find(ctx, bson.M{"status": "paid"}, options.Find().SetProjection(bson.M{"_id": 1, "order_id": 1, "updated_at": 1}))
	if paymentErr == nil {
		var payments []struct {
			ID        string    `bson:"_id"`
			OrderID   string    `bson:"order_id"`
			UpdatedAt time.Time `bson:"updated_at"`
		}
		_ = paymentCursor.All(ctx, &payments)
		paymentCursor.Close(ctx)
		for _, payment := range payments {
			var orderState struct {
				PaymentStatus string `bson:"payment_status"`
			}
			err := s.db.Collection("orders").FindOne(ctx, bson.M{"_id": payment.OrderID}, options.FindOne().SetProjection(bson.M{"payment_status": 1})).Decode(&orderState)
			if err != nil || (orderState.PaymentStatus != "paid" && orderState.PaymentStatus != "refund_pending" && orderState.PaymentStatus != "refunded") {
				age := int64(now.Sub(payment.UpdatedAt).Seconds())
				if age < 0 {
					age = 0
				}
				a := Anomaly{Key: EventPaidStateMismatch + ":" + payment.ID, EventCode: EventPaidStateMismatch, Severity: "critical", ResourceType: "payment", ResourceID: payment.ID, Summary: "Paid payment record disagrees with its order", Recommendation: "Compare the payment and order with the payment provider before changing state", AgeSeconds: age, Active: true}
				if err := s.record(ctx, a); err != nil {
					return err
				}
				seen[a.Key] = true
			}
		}
	}

	cursor, err := s.db.Collection("shipments").Find(ctx, bson.M{"$or": []bson.M{{"awb": ""}, {"awb": bson.M{"$exists": false}}}, "created_at": bson.M{"$lt": now.Add(-10 * time.Minute)}}, options.Find().SetProjection(bson.M{"_id": 1, "created_at": 1}))
	if err == nil {
		var rows []struct {
			ID        string    `bson:"_id"`
			CreatedAt time.Time `bson:"created_at"`
		}
		_ = cursor.All(ctx, &rows)
		cursor.Close(ctx)
		for _, row := range rows {
			a := Anomaly{Key: EventAWBMissing + ":" + row.ID, EventCode: EventAWBMissing, Severity: "high", ResourceType: "shipment", ResourceID: row.ID, Summary: "Shipment record has no AWB", Recommendation: "Compare the shipment with Shipmozo before booking again", AgeSeconds: int64(now.Sub(row.CreatedAt).Seconds()), Active: true}
			_ = s.record(ctx, a)
			seen[a.Key] = true
		}
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	filter := bson.M{"active": true}
	if len(keys) > 0 {
		filter["_id"] = bson.M{"$nin": keys}
	}
	_, err = s.db.Collection("ops_anomalies").UpdateMany(ctx, filter, bson.M{"$set": bson.M{"active": false, "resolved_at": now}})
	return err
}

func (s *Service) record(ctx context.Context, anomaly Anomaly) error {
	now := s.now()
	result := s.db.Collection("ops_anomalies").FindOneAndUpdate(ctx, bson.M{"_id": anomaly.Key}, bson.M{
		"$setOnInsert": bson.M{"first_seen": now},
		"$set":         bson.M{"event_code": anomaly.EventCode, "severity": anomaly.Severity, "resource_type": anomaly.ResourceType, "resource_id": anomaly.ResourceID, "request_id": anomaly.RequestID, "summary": anomaly.Summary, "recommendation": anomaly.Recommendation, "age_seconds": anomaly.AgeSeconds, "active": true, "last_seen": now},
		"$inc":         bson.M{"occurrences": 1}, "$unset": bson.M{"resolved_at": ""},
	}, options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.Before))
	var previous Anomaly
	resultErr := result.Decode(&previous)
	shouldAlert := resultErr == mongo.ErrNoDocuments || (resultErr == nil && !previous.Active)
	log.Warn("ANOMALY", anomaly.Summary, "event_code", anomaly.EventCode, "severity", anomaly.Severity, "resource_type", anomaly.ResourceType, "resource_id", anomaly.ResourceID)
	if shouldAlert && s.sentry != nil {
		s.sentry.CaptureError(anomaly.Summary, map[string]string{"event_code": anomaly.EventCode, "severity": anomaly.Severity, "resource_type": anomaly.ResourceType, "resource_id": anomaly.ResourceID})
	}
	if resultErr != nil && resultErr != mongo.ErrNoDocuments {
		return fmt.Errorf("upsert anomaly: %w", resultErr)
	}
	return nil
}
