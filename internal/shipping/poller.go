package shipping

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"ecom-core-service/internal/models"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	trackingWakeInterval = 5 * time.Minute
	trackingBatchSize    = 25
	trackingConcurrency  = 3
)

func (h *Handler) StartTrackingPoller(ctx context.Context) {
	if h.cfg.ShipmozoPublicKey == "" || h.cfg.ShipmozoPrivateKey == "" {
		return
	}
	go func() {
		h.pollTrackingBatch(ctx)
		ticker := time.NewTicker(trackingWakeInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.pollTrackingBatch(ctx)
			}
		}
	}()
	log.Info("TRACK_POLLER", "Started", "wake_minutes", int(trackingWakeInterval.Minutes()), "batch_size", trackingBatchSize, "concurrency", trackingConcurrency)
}

func (h *Handler) pollTrackingBatch(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	if !h.acquireTrackingLease(ctx) {
		return
	}
	now := time.Now()
	filter := bson.M{"provider": "shipmozo", "awb": bson.M{"$ne": ""}, "tracking_terminal": bson.M{"$ne": true}, "$or": []bson.M{{"next_tracking_poll_at": bson.M{"$lte": now}}, {"next_tracking_poll_at": bson.M{"$exists": false}}}}
	cursor, err := h.db.Collection("shipments").Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "next_tracking_poll_at", Value: 1}}).SetLimit(trackingBatchSize))
	if err != nil {
		log.Error("TRACK_POLLER", "Query failed", "err", err)
		return
	}
	var shipments []models.Shipment
	if err := cursor.All(ctx, &shipments); err != nil {
		cursor.Close(ctx)
		return
	}
	cursor.Close(ctx)
	sem := make(chan struct{}, trackingConcurrency)
	var wg sync.WaitGroup
	for _, shipment := range shipments {
		shipment := shipment
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			h.pollShipment(parent, &shipment)
		}()
	}
	wg.Wait()
}

func (h *Handler) acquireTrackingLease(ctx context.Context) bool {
	now := time.Now()
	owner := fmt.Sprintf("%d", now.UnixNano())
	result := h.db.Collection("ops_leases").FindOneAndUpdate(ctx, bson.M{"_id": "shipmozo-tracking-poller", "$or": []bson.M{{"expires_at": bson.M{"$lt": now}}, {"owner": owner}}}, bson.M{"$set": bson.M{"owner": owner, "expires_at": now.Add(4 * time.Minute), "updated_at": now}}, options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After))
	return result.Err() == nil
}

func (h *Handler) pollShipment(parent context.Context, shipment *models.Shipment) {
	now := time.Now()
	leaseUntil := now.Add(2 * time.Minute)
	claim, err := h.db.Collection("shipments").UpdateOne(parent, bson.M{"_id": shipment.ID, "tracking_terminal": bson.M{"$ne": true}, "$or": []bson.M{{"tracking_lease_until": bson.M{"$lt": now}}, {"tracking_lease_until": bson.M{"$exists": false}}}}, bson.M{"$set": bson.M{"tracking_lease_until": leaseUntil}})
	if err != nil || claim.MatchedCount == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 12*time.Second)
	defer cancel()
	snapshot, trackErr := h.provider.Track(ctx, shipment.AWB)
	if trackErr != nil {
		h.recordTrackingFailure(parent, shipment, trackErr)
		return
	}
	status, terminal := normalizeProviderStatus(snapshot.CurrentStatus)
	events := mergeTrackingEvents(shipment.Events, snapshot.Events)
	next := now.Add(withJitter(trackingInterval(status)))
	update := bson.M{"$set": bson.M{"provider_status": snapshot.CurrentStatus, "status": status, "events": events, "est_delivery": snapshot.ExpectedDeliveryDate, "last_tracking_poll_at": now, "next_tracking_poll_at": next, "tracking_poll_failures": 0, "tracking_terminal": terminal, "updated_at": now}, "$unset": bson.M{"tracking_lease_until": ""}}
	if terminal {
		update["$unset"].(bson.M)["next_tracking_poll_at"] = ""
	}
	_, _ = h.db.Collection("shipments").UpdateOne(parent, bson.M{"_id": shipment.ID}, update)
	h.updateOrderFromTracking(parent, shipment.OrderID, status, now)
}

func (h *Handler) recordTrackingFailure(ctx context.Context, shipment *models.Shipment, trackErr error) {
	failures := shipment.TrackingPollFailures + 1
	delay := trackingBackoff(failures)
	now := time.Now()
	_, _ = h.db.Collection("shipments").UpdateOne(ctx, bson.M{"_id": shipment.ID}, bson.M{"$set": bson.M{"last_tracking_poll_at": now, "next_tracking_poll_at": now.Add(delay), "updated_at": now}, "$inc": bson.M{"tracking_poll_failures": 1}, "$unset": bson.M{"tracking_lease_until": ""}})
	log.Warn("TRACK_POLLER", "Tracking poll failed", "shipment_id", shipment.ID, "failures", failures, "next_minutes", int(delay.Minutes()), "err", trackErr)
	if failures >= 8 {
		_, _ = h.db.Collection("ops_anomalies").UpdateOne(ctx, bson.M{"_id": "OPS_TRACKING_POLL_FAILED:" + shipment.ID}, bson.M{"$set": bson.M{"event_code": "OPS_TRACKING_POLL_FAILED", "severity": "high", "resource_type": "shipment", "resource_id": shipment.ID, "summary": "Shipmozo tracking repeatedly failed", "recommendation": "Check Shipmozo availability and the AWB before manual action", "active": true, "last_seen": now}, "$setOnInsert": bson.M{"first_seen": now}, "$inc": bson.M{"occurrences": 1}}, options.Update().SetUpsert(true))
	}
}

func (h *Handler) updateOrderFromTracking(ctx context.Context, orderID, status string, now time.Time) {
	if orderID == "" {
		return
	}
	set := bson.M{"updated_at": now}
	switch status {
	case "picked_up", "in_transit":
		set["status"] = "shipped"
	case "out_for_delivery":
		set["status"] = "out_for_delivery"
	case "delivered":
		set["status"] = "delivered"
		set["delivered_at"] = now
		var order models.Order
		if h.db.Collection("orders").FindOne(ctx, bson.M{"_id": orderID}).Decode(&order) == nil && order.CODAmount > 0 {
			set["cod_collected"] = true
		}
	case "cancelled":
		// Provider cancellation is a prerequisite for changing the local order to
		// cancelled. Record the confirmation, then let the normal cancellation
		// path release stock/coupons and initiate any refund.
		set["shipmozo_push_status"] = "cancelled"
	default:
		return
	}
	_, _ = h.db.Collection("orders").UpdateOne(ctx, bson.M{"_id": orderID, "status": bson.M{"$nin": []string{"cancelled", "returned", "refunded"}}}, bson.M{"$set": set})
}

func normalizeProviderStatus(raw string) (string, bool) {
	value := strings.ToLower(strings.TrimSpace(raw))
	value = strings.NewReplacer("-", "_", " ", "_").Replace(value)
	switch {
	case strings.Contains(value, "rto") && strings.Contains(value, "deliver"):
		return "rto_delivered", true
	case strings.Contains(value, "rto"):
		return "rto_in_transit", false
	case strings.Contains(value, "deliver") && !strings.Contains(value, "out_for"):
		return "delivered", true
	case strings.Contains(value, "cancel"):
		return "cancelled", true
	case strings.Contains(value, "out_for_delivery") || strings.Contains(value, "ofd"):
		return "out_for_delivery", false
	case strings.Contains(value, "ndr") || strings.Contains(value, "undeliver") || strings.Contains(value, "failed"):
		return "delivery_failed", false
	case strings.Contains(value, "picked") || strings.Contains(value, "pickup_done"):
		return "picked_up", false
	case strings.Contains(value, "transit") || strings.Contains(value, "destination_hub"):
		return "in_transit", false
	case strings.Contains(value, "pickup") || strings.Contains(value, "booked") || strings.Contains(value, "assigned"):
		return "booked", false
	default:
		return value, false
	}
}

func trackingInterval(status string) time.Duration {
	switch status {
	case "out_for_delivery":
		return 10 * time.Minute
	case "in_transit", "picked_up":
		return 30 * time.Minute
	case "delivery_failed":
		return 30 * time.Minute
	case "rto_in_transit":
		return 60 * time.Minute
	default:
		return 60 * time.Minute
	}
}
func trackingBackoff(failures int) time.Duration {
	intervals := []time.Duration{30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour}
	index := failures - 1
	if index < 0 {
		index = 0
	}
	if index >= len(intervals) {
		index = len(intervals) - 1
	}
	return intervals[index]
}
func withJitter(base time.Duration) time.Duration {
	spread := int64(base) / 10
	if spread <= 0 {
		return base
	}
	return base + time.Duration(rand.Int63n(spread*2+1)-spread)
}

func mergeTrackingEvents(existing, incoming []models.TrackingEvent) []models.TrackingEvent {
	seen := map[string]bool{}
	merged := make([]models.TrackingEvent, 0, len(existing)+len(incoming))
	for _, list := range [][]models.TrackingEvent{existing, incoming} {
		for _, event := range list {
			key := event.Status + "|" + event.Description + "|" + event.Timestamp.UTC().Format(time.RFC3339Nano)
			if !seen[key] {
				seen[key] = true
				merged = append(merged, event)
			}
		}
	}
	return merged
}
