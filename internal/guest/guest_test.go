package guest

import (
	"testing"
)

// ==================== Guest Token Tests ====================

func TestGenerateGuestToken_Deterministic(t *testing.T) {
	secret := "test-secret-key"
	orderID := "order-abc-123"
	phone := "9876543210"

	t1 := GenerateGuestToken(secret, orderID, phone)
	t2 := GenerateGuestToken(secret, orderID, phone)
	if t1 != t2 {
		t.Errorf("tokens should be deterministic: %s != %s", t1, t2)
	}
	if len(t1) != 64 { // SHA-256 hex is 64 chars
		t.Errorf("expected 64-char hex token, got %d chars", len(t1))
	}
}

func TestVerifyGuestToken_Valid(t *testing.T) {
	secret := "test-secret-key"
	orderID := "order-abc-123"
	phone := "9876543210"

	token := GenerateGuestToken(secret, orderID, phone)
	if !VerifyGuestToken(secret, orderID, phone, token) {
		t.Error("valid token should verify")
	}
}

func TestVerifyGuestToken_TamperedToken(t *testing.T) {
	secret := "test-secret-key"
	orderID := "order-abc-123"
	phone := "9876543210"

	token := GenerateGuestToken(secret, orderID, phone)
	// Tamper with the token
	tampered := "a" + token[1:]
	if VerifyGuestToken(secret, orderID, phone, tampered) {
		t.Error("tampered token should NOT verify")
	}
}

func TestVerifyGuestToken_WrongPhone(t *testing.T) {
	secret := "test-secret-key"
	orderID := "order-abc-123"

	token := GenerateGuestToken(secret, orderID, "9876543210")
	if VerifyGuestToken(secret, orderID, "0000000000", token) {
		t.Error("wrong phone should NOT verify")
	}
}

func TestVerifyGuestToken_WrongOrder(t *testing.T) {
	secret := "test-secret-key"
	phone := "9876543210"

	token := GenerateGuestToken(secret, "order-1", phone)
	if VerifyGuestToken(secret, "order-2", phone, token) {
		t.Error("wrong order ID should NOT verify")
	}
}

func TestVerifyGuestToken_WrongSecret(t *testing.T) {
	orderID := "order-abc-123"
	phone := "9876543210"

	token := GenerateGuestToken("secret-A", orderID, phone)
	if VerifyGuestToken("secret-B", orderID, phone, token) {
		t.Error("wrong secret should NOT verify")
	}
}

func TestVerifyGuestToken_EmptyInputs(t *testing.T) {
	// Empty inputs should still produce valid HMAC without panicking
	token := GenerateGuestToken("", "", "")
	if !VerifyGuestToken("", "", "", token) {
		t.Error("empty inputs with matching secret should verify")
	}
}

// ==================== Validation Tests ====================

func TestPhoneRegex(t *testing.T) {
	tests := []struct {
		phone string
		valid bool
	}{
		{"9876543210", true},
		{"0000000000", true},
		{"123456789", false},   // 9 digits
		{"12345678901", false}, // 11 digits
		{"98765abcde", false},  // letters
		{"", false},
		{"+919876543210", false}, // with country code
	}
	for _, tt := range tests {
		t.Run(tt.phone, func(t *testing.T) {
			got := phoneRegex.MatchString(tt.phone)
			if got != tt.valid {
				t.Errorf("phoneRegex(%q) = %v, want %v", tt.phone, got, tt.valid)
			}
		})
	}
}

func TestPincodeRegex(t *testing.T) {
	tests := []struct {
		pincode string
		valid   bool
	}{
		{"110001", true},
		{"560034", true},
		{"12345", false},   // 5 digits
		{"1234567", false}, // 7 digits
		{"abcdef", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.pincode, func(t *testing.T) {
			got := pincodeRegex.MatchString(tt.pincode)
			if got != tt.valid {
				t.Errorf("pincodeRegex(%q) = %v, want %v", tt.pincode, got, tt.valid)
			}
		})
	}
}
