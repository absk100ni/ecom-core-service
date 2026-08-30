package notify

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"ecom-core-service/internal/config"
)

func TestNormalizeIndianPhone(t *testing.T) {
	cases := []struct{ in, want string }{
		{"9876543210", "+919876543210"},
		{"09876543210", "+919876543210"},
		{"919876543210", "+919876543210"},
		{"+91 98765 43210", "+919876543210"},
		{"98765-43210", "+919876543210"},
		{"(987) 654-3210", "+919876543210"},
		{"6123456789", "+916123456789"}, // 6-series valid
		{"5123456789", ""},              // landline-range start, reject
		{"12345", ""},                   // too short
		{"919876543210123", ""},         // too long
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeIndianPhone(c.in); got != c.want {
			t.Errorf("normalizeIndianPhone(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSendWhatsAppTemplate_PayloadShape(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &gotBody)
		w.WriteHeader(200)
		w.Write([]byte(`{"messages":[{"id":"wamid.test"}]}`))
	}))
	defer srv.Close()

	oldBase := whatsappAPIBase
	whatsappAPIBase = srv.URL
	defer func() { whatsappAPIBase = oldBase }()

	n := New(&config.Config{
		WhatsAppAccessToken:   "test-token",
		WhatsAppPhoneNumberID: "12345",
		WhatsAppTemplateLang:  "en",
	})
	n.sendWhatsAppTemplate("9876543210", "order_confirmed", []string{"Ravi", "ORD-1", "299.70"})

	if gotPath != "/12345/messages" {
		t.Errorf("path = %q, want /12345/messages", gotPath)
	}
	if gotAuth != "Bearer test-token" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotBody["messaging_product"] != "whatsapp" || gotBody["to"] != "+919876543210" || gotBody["type"] != "template" {
		t.Errorf("body top-level wrong: %v", gotBody)
	}
	tpl := gotBody["template"].(map[string]any)
	if tpl["name"] != "order_confirmed" {
		t.Errorf("template name = %v", tpl["name"])
	}
	comps := tpl["components"].([]any)
	params := comps[0].(map[string]any)["parameters"].([]any)
	if len(params) != 3 {
		t.Fatalf("want 3 params, got %d", len(params))
	}
	if params[0].(map[string]any)["text"] != "Ravi" {
		t.Errorf("param[0] = %v", params[0])
	}
}

func TestSendWhatsAppTemplate_MockModeNoNetwork(t *testing.T) {
	// No token/phone-id: must be a pure no-op (no panic, no HTTP).
	// Point base at an unroutable address — if mock mode leaks a request,
	// the client would error loudly in logs, but more importantly a real
	// send path would need the httptest server above.
	oldBase := whatsappAPIBase
	whatsappAPIBase = "http://127.0.0.1:1"
	defer func() { whatsappAPIBase = oldBase }()

	n := New(&config.Config{})
	n.sendWhatsAppTemplate("9876543210", "order_confirmed", []string{"x"})
	// Also invalid phone with creds set: skip before network.
	n2 := New(&config.Config{WhatsAppAccessToken: "t", WhatsAppPhoneNumberID: "1"})
	n2.sendWhatsAppTemplate("bad-phone", "order_confirmed", []string{"x"})
}

func TestRupees(t *testing.T) {
	if got := rupees(29970); got != "299.70" {
		t.Errorf("rupees(29970) = %q", got)
	}
	if got := rupees(0); got != "0.00" {
		t.Errorf("rupees(0) = %q", got)
	}
}
