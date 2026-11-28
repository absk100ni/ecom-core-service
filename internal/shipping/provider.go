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
	CheckServiceability(ctx context.Context, req RateRequest) (ServiceabilityQuote, error)
	PushOrder(ctx context.Context, req ShipmentRequest) (ShipmentResult, error)
	CreateShipment(ctx context.Context, req ShipmentRequest) (ShipmentResult, error)
	Track(ctx context.Context, awb string) (TrackingSnapshot, error)
}

type RateRequest struct {
	DeliveryPincode  string
	PaymentType      string
	OrderAmountPaise int
	CODAmountPaise   int
	WeightGrams      int
	LengthCM         int
	WidthCM          int
	HeightCM         int
}

type ServiceabilityQuote struct {
	Serviceable   bool
	CODAvailable  bool
	EstimatedDays int
	CourierID     string
	CourierName   string
	TotalCharges  float64
	AutoPickup    bool
}

type TrackingSnapshot struct {
	CurrentStatus        string
	StatusTime           time.Time
	ExpectedDeliveryDate string
	Courier              string
	Events               []models.TrackingEvent
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
	ProviderOrderID string
	AWB             string
	CourierName     string
	TrackingURL     string
	LabelURL        string
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
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; LucubraElec/1.0; +https://lucubraelec.in)")
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
func (p *shipmozoProvider) CheckServiceability(ctx context.Context, req RateRequest) (ServiceabilityQuote, error) {
	paymentType := req.PaymentType
	if paymentType == "" {
		paymentType = "PREPAID"
	}
	weight := req.WeightGrams
	if weight <= 0 {
		weight = 500
	}
	length, width, height := req.LengthCM, req.WidthCM, req.HeightCM
	if length <= 0 {
		length = 10
	}
	if width <= 0 {
		width = 10
	}
	if height <= 0 {
		height = 10
	}
	orderAmount := req.OrderAmountPaise
	if orderAmount <= 0 {
		orderAmount = 50000
	}
	payload := map[string]interface{}{
		"order_id": "", "pickup_pincode": p.pickupPincode, "delivery_pincode": req.DeliveryPincode,
		"payment_type": paymentType, "order_amount": fmt.Sprintf("%.2f", float64(orderAmount)/100),
		"weight": fmt.Sprintf("%d", weight), "dimensions": []map[string]interface{}{{"no_of_box": "1", "length": fmt.Sprintf("%d", length), "width": fmt.Sprintf("%d", width), "height": fmt.Sprintf("%d", height)}},
		"type_of_package": "SPS", "rov_type": "ROV_OWNER", "rate_type": "FORWARD", "shipment_type": "FORWARD",
	}
	if paymentType == "COD" {
		payload["cod_amount"] = fmt.Sprintf("%.2f", float64(req.CODAmountPaise)/100)
	} else {
		payload["cod_amount"] = ""
	}
	data, err := p.doRequest(ctx, "POST", "/rate-calculator", payload)
	if err != nil {
		return ServiceabilityQuote{}, err
	}
	if err := envelopeError(data); err != nil {
		return ServiceabilityQuote{}, err
	}
	var root struct {
		Data []struct {
			ID        interface{} `json:"id"`
			Name      string      `json:"name"`
			Estimated string      `json:"estimated_delivery"`
			Total     float64     `json:"total_charges"`
			Auto      string      `json:"pickups_automatically_scheduled"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		return ServiceabilityQuote{}, err
	}
	if len(root.Data) == 0 {
		return ServiceabilityQuote{Serviceable: false}, nil
	}
	best := root.Data[0]
	bestDays := atoiSafe(best.Estimated)
	for _, rate := range root.Data[1:] {
		days := atoiSafe(rate.Estimated)
		if days > 0 && (bestDays == 0 || days < bestDays || (days == bestDays && rate.Total < best.Total)) {
			best, bestDays = rate, days
		}
	}
	return ServiceabilityQuote{Serviceable: true, CODAvailable: paymentType == "COD", EstimatedDays: bestDays, CourierID: fmt.Sprint(best.ID), CourierName: best.Name, TotalCharges: best.Total, AutoPickup: strings.EqualFold(best.Auto, "YES")}, nil
}

// PushOrder creates an unassigned order in Shipmozo. It does not book a
// courier, create an AWB, charge shipping, or schedule pickup.
func (p *shipmozoProvider) PushOrder(ctx context.Context, req ShipmentRequest) (ShipmentResult, error) {
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

	codAmount := ""
	if req.PaymentType == "COD" && req.CODAmountPaise > 0 {
		codAmount = fmt.Sprintf("%.2f", float64(req.CODAmountPaise)/100)
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
		"cod_amount":                 codAmount,
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

	return ShipmentResult{ProviderOrderID: providerOrderID}, nil
}

// CreateShipment preserves the future automatic-assignment path, but the admin
// dashboard currently uses PushOrder only.
func (p *shipmozoProvider) CreateShipment(ctx context.Context, req ShipmentRequest) (ShipmentResult, error) {
	pushed, err := p.PushOrder(ctx, req)
	if err != nil {
		return ShipmentResult{}, err
	}
	providerOrderID := pushed.ProviderOrderID
	assignResp, err := p.doRequest(ctx, "POST", "/auto-assign-order", map[string]interface{}{"order_id": providerOrderID})
	if err != nil {
		return ShipmentResult{}, fmt.Errorf("auto-assign-order failed (order pushed as %s): %w", providerOrderID, err)
	}
	if envErr := envelopeError(assignResp); envErr != nil {
		return ShipmentResult{}, fmt.Errorf("auto-assign-order (order pushed as %s): %w", providerOrderID, envErr)
	}
	awb := extractString(assignResp, "awb_number", "awb", "waybill")
	if awb == "" {
		return ShipmentResult{}, fmt.Errorf("no AWB in shipmozo response (order pushed as %s)", providerOrderID)
	}
	result := ShipmentResult{ProviderOrderID: providerOrderID, AWB: awb, CourierName: extractString(assignResp, "courier_name", "courier", "courier_partner"), TrackingURL: extractString(assignResp, "tracking_url", "track_url"), LabelURL: extractString(assignResp, "label_url", "label")}
	if result.LabelURL == "" {
		result.LabelURL = p.baseURL + "/get-order-label/" + url.PathEscape(awb)
	}
	return result, nil
}

// Track — GET /track-order?awb_number=...
func (p *shipmozoProvider) Track(ctx context.Context, awb string) (TrackingSnapshot, error) {
	data, err := p.doRequest(ctx, "GET", "/track-order?awb_number="+url.QueryEscape(awb), nil)
	if err != nil {
		return TrackingSnapshot{}, err
	}
	if err := envelopeError(data); err != nil {
		return TrackingSnapshot{}, err
	}
	var root map[string]interface{}
	if err := json.Unmarshal(data, &root); err != nil {
		return TrackingSnapshot{}, err
	}
	container := root
	if inner, ok := root["data"].(map[string]interface{}); ok {
		container = inner
	}
	snapshot := TrackingSnapshot{
		CurrentStatus:        strAt(container, "current_status", "status"),
		ExpectedDeliveryDate: strAt(container, "expected_delivery_date", "estimated_delivery_date"),
		Courier:              strAt(container, "courier", "courier_name", "courier_company"),
	}
	for _, key := range []string{"status_time", "updated_at"} {
		if raw, ok := container[key].(string); ok {
			snapshot.StatusTime = parseProviderTime(raw)
			if !snapshot.StatusTime.IsZero() {
				break
			}
		}
	}
	rawEvents := findEventArray(root)
	for _, raw := range rawEvents {
		eventMap, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		event := models.TrackingEvent{Status: strAt(eventMap, "status", "current_status", "scan_status"), Description: strAt(eventMap, "description", "remark", "activity", "message")}
		for _, key := range []string{"timestamp", "date", "scan_date", "status_time", "updated_at"} {
			if value, ok := eventMap[key].(string); ok {
				event.Timestamp = parseProviderTime(value)
				if !event.Timestamp.IsZero() {
					break
				}
			}
		}
		snapshot.Events = append(snapshot.Events, event)
	}
	if len(snapshot.Events) == 0 && snapshot.CurrentStatus != "" {
		snapshot.Events = append(snapshot.Events, models.TrackingEvent{Status: snapshot.CurrentStatus, Description: snapshot.CurrentStatus, Timestamp: snapshot.StatusTime})
	}
	if snapshot.CurrentStatus == "" && len(snapshot.Events) > 0 {
		snapshot.CurrentStatus = snapshot.Events[len(snapshot.Events)-1].Status
		snapshot.StatusTime = snapshot.Events[len(snapshot.Events)-1].Timestamp
	}
	return snapshot, nil
}

func parseProviderTime(value string) time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func findEventArray(root map[string]interface{}) []interface{} {
	for _, k := range []string{"events", "scan_detail", "scan_history", "tracking_data", "history", "scans"} {
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

func (p *mockProvider) CheckServiceability(_ context.Context, req RateRequest) (ServiceabilityQuote, error) {
	if len(req.DeliveryPincode) > 0 && req.DeliveryPincode[0] >= '8' {
		return ServiceabilityQuote{Serviceable: false}, nil
	}
	return ServiceabilityQuote{Serviceable: true, CODAvailable: req.PaymentType == "COD", EstimatedDays: 3, CourierName: "Mock Express"}, nil
}

func (p *mockProvider) PushOrder(_ context.Context, req ShipmentRequest) (ShipmentResult, error) {
	return ShipmentResult{ProviderOrderID: "mock-" + req.OrderNumber}, nil
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

func (p *mockProvider) Track(_ context.Context, awb string) (TrackingSnapshot, error) {
	events := []models.TrackingEvent{
		{Status: "picked_up", Description: "Shipment picked up from warehouse", Timestamp: time.Now().Add(-48 * time.Hour)},
		{Status: "in_transit", Description: "In transit to destination hub", Timestamp: time.Now().Add(-24 * time.Hour)},
	}
	return TrackingSnapshot{CurrentStatus: "in_transit", StatusTime: events[1].Timestamp, Courier: "Mock Express", Events: events}, nil
}

// ProviderFromConfig returns the appropriate courier provider based on config.
func ProviderFromConfig(cfg *config.Config) CourierProvider {
	if cfg.ShipmozoPublicKey != "" && cfg.ShipmozoPrivateKey != "" {
		return NewShipmozoProvider(cfg)
	}
	return NewMockProvider()
}
