// Package shipping provides the courier provider interface and implementations.
//
// Shipmozo mapping is written against the REAL OpenAPI spec (Shipping API v1,
// fetched 2026-08-30 from https://shipping-api.com/api/v1?api-docs-v1.json).
// Base URL: https://shipping-api.com/app/api/v1
// Auth: public-key + private-key headers on every request.
// Create flow is TWO steps: POST /push-order (creates order, returns order id)
// then POST /auto-assign-order (books courier, yields AWB).
// NOTE: the spec documents request bodies but not 200-response bodies, so all
// response parsing is defensive (multiple candidate keys) and raw bodies are
// logged at DEBUG for first-call verification.
package shipping

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"ecom-core-service/internal/config"
	"ecom-core-service/internal/models"
	"ecom-core-service/pkg/logger"
)

var plog = logger.New("SHIPPING", "PROVIDER")

// CourierProvider abstracts courier operations behind a testable interface.
type CourierProvider interface {
	CheckServiceability(ctx context.Context, pincode string, codAmountPaise int) (models.TrackingEvent, bool, error)
	CreateShipment(ctx context.Context, req ShipmentRequest) (ShipmentResult, error)
	Track(ctx context.Context, awb string) ([]models.TrackingEvent, error)
}

// ShipmentRequest contains everything needed to push an order to a courier.
type ShipmentRequest struct {
	OrderNumber    string
	OrderDate      time.Time
	PaymentType    string // "PREPAID" or "COD"
	CODAmountPaise int    // COD amount in paise (converted to rupees at boundary)
	CustomerName   string
	CustomerPhone  string
	CustomerEmail  string
	Address        models.Address
	Items          []models.OrderItem
	WeightGrams    int
	LengthCM       int
	WidthCM        int
	HeightCM       int
}

// ShipmentResult is the response from creating a shipment.
type ShipmentResult struct {
	AWB         string
	CourierName string
	TrackingURL string
	LabelURL    string
}

// ==================== SHIPMOZO PROVIDER (Shipping API v1) ====================

type shipmozoProvider struct {
	baseURL       string
	publicKey     string
	privateKey    string
	pickupPincode string
	warehouseID   string
}

// NewShipmozoProvider creates a provider backed by the Shipping API v1.
func NewShipmozoProvider(cfg *config.Config) CourierProvider {
	return &shipmozoProvider{
		baseURL:       strings.TrimRight(cfg.ShipmozoBaseURL, "/"),
		publicKey:     cfg.ShipmozoPublicKey,
		privateKey:    cfg.ShipmozoPrivateKey,
		pickupPincode: cfg.WarehousePincode,
		warehouseID:   cfg.ShipmozoWarehouseID,
	}
}

func (p *shipmozoProvider) doRequest(ctx context.Context, method, path string, body interface{}) ([]byte, error) {
	var reqBody io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reqBody = bytes.NewBuffer(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, reqBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("public-key", p.publicKey)
	req.Header.Set("private-key", p.privateKey)

	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	// Raw-body debug log: response shapes are undocumented in the spec, so the
	// first real calls are our ground truth.
	plog.Debug("doRequest", "Shipmozo response", "path", path, "status", resp.StatusCode, "body", truncateStr(string(respBody), 500))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("shipmozo %s %s: status %d: %s", method, path, resp.StatusCode, truncateStr(string(respBody), 300))
	}
	return respBody, nil
}

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// apiEnvelope covers the common {result/status, message, data:{...}} wrappers
// Indian courier APIs use. All lookups fall back to top level if data is absent.
// Confirmed live 2026-08-30: Shipmozo returns {"result":"0|1","message":"...","data":...}
// with result as a STRING and HTTP 200 even on business failures.
func envelopeError(data []byte) error {
	var root struct {
		Result  interface{} `json:"result"`
		Message string      `json:"message"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		return nil // not an envelope; let callers parse what they can
	}
	switch v := root.Result.(type) {
	case string:
		if v == "0" {
			return fmt.Errorf("shipmozo rejected request: %s", root.Message)
		}
	case float64:
		if v == 0 {
			return fmt.Errorf("shipmozo rejected request: %s", root.Message)
		}
	}
	return nil
}

func extractString(data []byte, keys ...string) string {
	var root map[string]interface{}
	if err := json.Unmarshal(data, &root); err != nil {
		return ""
	}
	candidates := []map[string]interface{}{root}
	if inner, ok := root["data"].(map[string]interface{}); ok {
		candidates = append([]map[string]interface{}{inner}, candidates...)
	}
	for _, m := range candidates {
		for _, k := range keys {
			if v, ok := m[k]; ok {
				switch val := v.(type) {
				case string:
					if val != "" {
						return val
					}
				case float64:
					return strings.TrimSuffix(fmt.Sprintf("%.0f", val), ".0")
				}
			}
		}
	}
	return ""
}

// CheckServiceability — POST /rate-calculator: serviceable ⇔ at least one courier
// returns a rate for the lane.
//
// Deliberately NOT /pincode-serviceability: verified live 2026-08-30 that it returns
// {"serviceable":false} for lanes where /rate-calculator simultaneously returns real
// courier rates (e.g. Faridabad→Delhi, BlueDart ₹41). The rate calculator is the
// trustworthy signal and matches what booking will actually do.
func (p *shipmozoProvider) CheckServiceability(ctx context.Context, pincode string, codAmountPaise int) (models.TrackingEvent, bool, error) {
	paymentType := "PREPAID"
	orderAmount := "500"
	if codAmountPaise > 0 {
		paymentType = "COD"
		orderAmount = fmt.Sprintf("%d", (codAmountPaise+99)/100) // paise -> rupees, ceil
	}
	payload := map[string]interface{}{
		"pickup_pincode":   p.pickupPincode,
		"delivery_pincode": pincode,
		"payment_type":     paymentType,
		"order_amount":     orderAmount,
		"weight":           "500", // grams; representative default for a quote
		"dimensions": []map[string]interface{}{
			{"no_of_box": "1", "length": "10", "width": "10", "height": "10"},
		},
		"type_of_package": "SPS",
		"rate_type":       "FORWARD",
		"shipment_type":   "FORWARD",
	}
	data, err := p.doRequest(ctx, "POST", "/rate-calculator", payload)
	if err != nil {
		return models.TrackingEvent{}, false, err
	}
	if err := envelopeError(data); err != nil {
		return models.TrackingEvent{}, false, err
	}
	var root struct {
		Data []map[string]interface{} `json:"data"`
	}
	json.Unmarshal(data, &root)
	return models.TrackingEvent{}, len(root.Data) > 0, nil
}

// CreateShipment — two-step: POST /push-order then POST /auto-assign-order.
func (p *shipmozoProvider) CreateShipment(ctx context.Context, req ShipmentRequest) (ShipmentResult, error) {
	items := make([]map[string]interface{}, 0, len(req.Items))
	for _, it := range req.Items {
		items = append(items, map[string]interface{}{
			"name":       it.Name,
			"sku_number": it.SKU,
			"quantity":   it.Quantity,
			"unit_price": float64(it.Price) / 100.0, // paise -> rupees at boundary
			"discount":   0,
		})
	}

	pushPayload := map[string]interface{}{
		"order_id":                   req.OrderNumber,
		"order_date":                 req.OrderDate.Format("2006-01-02"),
		"consignee_name":             req.CustomerName,
		"consignee_phone":            req.CustomerPhone,
		"consignee_email":            req.CustomerEmail,
		"consignee_address_line_one": req.Address.Line1,
		"consignee_address_line_two": req.Address.Line2,
		"consignee_pin_code":         req.Address.Pincode,
		"consignee_city":             req.Address.City,
		"consignee_state":            req.Address.State,
		"product_detail":             items,
		"payment_type":               req.PaymentType,
		"cod_amount":                 float64(req.CODAmountPaise) / 100.0,
		"shipping_charges":           0,
		"weight":                     req.WeightGrams, // spec: weight in GRAM
		"length":                     req.LengthCM,
		"width":                      req.WidthCM,
		"height":                     req.HeightCM,
	}
	if p.warehouseID != "" {
		pushPayload["warehouse_id"] = p.warehouseID
	}

	pushResp, err := p.doRequest(ctx, "POST", "/push-order", pushPayload)
	if err != nil {
		return ShipmentResult{}, fmt.Errorf("push-order failed: %w", err)
	}
	if envErr := envelopeError(pushResp); envErr != nil {
		return ShipmentResult{}, fmt.Errorf("push-order: %w", envErr)
	}

	// The provider-side order id may differ from ours; fall back to ours.
	providerOrderID := extractString(pushResp, "order_id", "orderId", "id")
	if providerOrderID == "" {
		providerOrderID = req.OrderNumber
	}

	assignResp, err := p.doRequest(ctx, "POST", "/auto-assign-order", map[string]interface{}{
		"order_id": providerOrderID,
	})
	if err != nil {
		return ShipmentResult{}, fmt.Errorf("auto-assign-order failed (order pushed as %s): %w", providerOrderID, err)
	}
	if envErr := envelopeError(assignResp); envErr != nil {
		return ShipmentResult{}, fmt.Errorf("auto-assign-order (order pushed as %s): %w", providerOrderID, envErr)
	}

	awb := extractString(assignResp, "awb_number", "awb", "waybill")
	if awb == "" {
		// Some flows return AWB on the push response instead.
		awb = extractString(pushResp, "awb_number", "awb", "waybill")
	}
	if awb == "" {
		return ShipmentResult{}, fmt.Errorf("no AWB in shipmozo response (order pushed as %s) — check raw response in debug logs", providerOrderID)
	}

	result := ShipmentResult{
		AWB:         awb,
		CourierName: extractString(assignResp, "courier_name", "courier", "courier_partner"),
		TrackingURL: extractString(assignResp, "tracking_url", "track_url"),
		LabelURL:    extractString(assignResp, "label_url", "label"),
	}
	if result.LabelURL == "" {
		// Spec: GET /get-order-label/{awb_number} serves the label.
		result.LabelURL = p.baseURL + "/get-order-label/" + url.PathEscape(awb)
	}
	return result, nil
}

// Track — GET /track-order?awb_number=...
func (p *shipmozoProvider) Track(ctx context.Context, awb string) ([]models.TrackingEvent, error) {
	data, err := p.doRequest(ctx, "GET", "/track-order?awb_number="+url.QueryEscape(awb), nil)
	if err != nil {
		return nil, err
	}
	// Undocumented response shape: try the common containers.
	var root map[string]interface{}
	if err := json.Unmarshal(data, &root); err != nil {
		return []models.TrackingEvent{}, nil
	}
	rawEvents := findEventArray(root)
	events := make([]models.TrackingEvent, 0, len(rawEvents))
	for _, re := range rawEvents {
		em, ok := re.(map[string]interface{})
		if !ok {
			continue
		}
		ev := models.TrackingEvent{
			Status:      strAt(em, "status", "current_status", "scan_status"),
			Description: strAt(em, "description", "remark", "activity", "message"),
		}
		for _, tk := range []string{"timestamp", "date", "scan_date", "updated_at"} {
			if ts, ok := em[tk].(string); ok {
				for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
					if t, err := time.Parse(layout, ts); err == nil {
						ev.Timestamp = t
						break
					}
				}
				break
			}
		}
		events = append(events, ev)
	}
	return events, nil
}

func findEventArray(root map[string]interface{}) []interface{} {
	for _, k := range []string{"events", "scan_history", "tracking_data", "history", "scans"} {
		if arr, ok := root[k].([]interface{}); ok {
			return arr
		}
	}
	if inner, ok := root["data"].(map[string]interface{}); ok {
		return findEventArray(inner)
	}
	if arr, ok := root["data"].([]interface{}); ok {
		return arr
	}
	return nil
}

func strAt(m map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// ==================== MOCK PROVIDER ====================

type mockProvider struct{}

// NewMockProvider creates a deterministic mock courier provider for development.
func NewMockProvider() CourierProvider { return &mockProvider{} }

func (p *mockProvider) CheckServiceability(_ context.Context, pincode string, _ int) (models.TrackingEvent, bool, error) {
	if len(pincode) > 0 && pincode[0] >= '8' {
		return models.TrackingEvent{}, false, nil
	}
	return models.TrackingEvent{}, true, nil
}

func (p *mockProvider) CreateShipment(_ context.Context, req ShipmentRequest) (ShipmentResult, error) {
	awb := fmt.Sprintf("AWB%010d", time.Now().UnixNano()%10000000000)
	return ShipmentResult{
		AWB:         awb,
		CourierName: "Mock Express",
		TrackingURL: fmt.Sprintf("https://track.example.com/%s", awb),
		LabelURL:    fmt.Sprintf("https://labels.example.com/%s.pdf", awb),
	}, nil
}

func (p *mockProvider) Track(_ context.Context, awb string) ([]models.TrackingEvent, error) {
	return []models.TrackingEvent{
		{Status: "picked_up", Description: "Shipment picked up from warehouse", Timestamp: time.Now().Add(-48 * time.Hour)},
		{Status: "in_transit", Description: "In transit to destination hub", Timestamp: time.Now().Add(-24 * time.Hour)},
	}, nil
}

// ProviderFromConfig returns the appropriate courier provider based on config.
func ProviderFromConfig(cfg *config.Config) CourierProvider {
	if cfg.ShipmozoPublicKey != "" && cfg.ShipmozoPrivateKey != "" {
		return NewShipmozoProvider(cfg)
	}
	return NewMockProvider()
}
