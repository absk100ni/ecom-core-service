package order

import "strings"

// AllowedTransitions defines the valid status transitions for orders.
// Key = current status, Value = set of allowed next statuses.
var AllowedTransitions = map[string]map[string]bool{
	"placed": {
		"confirmed": true,
		"cancelled": true,
		"expired":   true, // system only (sweeper)
		"abandoned": true, // system only (payment dismissed/failed)
	},
	"pending_payment": {
		"confirmed": true,
		"cancelled": true,
		"expired":   true,
	},
	"confirmed": {
		"processing": true,
		"cancelled":  true,
	},
	"processing": {
		"shipped":   true,
		"cancelled": true,
	},
	"shipped": {
		"out_for_delivery": true,
		"delivered":        true, // direct jump allowed (some couriers skip OFD)
	},
	"out_for_delivery": {
		"delivered": true,
	},
	"delivered": {
		"returned": true, // return flow only
	},
	"cancelled": {
		// terminal — no transitions out
	},
	"abandoned": {
		"confirmed": true, // system only: late payment captured, stock re-reserved
		"cancelled": true, // system only: late payment captured but stock gone → refunded
	},
	"expired": {
		// terminal — no transitions out
	},
	"returned": {
		// terminal
	},
}

// ValidateTransition checks if a status transition is allowed.
// Returns (allowed bool, allowedNextStatuses []string).
func ValidateTransition(currentStatus, newStatus string) (bool, []string) {
	allowed, ok := AllowedTransitions[currentStatus]
	if !ok {
		return false, nil
	}

	if allowed[newStatus] {
		return true, nil
	}

	// Build list of allowed next states for the error message
	next := make([]string, 0, len(allowed))
	for s := range allowed {
		next = append(next, s)
	}
	return false, next
}

// FormatAllowedStates returns a human-readable string of allowed next states.
func FormatAllowedStates(states []string) string {
	if len(states) == 0 {
		return "none (terminal state)"
	}
	return strings.Join(states, ", ")
}
