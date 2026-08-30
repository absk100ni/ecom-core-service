package order

import (
	"testing"
)

// ComputeAdvance is a pure function extracted for testability.
func ComputeAdvance(totalPaise, effectivePercent int) (advance, cod int) {
	if effectivePercent <= 0 {
		return 0, totalPaise
	}
	if effectivePercent >= 100 {
		return totalPaise, 0
	}
	advance = (totalPaise*effectivePercent + 99) / 100 // ceil
	cod = totalPaise - advance
	return
}

func TestComputeAdvance(t *testing.T) {
	tests := []struct {
		name    string
		total   int
		percent int
		advance int
		cod     int
	}{
		{"20% of 10000", 10000, 20, 2000, 8000},
		{"20% of 9999 (ceiling)", 9999, 20, 2000, 7999},
		{"20% of 1 paise", 1, 20, 1, 0},
		{"0% means all COD", 10000, 0, 0, 10000},
		{"100% means full prepaid", 10000, 100, 10000, 0},
		{"50% of 101 (ceiling)", 101, 50, 51, 50},
		{"20% of 0 total", 0, 20, 0, 0},
		{"small amount 5 paise at 20%", 5, 20, 1, 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adv, cod := ComputeAdvance(tt.total, tt.percent)
			if adv != tt.advance {
				t.Errorf("advance: got %d, want %d", adv, tt.advance)
			}
			if cod != tt.cod {
				t.Errorf("cod: got %d, want %d", cod, tt.cod)
			}
			// Invariant: advance + cod == total
			if adv+cod != tt.total {
				t.Errorf("advance+cod=%d, want total=%d", adv+cod, tt.total)
			}
		})
	}
}

// MaxAdvancePercent picks the highest advance_percent from products.
func MaxAdvancePercent(percents []*int, globalDefault int) int {
	max := globalDefault
	for _, p := range percents {
		if p != nil && *p > max {
			max = *p
		}
	}
	return max
}

func TestMaxAdvancePercent(t *testing.T) {
	p30 := 30
	p50 := 50
	p0 := 0
	tests := []struct {
		name     string
		percents []*int
		global   int
		want     int
	}{
		{"nil uses global", []*int{nil, nil}, 20, 20},
		{"one product overrides", []*int{&p30, nil}, 20, 30},
		{"max wins among multiple", []*int{&p30, &p50}, 20, 50},
		{"zero product doesn't override", []*int{&p0}, 20, 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MaxAdvancePercent(tt.percents, tt.global)
			if got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestPaymentAmountSelection(t *testing.T) {
	tests := []struct {
		name          string
		total         int
		plan          string
		advanceAmount int
		wantCharge    int
	}{
		{"full plan charges total", 10000, "full", 10000, 10000},
		{"partial plan charges advance", 10000, "partial", 2000, 2000},
		{"partial plan zero advance charges total", 10000, "partial", 0, 10000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chargeAmount := tt.total
			if tt.plan == "partial" && tt.advanceAmount > 0 {
				chargeAmount = tt.advanceAmount
			}
			if chargeAmount != tt.wantCharge {
				t.Errorf("chargeAmount: got %d, want %d", chargeAmount, tt.wantCharge)
			}
		})
	}
}
