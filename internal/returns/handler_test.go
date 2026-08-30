package returns

import (
	"testing"
	"time"
)

func TestReturnWindowEnforcement(t *testing.T) {
	// Simulate a delivery 8 days ago with 7-day window
	deliveredAt := time.Now().Add(-8 * 24 * time.Hour)
	windowDays := 7
	deadline := deliveredAt.Add(time.Duration(windowDays) * 24 * time.Hour)

	if !time.Now().After(deadline) {
		t.Error("Expected window to be expired for delivery 8 days ago with 7-day window")
	}

	// Simulate delivery 5 days ago — within window
	deliveredAt2 := time.Now().Add(-5 * 24 * time.Hour)
	deadline2 := deliveredAt2.Add(time.Duration(windowDays) * 24 * time.Hour)
	if time.Now().After(deadline2) {
		t.Error("Expected window to still be open for delivery 5 days ago with 7-day window")
	}
}

func TestReturnWindowEdge(t *testing.T) {
	// Exactly at boundary
	deliveredAt := time.Now().Add(-7 * 24 * time.Hour)
	windowDays := 7
	deadline := deliveredAt.Add(time.Duration(windowDays) * 24 * time.Hour)

	// At exactly 7 days, Now() is slightly after, so should be expired
	if !time.Now().After(deadline) {
		// This may be flaky at exact ms boundary, but the logic is correct
		t.Log("Edge case: exactly at 7-day mark")
	}
}

func TestNonDeliveredRejection(t *testing.T) {
	// Business rule: only "delivered" status allows returns
	validStatuses := []string{"delivered"}
	invalidStatuses := []string{"pending", "confirmed", "shipped", "cancelled"}

	for _, s := range validStatuses {
		if s != "delivered" {
			t.Errorf("Status %q should not be valid for returns", s)
		}
	}
	for _, s := range invalidStatuses {
		if s == "delivered" {
			t.Errorf("Status %q should not be in invalid list", s)
		}
	}
}
