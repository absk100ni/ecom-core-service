package shipping

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ecom-core-service/internal/config"
)

func TestShipmozoServiceabilitySendsAcceptedClientAndCODPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.UserAgent(), "Go-http-client") || r.UserAgent() == "" {
			t.Errorf("expected explicit application user agent, got %q", r.UserAgent())
		}
		if r.Header.Get("public-key") != "public" || r.Header.Get("private-key") != "private" {
			t.Error("expected Shipmozo authentication headers")
		}
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["payment_type"] != "COD" {
			t.Fatalf("payment_type = %v", payload["payment_type"])
		}
		if payload["cod_amount"] != "500.00" {
			t.Fatalf("cod_amount = %v, want 500.00", payload["cod_amount"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":"1","message":"Success","data":[{"courier_name":"Test Courier"}]}`))
	}))
	defer server.Close()

	provider := NewShipmozoProvider(&config.Config{
		ShipmozoBaseURL:    server.URL,
		ShipmozoPublicKey:  "public",
		ShipmozoPrivateKey: "private",
		WarehousePincode:   "110001",
	})
	quote, err := provider.CheckServiceability(context.Background(), RateRequest{DeliveryPincode: "121003", PaymentType: "COD", OrderAmountPaise: 50000, CODAmountPaise: 50000, WeightGrams: 500, LengthCM: 10, WidthCM: 10, HeightCM: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !quote.Serviceable {
		t.Fatal("expected a returned courier rate to make the lane serviceable")
	}
}
