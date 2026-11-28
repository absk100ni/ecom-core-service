package shipping

import (
	"testing"
	"time"

	"ecom-core-service/internal/models"
)

func TestNormalizeProviderStatus(t *testing.T) {
	tests := []struct {
		raw, want string
		terminal  bool
	}{
		{"Pickup Pending", "booked", false},
		{"In Transit", "in_transit", false},
		{"Out For Delivery", "out_for_delivery", false},
		{"Delivered", "delivered", true},
		{"RTO In-Transit", "rto_in_transit", false},
		{"RTO Delivered", "rto_delivered", true},
		{"Cancelled", "cancelled", true},
	}
	for _, test := range tests {
		got, terminal := normalizeProviderStatus(test.raw)
		if got != test.want || terminal != test.terminal {
			t.Errorf("%q => %q/%v, want %q/%v", test.raw, got, terminal, test.want, test.terminal)
		}
	}
}

func TestAdaptiveTrackingIntervals(t *testing.T) {
	if trackingInterval("booked") != 60*time.Minute {
		t.Fatal("booked should poll hourly")
	}
	if trackingInterval("in_transit") != 30*time.Minute {
		t.Fatal("in transit should poll every 30 minutes")
	}
	if trackingInterval("out_for_delivery") != 10*time.Minute {
		t.Fatal("out for delivery should poll every 10 minutes")
	}
	if trackingBackoff(1) != 30*time.Minute || trackingBackoff(4) != 4*time.Hour || trackingBackoff(20) != 4*time.Hour {
		t.Fatal("unexpected backoff schedule")
	}
}

func TestMergeTrackingEventsIsIdempotent(t *testing.T) {
	timestamp := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	event := models.TrackingEvent{Status: "in_transit", Description: "At hub", Timestamp: timestamp}
	merged := mergeTrackingEvents([]models.TrackingEvent{event}, []models.TrackingEvent{event, {Status: "out_for_delivery", Description: "Vehicle dispatched", Timestamp: timestamp.Add(time.Hour)}})
	if len(merged) != 2 {
		t.Fatalf("merged events = %d, want 2", len(merged))
	}
}

func TestRateRequestForOrderUsesActualPackageAndPayment(t *testing.T) {
	order := &models.Order{Total: 200000, CODAmount: 50000, ShippingAddress: models.Address{Pincode: "121003"}, Items: []models.OrderItem{{Weight: 800, LengthCM: 20, WidthCM: 15, HeightCM: 10, Quantity: 2}, {Weight: 200, LengthCM: 8, WidthCM: 7, HeightCM: 3, Quantity: 1}}}
	request := rateRequestForOrder(order)
	if request.PaymentType != "COD" || request.CODAmountPaise != 50000 || request.OrderAmountPaise != 200000 {
		t.Fatalf("payment quote = %+v", request)
	}
	if request.WeightGrams != 1800 || request.LengthCM != 20 || request.WidthCM != 15 || request.HeightCM != 23 {
		t.Fatalf("package quote = %+v", request)
	}
}
