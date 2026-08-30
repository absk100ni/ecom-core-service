package shipping

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ecom-core-service/internal/config"
	"ecom-core-service/internal/models"
)

// Conformance tests against the documented Shipping API v1 spec
// (request shapes per https://shipping-api.com/api/v1?api-docs-v1.json).

func testProvider(baseURL string) CourierProvider {
	return NewShipmozoProvider(&config.Config{
		ShipmozoBaseURL:     baseURL,
		ShipmozoPublicKey:   "pub-test",
		ShipmozoPrivateKey:  "priv-test",
		ShipmozoWarehouseID: "wh-1",
		WarehousePincode:    "400001",
	})
}

func TestShipmozoCreateShipment_TwoStepFlow(t *testing.T) {
	var pushBody, assignBody map[string]interface{}
	var pushAuth [2]string
	calls := []string{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		raw, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/push-order":
			pushAuth[0] = r.Header.Get("public-key")
			pushAuth[1] = r.Header.Get("private-key")
			json.Unmarshal(raw, &pushBody)
			w.Write([]byte(`{"result":1,"message":"ok","data":{"order_id":"SMZ-777"}}`))
		case "/auto-assign-order":
			json.Unmarshal(raw, &assignBody)
			w.Write([]byte(`{"result":1,"data":{"awb_number":"AWB123456","courier_name":"Delhivery","tracking_url":"https://t.example/AWB123456"}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	p := testProvider(srv.URL)
	res, err := p.CreateShipment(context.Background(), ShipmentRequest{
		OrderNumber:    "ORD-1",
		OrderDate:      time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC),
		PaymentType:    "COD",
		CODAmountPaise: 119920, // ₹1199.20
		CustomerName:   "Ravi",
		CustomerPhone:  "9876543210",
		Address:        models.Address{Line1: "1 Test Rd", City: "Mumbai", State: "MH", Pincode: "400002"},
		Items:          []models.OrderItem{{Name: "Bulb", SKU: "SKU1", Price: 14900, Quantity: 2}},
		WeightGrams:    500,
		LengthCM:       30, WidthCM: 25, HeightCM: 5,
	})
	if err != nil {
		t.Fatalf("CreateShipment: %v", err)
	}
	// Two-step order matters: push first, then assign.
	if len(calls) != 2 || calls[0] != "/push-order" || calls[1] != "/auto-assign-order" {
		t.Fatalf("call sequence = %v", calls)
	}
	if pushAuth[0] != "pub-test" || pushAuth[1] != "priv-test" {
		t.Errorf("auth headers = %v", pushAuth)
	}
	// Spec field names.
	if pushBody["consignee_name"] != "Ravi" || pushBody["consignee_pin_code"] != "400002" {
		t.Errorf("consignee fields wrong: %v", pushBody)
	}
	if pushBody["cod_amount"].(float64) != 1199.20 {
		t.Errorf("cod_amount = %v, want rupees 1199.20", pushBody["cod_amount"])
	}
	if pushBody["weight"].(float64) != 500 {
		t.Errorf("weight = %v, want 500 grams", pushBody["weight"])
	}
	items := pushBody["product_detail"].([]interface{})
	item0 := items[0].(map[string]interface{})
	if item0["sku_number"] != "SKU1" || item0["unit_price"].(float64) != 149.00 {
		t.Errorf("product_detail[0] = %v", item0)
	}
	// Assign must reference the provider-side id from push response.
	if assignBody["order_id"] != "SMZ-777" {
		t.Errorf("assign order_id = %v, want SMZ-777", assignBody["order_id"])
	}
	if res.AWB != "AWB123456" || res.CourierName != "Delhivery" {
		t.Errorf("result = %+v", res)
	}
}

func TestShipmozoCreateShipment_NoAWBFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"result":1,"message":"ok"}`)) // no awb anywhere
	}))
	defer srv.Close()
	_, err := testProvider(srv.URL).CreateShipment(context.Background(), ShipmentRequest{OrderNumber: "ORD-2", OrderDate: time.Now()})
	if err == nil {
		t.Fatal("expected error when no AWB returned")
	}
}

func TestShipmozoServiceability_RequestShape(t *testing.T) {
	var body map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rate-calculator" || r.Method != "POST" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		w.Write([]byte(`{"result":"1","message":"Success","data":[{"name":"BlueDart Air","total_charges":55.46}]}`))
	}))
	defer srv.Close()
	_, ok, err := testProvider(srv.URL).CheckServiceability(context.Background(), "110001", 0)
	if err != nil || !ok {
		t.Fatalf("serviceability: ok=%v err=%v", ok, err)
	}
	if body["pickup_pincode"] != "400001" || body["delivery_pincode"] != "110001" {
		t.Errorf("body = %v", body)
	}
	if body["payment_type"] != "PREPAID" {
		t.Errorf("payment_type = %v", body["payment_type"])
	}
	if _, hasDims := body["dimensions"]; !hasDims {
		t.Error("dimensions missing -- API requires it")
	}
}

func TestShipmozoServiceability_NoCouriersMeansNotServiceable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"result":"1","message":"Success","data":[]}`))
	}))
	defer srv.Close()
	_, ok, err := testProvider(srv.URL).CheckServiceability(context.Background(), "999999", 0)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ok {
		t.Error("empty courier list must mean not serviceable")
	}
}

func TestShipmozoTrack_QueryParam(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/track-order" || r.URL.Query().Get("awb_number") != "AWB9" {
			t.Errorf("unexpected %s %s", r.URL.Path, r.URL.RawQuery)
		}
		w.Write([]byte(`{"data":{"scan_history":[{"status":"in_transit","remark":"Left origin hub","date":"2026-08-30 10:00:00"}]}}`))
	}))
	defer srv.Close()
	events, err := testProvider(srv.URL).Track(context.Background(), "AWB9")
	if err != nil {
		t.Fatalf("track: %v", err)
	}
	if len(events) != 1 || events[0].Status != "in_transit" || events[0].Description != "Left origin hub" {
		t.Errorf("events = %+v", events)
	}
}

func TestShipmozoEnvelopeRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Real shape observed live 2026-08-30: HTTP 200 + result "0" on business failure.
		w.Write([]byte(`{"result":"0","message":"Your profile is under verification","data":[]}`))
	}))
	defer srv.Close()
	_, err := testProvider(srv.URL).CreateShipment(context.Background(), ShipmentRequest{OrderNumber: "ORD-3", OrderDate: time.Now()})
	if err == nil || !strings.Contains(err.Error(), "under verification") {
		t.Fatalf("expected envelope rejection surfaced, got: %v", err)
	}
}
