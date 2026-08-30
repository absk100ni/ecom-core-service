package payment

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

func TestHmacSHA256(t *testing.T) {
	tests := []struct {
		data   string
		secret string
	}{
		{"order_123|pay_456", "my-secret-key"},
		{"test-data", "another-secret"},
		{"", "empty-data-test"},
	}
	for _, tt := range tests {
		result := hmacSHA256(tt.data, tt.secret)
		// Verify it's valid hex
		decoded, err := hex.DecodeString(result)
		if err != nil {
			t.Errorf("hmacSHA256(%q, %q) produced non-hex: %s", tt.data, tt.secret, result)
		}
		if len(decoded) != 32 { // SHA256 = 32 bytes
			t.Errorf("hmacSHA256 output length %d, want 32", len(decoded))
		}
		// Verify deterministic
		if hmacSHA256(tt.data, tt.secret) != result {
			t.Error("hmacSHA256 not deterministic")
		}
	}
}

func TestHmacSHA256Base64(t *testing.T) {
	tests := []struct {
		data   string
		secret string
	}{
		{"1234567890" + `{"type":"ORDER_PAID"}`, "cashfree-secret"},
		{"timestampbody", "key"},
	}
	for _, tt := range tests {
		result := hmacSHA256Base64(tt.data, tt.secret)
		// Verify it's valid base64
		decoded, err := base64.StdEncoding.DecodeString(result)
		if err != nil {
			t.Errorf("hmacSHA256Base64(%q, %q) produced non-base64: %s", tt.data, tt.secret, result)
		}
		if len(decoded) != 32 {
			t.Errorf("hmacSHA256Base64 output length %d, want 32", len(decoded))
		}
	}
}

func TestRazorpaySignatureVerification(t *testing.T) {
	secret := "test-razorpay-secret"
	orderID := "order_123"
	paymentID := "pay_456"
	data := orderID + "|" + paymentID

	// Generate valid signature
	validSig := hmacSHA256(data, secret)

	// Valid signature should match
	expected := hmacSHA256(data, secret)
	if expected != validSig {
		t.Error("Valid signature rejected")
	}

	// Tampered payload should NOT match
	tamperedSig := hmacSHA256("order_EVIL|pay_456", secret)
	if tamperedSig == validSig {
		t.Error("Tampered payload produced same signature")
	}

	// Wrong secret should NOT match
	wrongSecretSig := hmacSHA256(data, "wrong-secret")
	if wrongSecretSig == validSig {
		t.Error("Wrong secret produced same signature")
	}
}

func TestCashfreeSignatureVerification(t *testing.T) {
	secret := "cashfree-secret-key"
	timestamp := "1625000000"
	body := `{"type":"ORDER_PAID","data":{"order":{"order_id":"order_123"}}}`
	data := timestamp + body

	// Generate valid signature
	validSig := hmacSHA256Base64(data, secret)

	// Verify deterministic and matches manual computation
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(data))
	manualSig := base64.StdEncoding.EncodeToString(h.Sum(nil))
	if validSig != manualSig {
		t.Errorf("hmacSHA256Base64 output doesn't match manual: %s != %s", validSig, manualSig)
	}

	// Tampered body should NOT match
	tamperedSig := hmacSHA256Base64(timestamp+`{"type":"ORDER_PAID","data":{"order":{"order_id":"EVIL"}}}`, secret)
	if tamperedSig == validSig {
		t.Error("Tampered payload produced same signature")
	}
}
