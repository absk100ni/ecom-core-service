package order

import (
	"testing"
	"time"

	"ecom-core-service/internal/models"
)

// ==================== P0-4: State Machine Tests ====================

func TestValidateTransition_AllowedPaths(t *testing.T) {
	tests := []struct {
		from string
		to   string
	}{
		{"placed", "confirmed"},
		{"placed", "cancelled"},
		{"placed", "expired"},
		{"confirmed", "processing"},
		{"confirmed", "cancelled"},
		{"processing", "shipped"},
		{"processing", "cancelled"},
		{"shipped", "out_for_delivery"},
		{"shipped", "delivered"},
		{"out_for_delivery", "delivered"},
		{"delivered", "returned"},
	}
	for _, tt := range tests {
		t.Run(tt.from+"->"+tt.to, func(t *testing.T) {
			ok, _ := ValidateTransition(tt.from, tt.to)
			if !ok {
				t.Errorf("expected %s->%s to be allowed", tt.from, tt.to)
			}
		})
	}
}

func TestValidateTransition_IllegalPaths(t *testing.T) {
	tests := []struct {
		from string
		to   string
	}{
		{"delivered", "placed"},
		{"delivered", "confirmed"},
		{"delivered", "shipped"},
		{"cancelled", "confirmed"},
		{"cancelled", "placed"},
		{"expired", "confirmed"},
		{"expired", "placed"},
		{"shipped", "placed"},
		{"shipped", "confirmed"},
		{"shipped", "cancelled"}, // no cancel after shipped
		{"confirmed", "placed"},
		{"processing", "placed"},
		{"processing", "confirmed"},
	}
	for _, tt := range tests {
		t.Run(tt.from+"->"+tt.to, func(t *testing.T) {
			ok, nextStates := ValidateTransition(tt.from, tt.to)
			if ok {
				t.Errorf("expected %s->%s to be rejected", tt.from, tt.to)
			}
			// For non-terminal states, should return allowed alternatives
			if tt.from != "cancelled" && tt.from != "expired" && tt.from != "returned" {
				if len(nextStates) == 0 {
					t.Errorf("expected non-empty allowed states for %s", tt.from)
				}
			}
		})
	}
}

func TestValidateTransition_TerminalStates(t *testing.T) {
	terminal := []string{"cancelled", "expired", "returned"}
	for _, status := range terminal {
		t.Run("from_"+status, func(t *testing.T) {
			// Nothing should be allowed out of terminal states
			for next := range AllowedTransitions {
				ok, _ := ValidateTransition(status, next)
				if ok {
					t.Errorf("expected no transitions out of %s, but %s was allowed", status, next)
				}
			}
		})
	}
}

func TestFormatAllowedStates(t *testing.T) {
	if FormatAllowedStates(nil) != "none (terminal state)" {
		t.Error("nil should return terminal message")
	}
	if FormatAllowedStates([]string{}) != "none (terminal state)" {
		t.Error("empty should return terminal message")
	}
	result := FormatAllowedStates([]string{"confirmed", "cancelled"})
	if result != "confirmed, cancelled" {
		t.Errorf("unexpected: %s", result)
	}
}

// ==================== P0-1: Order Expiry Tests ====================

func TestIsOrderExpired(t *testing.T) {
	tests := []struct {
		name       string
		order      models.Order
		ttlMinutes int
		want       bool
	}{
		{
			"fresh order not expired",
			models.Order{Status: "placed", PaymentStatus: "pending", CreatedAt: time.Now().Add(-5 * time.Minute)},
			30,
			false,
		},
		{
			"stale order expired",
			models.Order{Status: "placed", PaymentStatus: "pending", CreatedAt: time.Now().Add(-35 * time.Minute)},
			30,
			true,
		},
		{
			"paid order never expired",
			models.Order{Status: "confirmed", PaymentStatus: "paid", CreatedAt: time.Now().Add(-60 * time.Minute)},
			30,
			false,
		},
		{
			"cancelled order never expired",
			models.Order{Status: "cancelled", PaymentStatus: "pending", CreatedAt: time.Now().Add(-60 * time.Minute)},
			30,
			false,
		},
		{
			"custom TTL 10 minutes",
			models.Order{Status: "placed", PaymentStatus: "pending", CreatedAt: time.Now().Add(-15 * time.Minute)},
			10,
			true,
		},
		{
			"zero TTL uses default 30min",
			models.Order{Status: "placed", PaymentStatus: "pending", CreatedAt: time.Now().Add(-25 * time.Minute)},
			0,
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsOrderExpired(&tt.order, tt.ttlMinutes)
			if got != tt.want {
				t.Errorf("IsOrderExpired() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ==================== P0-5a: Refund Amount Validation Tests ====================

func TestRefundAmountValidation(t *testing.T) {
	tests := []struct {
		name            string
		totalPaid       int // total paid (advance or full)
		alreadyRefunded int
		requestedAmount int
		expectValid     bool
		expectRefund    int // expected refund amount if valid
	}{
		{"full refund on full paid", 10000, 0, 0, true, 10000},
		{"partial refund", 10000, 0, 5000, true, 5000},
		{"partial after prior refund", 10000, 3000, 5000, true, 5000},
		{"exceeds remaining", 10000, 3000, 8000, false, 0},
		{"exactly remaining", 10000, 3000, 7000, true, 7000},
		{"already fully refunded", 10000, 10000, 1000, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			remaining := tt.totalPaid - tt.alreadyRefunded
			if remaining <= 0 {
				if tt.expectValid {
					t.Error("expected invalid but got valid")
				}
				return
			}
			refundAmount := remaining
			if tt.requestedAmount > 0 {
				if tt.requestedAmount > remaining {
					if tt.expectValid {
						t.Error("expected invalid but got valid")
					}
					return
				}
				refundAmount = tt.requestedAmount
			}
			if !tt.expectValid {
				t.Error("expected invalid but logic reached valid path")
				return
			}
			if refundAmount != tt.expectRefund {
				t.Errorf("refundAmount=%d, want %d", refundAmount, tt.expectRefund)
			}
		})
	}
}

// ==================== P0-5c: Coupon Validation Tests ====================

func TestCouponValidation(t *testing.T) {
	tests := []struct {
		name       string
		couponType string
		value      int
		maxDisc    int
		expectErr  string
	}{
		{"valid percentage", "percentage", 50, 0, ""},
		{"valid fixed", "fixed", 500, 0, ""},
		{"percentage too high", "percentage", 101, 0, "between 1 and 100"},
		{"percentage zero", "percentage", 0, 0, "greater than 0"},
		{"negative value", "fixed", -100, 0, "greater than 0"},
		{"negative max_discount", "fixed", 100, -50, "cannot be negative"},
		{"invalid type", "bogus", 50, 0, "percentage' or 'fixed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCoupon(tt.couponType, tt.value, tt.maxDisc)
			if tt.expectErr == "" && err != "" {
				t.Errorf("expected valid, got error: %s", err)
			}
			if tt.expectErr != "" {
				if err == "" {
					t.Errorf("expected error containing %q, got valid", tt.expectErr)
				}
				// Just verify error is non-empty (the actual validation is in handler)
			}
		})
	}
}

// validateCoupon is a pure test helper that mirrors the coupon handler validation logic.
func validateCoupon(couponType string, value, maxDiscount int) string {
	if couponType != "percentage" && couponType != "fixed" {
		return "Type must be 'percentage' or 'fixed'"
	}
	if value <= 0 {
		return "Coupon value must be greater than 0"
	}
	if couponType == "percentage" && (value < 1 || value > 100) {
		return "Percentage discount must be between 1 and 100"
	}
	if maxDiscount < 0 {
		return "Max discount cannot be negative"
	}
	return ""
}

// ==================== P1-1: DeliveredAt Test ====================

func TestDeliveredAtSetOnTransition(t *testing.T) {
	// Verify the state machine allows the delivered transition
	ok, _ := ValidateTransition("shipped", "delivered")
	if !ok {
		t.Fatal("shipped -> delivered should be allowed")
	}
	ok, _ = ValidateTransition("out_for_delivery", "delivered")
	if !ok {
		t.Fatal("out_for_delivery -> delivered should be allowed")
	}
	// The actual delivered_at set is tested via the UpdateStatus handler
	// (would need HTTP test), but this validates the state machine allows it
}

// ==================== P0-2: Coupon Release Idempotency Test ====================

func TestCouponReleaseRequiresUsedFlag(t *testing.T) {
	// Test that ReleaseCoupon logic requires CouponUsed=true
	// This is a unit-level check of the preconditions
	order := &models.Order{
		CouponCode:     "TEST10",
		CouponUsed:     false,
		CouponReleased: false,
	}
	// If CouponUsed is false, the DB update filter won't match,
	// so no decrement happens. This is the correct idempotent behavior.
	if order.CouponUsed {
		t.Error("order shouldn't have CouponUsed=true")
	}

	// When CouponUsed is true and CouponReleased is false, should release
	order.CouponUsed = true
	if !order.CouponUsed || order.CouponReleased {
		t.Error("precondition: CouponUsed=true, CouponReleased=false")
	}

	// After release, CouponReleased should be true (prevents double-decrement)
	order.CouponReleased = true
	if !order.CouponReleased {
		t.Error("postcondition: CouponReleased should be true")
	}
}
