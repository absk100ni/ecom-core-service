package shipping

import (
	"context"
	"testing"
	"time"

	"ecom-core-service/internal/models"
)

func TestMockProviderCreateShipment(t *testing.T) {
	p := NewMockProvider()
	result, err := p.CreateShipment(context.Background(), ShipmentRequest{
		OrderNumber:    "ORD-12345",
		OrderDate:      time.Now(),
		PaymentType:    "PREPAID",
		CODAmountPaise: 0,
		CustomerName:   "Test User",
		CustomerPhone:  "9876543210",
		Address:        models.Address{Pincode: "110001"},
		Items:          []models.OrderItem{{Name: "Widget", Quantity: 1, Price: 5000}},
		WeightGrams:    500,
		LengthCM:       30,
		WidthCM:        25,
		HeightCM:       5,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.AWB == "" {
		t.Error("AWB should not be empty")
	}
	if result.CourierName != "Mock Express" {
		t.Errorf("CourierName: got %q, want %q", result.CourierName, "Mock Express")
	}
	if result.TrackingURL == "" {
		t.Error("TrackingURL should not be empty")
	}
	if result.LabelURL == "" {
		t.Error("LabelURL should not be empty")
	}
}

func TestMockProviderDeterministic(t *testing.T) {
	p := NewMockProvider()

	// Two calls should produce different AWBs (uniqueness via timestamp)
	r1, _ := p.CreateShipment(context.Background(), ShipmentRequest{OrderNumber: "ORD-1"})
	time.Sleep(time.Millisecond) // ensure different nanosecond
	r2, _ := p.CreateShipment(context.Background(), ShipmentRequest{OrderNumber: "ORD-2"})
	if r1.AWB == r2.AWB {
		t.Error("Mock provider should produce unique AWBs")
	}
}

func TestMockProviderServiceability(t *testing.T) {
	p := NewMockProvider()
	tests := []struct {
		pincode     string
		serviceable bool
	}{
		{"110001", true},  // Delhi — serviceable
		{"500001", true},  // Hyderabad — serviceable
		{"800001", false}, // starts with 8 — not serviceable
		{"900001", false}, // starts with 9 — not serviceable
	}
	for _, tt := range tests {
		_, ok, err := p.CheckServiceability(context.Background(), tt.pincode, 0)
		if err != nil {
			t.Fatalf("pincode %s: unexpected error: %v", tt.pincode, err)
		}
		if ok != tt.serviceable {
			t.Errorf("pincode %s: serviceable got %v, want %v", tt.pincode, ok, tt.serviceable)
		}
	}
}

func TestMockProviderTrack(t *testing.T) {
	p := NewMockProvider()
	events, err := p.Track(context.Background(), "AWB123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events: got %d, want 2", len(events))
	}
	if events[0].Status != "picked_up" {
		t.Errorf("first event status: got %q, want %q", events[0].Status, "picked_up")
	}
}
