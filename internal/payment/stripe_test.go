package payment

import (
	"fmt"
	"net/url"
	"strconv"
	"testing"
	"time"
)

func TestVerifyStripeWebhookSignature_ValidSig(t *testing.T) {
	secret := "whsec_test_secret_key"
	payload := []byte(`{"type":"payment_intent.succeeded","data":{"object":{"id":"pi_123"}}}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	// Compute valid signature
	sig := computeStripeSignature(ts, payload, secret)
	header := fmt.Sprintf("t=%s,v1=%s", ts, sig)

	gotTS, err := VerifyStripeWebhookSignature(payload, header, secret)
	if err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if gotTS == 0 {
		t.Error("timestamp should be non-zero")
	}
}

func TestVerifyStripeWebhookSignature_TamperedBody(t *testing.T) {
	secret := "whsec_test_secret_key"
	payload := []byte(`{"type":"payment_intent.succeeded"}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	sig := computeStripeSignature(ts, payload, secret)
	header := fmt.Sprintf("t=%s,v1=%s", ts, sig)

	// Tamper the body
	tampered := []byte(`{"type":"payment_intent.succeeded","EVIL":true}`)
	_, err := VerifyStripeWebhookSignature(tampered, header, secret)
	if err == nil {
		t.Fatal("tampered body should be rejected")
	}
}

func TestVerifyStripeWebhookSignature_ExpiredTimestamp(t *testing.T) {
	secret := "whsec_test_secret_key"
	payload := []byte(`{"type":"payment_intent.succeeded"}`)
	// Timestamp 10 minutes ago
	ts := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)

	sig := computeStripeSignature(ts, payload, secret)
	header := fmt.Sprintf("t=%s,v1=%s", ts, sig)

	_, err := VerifyStripeWebhookSignature(payload, header, secret)
	if err == nil {
		t.Fatal("expired timestamp should be rejected")
	}
}

func TestVerifyStripeWebhookSignature_MultipleV1OneValid(t *testing.T) {
	secret := "whsec_test_secret_key"
	payload := []byte(`{"type":"payment_intent.succeeded"}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	validSig := computeStripeSignature(ts, payload, secret)
	invalidSig := "deadbeef0123456789abcdef0123456789abcdef0123456789abcdef01234567"
	header := fmt.Sprintf("t=%s,v1=%s,v1=%s", ts, invalidSig, validSig)

	_, err := VerifyStripeWebhookSignature(payload, header, secret)
	if err != nil {
		t.Fatalf("should pass when at least one v1 is valid: %v", err)
	}
}

func TestVerifyStripeWebhookSignature_MissingHeader(t *testing.T) {
	_, err := VerifyStripeWebhookSignature([]byte("body"), "", "secret")
	if err == nil {
		t.Fatal("missing header should fail")
	}
}

func TestVerifyStripeWebhookSignature_MissingSecret(t *testing.T) {
	_, err := VerifyStripeWebhookSignature([]byte("body"), "t=123,v1=abc", "")
	if err == nil {
		t.Fatal("missing secret should fail")
	}
}

func TestVerifyStripePaymentIntent_Success(t *testing.T) {
	pi := &StripePaymentIntent{
		ID:       "pi_test123",
		Amount:   50000,
		Currency: "inr",
		Status:   "succeeded",
		Metadata: map[string]string{"order_id": "order-abc", "payment_id": "pay-123"},
	}
	if err := VerifyStripePaymentIntent(pi, 50000, "order-abc"); err != nil {
		t.Fatalf("valid PI rejected: %v", err)
	}
}

func TestVerifyStripePaymentIntent_WrongStatus(t *testing.T) {
	pi := &StripePaymentIntent{
		ID: "pi_test", Amount: 50000, Currency: "inr", Status: "requires_payment_method",
		Metadata: map[string]string{"order_id": "order-abc"},
	}
	if err := VerifyStripePaymentIntent(pi, 50000, "order-abc"); err == nil {
		t.Fatal("wrong status should be rejected")
	}
}

func TestVerifyStripePaymentIntent_AmountMismatch(t *testing.T) {
	pi := &StripePaymentIntent{
		ID: "pi_test", Amount: 99999, Currency: "inr", Status: "succeeded",
		Metadata: map[string]string{"order_id": "order-abc"},
	}
	if err := VerifyStripePaymentIntent(pi, 50000, "order-abc"); err == nil {
		t.Fatal("amount mismatch should be rejected")
	}
}

func TestVerifyStripePaymentIntent_MetadataMismatch(t *testing.T) {
	pi := &StripePaymentIntent{
		ID: "pi_test", Amount: 50000, Currency: "inr", Status: "succeeded",
		Metadata: map[string]string{"order_id": "order-EVIL"},
	}
	if err := VerifyStripePaymentIntent(pi, 50000, "order-abc"); err == nil {
		t.Fatal("metadata mismatch should be rejected")
	}
}

func TestEncodeStripeForm(t *testing.T) {
	form := EncodeStripeForm(150000, "INR", "order-123", "pay-456", "ORD-99001")

	tests := map[string]string{
		"amount":                             "150000",
		"currency":                           "inr",
		"automatic_payment_methods[enabled]": "true",
		"metadata[order_id]":                 "order-123",
		"metadata[payment_id]":               "pay-456",
		"metadata[order_number]":             "ORD-99001",
	}

	for key, want := range tests {
		got := form.Get(key)
		if got != want {
			t.Errorf("form[%q] = %q, want %q", key, got, want)
		}
	}

	// Verify it encodes properly
	encoded := form.Encode()
	parsed, err := url.ParseQuery(encoded)
	if err != nil {
		t.Fatalf("form.Encode() produced invalid query string: %v", err)
	}
	if parsed.Get("amount") != "150000" {
		t.Error("round-trip encode/decode failed for amount")
	}
}

// computeStripeSignature is a test helper that generates a valid Stripe signature.
func computeStripeSignature(timestamp string, payload []byte, secret string) string {
	signedPayload := timestamp + "." + string(payload)
	return hmacSHA256(signedPayload, secret)
}
