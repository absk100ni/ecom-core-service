package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"ecom-core-service/internal/models"

	"go.mongodb.org/mongo-driver/bson"
)

type providerPaymentEvidence struct {
	Provider        string
	PaymentID       string
	ProviderOrderID string
	Amount          int
	Currency        string
	Status          string
}

type paymentValidationError struct {
	Kind string
	Err  error
}

func (e *paymentValidationError) Error() string { return e.Err.Error() }

func validateProviderPayment(e providerPaymentEvidence, payment *models.Payment, capturedStatuses ...string) error {
	if payment == nil {
		return &paymentValidationError{Kind: "payment", Err: fmt.Errorf("payment record is missing")}
	}
	if e.ProviderOrderID != "" && e.ProviderOrderID != payment.RazorpayOrderID {
		return &paymentValidationError{Kind: "order", Err: fmt.Errorf("provider order id mismatch")}
	}
	if e.Amount != payment.Amount {
		return &paymentValidationError{Kind: "amount", Err: fmt.Errorf("provider amount %d does not match expected amount %d", e.Amount, payment.Amount)}
	}
	if !strings.EqualFold(strings.TrimSpace(e.Currency), strings.TrimSpace(payment.Currency)) {
		return &paymentValidationError{Kind: "currency", Err: fmt.Errorf("provider currency %q does not match expected currency %q", e.Currency, payment.Currency)}
	}
	statusOK := false
	for _, allowed := range capturedStatuses {
		if strings.EqualFold(strings.TrimSpace(e.Status), allowed) {
			statusOK = true
			break
		}
	}
	if !statusOK {
		return &paymentValidationError{Kind: "status", Err: fmt.Errorf("provider payment status %q is not captured", e.Status)}
	}
	return nil
}

func isMonetaryValidationMismatch(err error) bool {
	validationErr, ok := err.(*paymentValidationError)
	return ok && (validationErr.Kind == "amount" || validationErr.Kind == "currency" || validationErr.Kind == "order")
}

func (h *Handler) recordPaymentValidationMismatch(payment *models.Payment, evidence providerPaymentEvidence, validationErr error) {
	if payment == nil || payment.OrderID == "" || !isMonetaryValidationMismatch(validationErr) {
		return
	}
	now := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = h.db.Collection("orders").UpdateOne(ctx, bson.M{"_id": payment.OrderID}, bson.M{"$set": bson.M{
		"payment_validation_mismatch":          true,
		"payment_validation_error":             validationErr.Error(),
		"payment_validation_provider":          evidence.Provider,
		"payment_validation_observed_amount":   evidence.Amount,
		"payment_validation_observed_currency": evidence.Currency,
		"payment_validation_observed_status":   evidence.Status,
		"updated_at":                           now,
	}})
}

func (h *Handler) fetchRazorpayPayment(paymentID string) (providerPaymentEvidence, error) {
	req, err := http.NewRequest(http.MethodGet, "https://api.razorpay.com/v1/payments/"+paymentID, nil)
	if err != nil {
		return providerPaymentEvidence{}, err
	}
	req.SetBasicAuth(h.cfg.RazorpayKeyID, h.cfg.RazorpaySecret)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return providerPaymentEvidence{}, fmt.Errorf("razorpay payment lookup: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return providerPaymentEvidence{}, fmt.Errorf("razorpay payment lookup status %d: %s", resp.StatusCode, string(body))
	}
	var result struct {
		ID       string `json:"id"`
		OrderID  string `json:"order_id"`
		Amount   int    `json:"amount"`
		Currency string `json:"currency"`
		Status   string `json:"status"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return providerPaymentEvidence{}, fmt.Errorf("razorpay payment decode: %w", err)
	}
	return providerPaymentEvidence{Provider: "razorpay", PaymentID: result.ID, ProviderOrderID: result.OrderID, Amount: result.Amount, Currency: result.Currency, Status: result.Status}, nil
}

type cashfreeWebhookEvidence struct {
	OrderID   string
	PaymentID string
	Amount    int
	Currency  string
	Status    string
}

func decodeCashfreeWebhookEvidence(body []byte) (string, cashfreeWebhookEvidence, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload struct {
		Type string `json:"type"`
		Data struct {
			Order struct {
				OrderID       string      `json:"order_id"`
				OrderAmount   json.Number `json:"order_amount"`
				OrderCurrency string      `json:"order_currency"`
				OrderStatus   string      `json:"order_status"`
			} `json:"order"`
			Payment struct {
				PaymentID       string      `json:"cf_payment_id"`
				PaymentStatus   string      `json:"payment_status"`
				PaymentAmount   json.Number `json:"payment_amount"`
				PaymentCurrency string      `json:"payment_currency"`
			} `json:"payment"`
		} `json:"data"`
	}
	if err := decoder.Decode(&payload); err != nil {
		return "", cashfreeWebhookEvidence{}, err
	}
	amountValue := payload.Data.Payment.PaymentAmount
	currency := payload.Data.Payment.PaymentCurrency
	status := payload.Data.Payment.PaymentStatus
	if amountValue.String() == "" {
		amountValue = payload.Data.Order.OrderAmount
	}
	if currency == "" {
		currency = payload.Data.Order.OrderCurrency
	}
	if status == "" {
		status = payload.Data.Order.OrderStatus
	}
	amountRupees, err := amountValue.Float64()
	if err != nil {
		return payload.Type, cashfreeWebhookEvidence{}, fmt.Errorf("invalid cashfree webhook amount: %w", err)
	}
	return payload.Type, cashfreeWebhookEvidence{
		OrderID: payload.Data.Order.OrderID, PaymentID: payload.Data.Payment.PaymentID,
		Amount: int(math.Round(amountRupees * 100)), Currency: currency,
		Status: status,
	}, nil
}

type cashfreeOrderEvidence struct {
	ProviderOrderID string
	Amount          int
	Currency        string
	Status          string
}

func decodeCashfreeOrderEvidence(body []byte) (cashfreeOrderEvidence, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var result struct {
		OrderID       string      `json:"order_id"`
		OrderAmount   json.Number `json:"order_amount"`
		OrderCurrency string      `json:"order_currency"`
		OrderStatus   string      `json:"order_status"`
	}
	if err := decoder.Decode(&result); err != nil {
		return cashfreeOrderEvidence{}, err
	}
	amountRupees, err := result.OrderAmount.Float64()
	if err != nil {
		return cashfreeOrderEvidence{}, fmt.Errorf("invalid cashfree order amount: %w", err)
	}
	return cashfreeOrderEvidence{
		ProviderOrderID: result.OrderID,
		Amount:          int(math.Round(amountRupees * 100)),
		Currency:        result.OrderCurrency,
		Status:          result.OrderStatus,
	}, nil
}
