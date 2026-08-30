package payment

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ==================== STRIPE API (stdlib net/http, form-encoded) ====================
// Stripe API uses Bearer auth + application/x-www-form-urlencoded bodies.
// No SDK dependency — all calls are plain HTTP.

const stripeBaseURL = "https://api.stripe.com/v1"

// stripeClient is a shared HTTP client with a 10s timeout for all Stripe calls.
var stripeClient = &http.Client{Timeout: 10 * time.Second}

// StripePaymentIntent represents the subset of Stripe's PaymentIntent object we use.
type StripePaymentIntent struct {
	ID           string            `json:"id"`
	Amount       int               `json:"amount"`
	Currency     string            `json:"currency"`
	Status       string            `json:"status"`
	ClientSecret string            `json:"client_secret"`
	Metadata     map[string]string `json:"metadata"`
}

// StripeRefund represents the subset of Stripe's Refund object we use.
type StripeRefund struct {
	ID     string `json:"id"`
	Amount int    `json:"amount"`
	Status string `json:"status"`
}

// StripeError represents an error response from the Stripe API.
type StripeError struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// stripeAPIError wraps a Stripe API error for logging/returning.
type stripeAPIError struct {
	StatusCode int
	Err        StripeError
}

func (e *stripeAPIError) Error() string {
	return fmt.Sprintf("stripe %d: [%s] %s", e.StatusCode, e.Err.Code, e.Err.Message)
}

// CreatePaymentIntent creates a Stripe PaymentIntent via POST /v1/payment_intents.
func (h *Handler) CreatePaymentIntent(amountPaise int, currency, orderID, paymentID, orderNumber string) (*StripePaymentIntent, error) {
	form := url.Values{}
	form.Set("amount", strconv.Itoa(amountPaise))
	form.Set("currency", strings.ToLower(currency))
	form.Set("automatic_payment_methods[enabled]", "true")
	form.Set("metadata[order_id]", orderID)
	form.Set("metadata[payment_id]", paymentID)
	form.Set("metadata[order_number]", orderNumber)

	req, err := http.NewRequest("POST", stripeBaseURL+"/payment_intents", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("stripe request build: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.cfg.StripeSecretKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := stripeClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("stripe API unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		var errResp struct {
			Error StripeError `json:"error"`
		}
		json.Unmarshal(body, &errResp)
		return nil, &stripeAPIError{StatusCode: resp.StatusCode, Err: errResp.Error}
	}

	var pi StripePaymentIntent
	if err := json.Unmarshal(body, &pi); err != nil {
		return nil, fmt.Errorf("stripe response decode: %w", err)
	}
	return &pi, nil
}

// RetrievePaymentIntent fetches a PaymentIntent by ID via GET /v1/payment_intents/:id.
func (h *Handler) RetrievePaymentIntent(paymentIntentID string) (*StripePaymentIntent, error) {
	req, err := http.NewRequest("GET", stripeBaseURL+"/payment_intents/"+paymentIntentID, nil)
	if err != nil {
		return nil, fmt.Errorf("stripe request build: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.cfg.StripeSecretKey)

	resp, err := stripeClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("stripe API unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		var errResp struct {
			Error StripeError `json:"error"`
		}
		json.Unmarshal(body, &errResp)
		return nil, &stripeAPIError{StatusCode: resp.StatusCode, Err: errResp.Error}
	}

	var pi StripePaymentIntent
	if err := json.Unmarshal(body, &pi); err != nil {
		return nil, fmt.Errorf("stripe response decode: %w", err)
	}
	return &pi, nil
}

// CreateStripeRefund creates a refund via POST /v1/refunds.
func (h *Handler) CreateStripeRefund(paymentIntentID string, amountPaise int) (*StripeRefund, error) {
	form := url.Values{}
	form.Set("payment_intent", paymentIntentID)
	form.Set("amount", strconv.Itoa(amountPaise))

	req, err := http.NewRequest("POST", stripeBaseURL+"/refunds", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("stripe request build: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.cfg.StripeSecretKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := stripeClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("stripe API unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		var errResp struct {
			Error StripeError `json:"error"`
		}
		json.Unmarshal(body, &errResp)
		return nil, &stripeAPIError{StatusCode: resp.StatusCode, Err: errResp.Error}
	}

	var refund StripeRefund
	if err := json.Unmarshal(body, &refund); err != nil {
		return nil, fmt.Errorf("stripe response decode: %w", err)
	}
	return &refund, nil
}

// ==================== STRIPE WEBHOOK SIGNATURE VERIFICATION ====================

// VerifyStripeWebhookSignature verifies Stripe's webhook signature.
// Header format: "t=<unix>,v1=<hex>,v1=<hex>..."
// Signed payload: "{t}.{rawBody}" HMAC-SHA256 with webhook secret.
// Returns the timestamp and nil error on success.
func VerifyStripeWebhookSignature(payload []byte, sigHeader, secret string) (int64, error) {
	if sigHeader == "" {
		return 0, fmt.Errorf("missing Stripe-Signature header")
	}
	if secret == "" {
		return 0, fmt.Errorf("webhook secret not configured")
	}

	parts := strings.Split(sigHeader, ",")
	var timestamp string
	var signatures []string

	for _, part := range parts {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			timestamp = kv[1]
		case "v1":
			signatures = append(signatures, kv[1])
		}
	}

	if timestamp == "" {
		return 0, fmt.Errorf("no timestamp in signature header")
	}
	if len(signatures) == 0 {
		return 0, fmt.Errorf("no v1 signature in header")
	}

	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid timestamp: %w", err)
	}

	// Replay protection: reject if older than 5 minutes
	age := time.Since(time.Unix(ts, 0))
	if age > 5*time.Minute || age < -5*time.Minute {
		return 0, fmt.Errorf("timestamp too old or in future: %v", age)
	}

	// Compute expected signature
	signedPayload := timestamp + "." + string(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signedPayload))
	expected := hex.EncodeToString(mac.Sum(nil))

	// Check if ANY v1 signature matches (constant-time)
	for _, sig := range signatures {
		if hmac.Equal([]byte(expected), []byte(sig)) {
			return ts, nil
		}
	}

	return 0, fmt.Errorf("signature mismatch")
}

// VerifyStripePaymentIntent validates a PaymentIntent meets our requirements for marking paid.
// Returns nil if valid, error describing the mismatch otherwise.
func VerifyStripePaymentIntent(pi *StripePaymentIntent, expectedAmount int, expectedOrderID string) error {
	if pi.Status != "succeeded" {
		return fmt.Errorf("payment_intent status is %q, expected \"succeeded\"", pi.Status)
	}
	if pi.Amount != expectedAmount {
		return fmt.Errorf("amount mismatch: stripe=%d, expected=%d", pi.Amount, expectedAmount)
	}
	if pi.Metadata["order_id"] != expectedOrderID {
		return fmt.Errorf("metadata.order_id mismatch: stripe=%q, expected=%q", pi.Metadata["order_id"], expectedOrderID)
	}
	return nil
}

// EncodeStripeForm encodes amount and metadata into form values (exposed for testing).
func EncodeStripeForm(amountPaise int, currency, orderID, paymentID, orderNumber string) url.Values {
	form := url.Values{}
	form.Set("amount", strconv.Itoa(amountPaise))
	form.Set("currency", strings.ToLower(currency))
	form.Set("automatic_payment_methods[enabled]", "true")
	form.Set("metadata[order_id]", orderID)
	form.Set("metadata[payment_id]", paymentID)
	form.Set("metadata[order_number]", orderNumber)
	return form
}
