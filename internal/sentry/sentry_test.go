package sentry

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestParseDSN_Valid(t *testing.T) {
	dsn := "https://abc123key@o12345.ingest.sentry.io/6789012"
	parsed, err := ParseDSN(dsn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parsed.publicKey != "abc123key" {
		t.Errorf("publicKey = %q, want %q", parsed.publicKey, "abc123key")
	}
	if parsed.host != "o12345.ingest.sentry.io" {
		t.Errorf("host = %q, want %q", parsed.host, "o12345.ingest.sentry.io")
	}
	if parsed.scheme != "https" {
		t.Errorf("scheme = %q, want %q", parsed.scheme, "https")
	}
	if parsed.projectID != "6789012" {
		t.Errorf("projectID = %q, want %q", parsed.projectID, "6789012")
	}
}

func TestParseDSN_Invalid(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
	}{
		{"empty", ""},
		{"no key", "https://sentry.io/123"},
		{"no project", "https://key@sentry.io/"},
		{"garbage", "not-a-url-at-all"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseDSN(tc.dsn)
			if err == nil {
				t.Errorf("expected error for DSN %q, got nil", tc.dsn)
			}
		})
	}
}

func TestParseDSN_Empty_DisablesClient(t *testing.T) {
	c := New("", "production")
	if c.Enabled() {
		t.Error("client should be disabled with empty DSN")
	}
}

func TestNew_NonProduction_DisablesClient(t *testing.T) {
	c := New("https://key@sentry.io/123", "development")
	if c.Enabled() {
		t.Error("client should be disabled in non-production environment")
	}
}

func TestNew_ValidProduction_Enables(t *testing.T) {
	c := New("https://key@o1234.ingest.sentry.io/999", "production")
	if !c.Enabled() {
		t.Error("client should be enabled with valid DSN + production")
	}
}

func TestRateCap_DropsAfterMax(t *testing.T) {
	c := New("https://key@o1234.ingest.sentry.io/999", "production")
	// Disable actual HTTP sends by pointing endpoint to nothing valid
	c.endpoint = "http://127.0.0.1:1" // will fail to connect, but that's fine

	for i := 0; i < maxEventsPerHour; i++ {
		c.mu.Lock()
		now := time.Now()
		if now.Sub(c.windowAt) >= time.Hour {
			c.count = 0
			c.windowAt = now
		}
		c.count++
		c.mu.Unlock()
	}

	// 6th event should be rate-capped
	c.mu.Lock()
	atCap := c.count >= maxEventsPerHour
	c.mu.Unlock()

	if !atCap {
		t.Errorf("expected count >= %d after %d increments", maxEventsPerHour, maxEventsPerHour)
	}

	// Simulate calling CaptureError — it should not increment count
	c.CaptureError("should-be-dropped", map[string]string{"path": "/test"})

	// Allow goroutine to settle
	time.Sleep(50 * time.Millisecond)

	c.mu.Lock()
	finalCount := c.count
	c.mu.Unlock()

	if finalCount > maxEventsPerHour {
		t.Errorf("count should not exceed %d, got %d", maxEventsPerHour, finalCount)
	}
}

func TestDedupe_SameSignatureDropped(t *testing.T) {
	c := New("https://key@o1234.ingest.sentry.io/999", "production")
	c.endpoint = "http://127.0.0.1:1"

	// First call: should pass
	c.CaptureError("same error", map[string]string{"path": "/api/test"})
	time.Sleep(20 * time.Millisecond)

	c.mu.Lock()
	count1 := c.count
	c.mu.Unlock()

	// Second call with same signature: should be deduped
	c.CaptureError("same error", map[string]string{"path": "/api/test"})
	time.Sleep(20 * time.Millisecond)

	c.mu.Lock()
	count2 := c.count
	c.mu.Unlock()

	if count2 != count1 {
		t.Errorf("deduplicate failed: count went from %d to %d", count1, count2)
	}
}

func TestDedupe_DifferentSignatureAllowed(t *testing.T) {
	c := New("https://key@o1234.ingest.sentry.io/999", "production")
	c.endpoint = "http://127.0.0.1:1"

	c.CaptureError("error A", map[string]string{"path": "/path1"})
	time.Sleep(20 * time.Millisecond)

	c.mu.Lock()
	count1 := c.count
	c.mu.Unlock()

	c.CaptureError("error B", map[string]string{"path": "/path2"})
	time.Sleep(20 * time.Millisecond)

	c.mu.Lock()
	count2 := c.count
	c.mu.Unlock()

	if count2 <= count1 {
		t.Errorf("different signature should not be deduped: count1=%d count2=%d", count1, count2)
	}
}

func TestNoOp_Disabled_NoPanic(t *testing.T) {
	c := New("", "development")
	// Should not panic or make network calls
	c.CaptureError("test error", map[string]string{"path": "/"})
	c.CapturePanic("boom", map[string]string{"path": "/"})
}

func TestPayloadShape(t *testing.T) {
	var mu sync.Mutex
	var received []byte

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		received = body
		// Verify auth header
		auth := r.Header.Get("X-Sentry-Auth")
		if auth == "" {
			t.Error("missing X-Sentry-Auth header")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	c := &Client{
		enabled:    true,
		endpoint:   ts.URL,
		authHeader: "Sentry sentry_version=7, sentry_key=testkey, sentry_client=ecom-go/1.0",
		env:        "production",
		dedupeMap:  make(map[string]time.Time),
		windowAt:   time.Now(),
	}

	c.CaptureError("test 500 error", map[string]string{
		"request_id": "req-123",
		"path":       "/api/v1/orders",
		"method":     "POST",
		"status":     "500",
	})

	// Wait for async send
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	data := received
	mu.Unlock()

	if data == nil {
		t.Fatal("no event received by test server")
	}

	var evt map[string]interface{}
	if err := json.Unmarshal(data, &evt); err != nil {
		t.Fatalf("invalid JSON payload: %v", err)
	}

	// Check required fields
	requiredFields := []string{"event_id", "timestamp", "platform", "level", "environment", "server_name", "message"}
	for _, f := range requiredFields {
		if _, ok := evt[f]; !ok {
			t.Errorf("missing required field %q in payload", f)
		}
	}
	if evt["platform"] != "go" {
		t.Errorf("platform = %v, want 'go'", evt["platform"])
	}
	if evt["level"] != "error" {
		t.Errorf("level = %v, want 'error'", evt["level"])
	}
	if evt["environment"] != "production" {
		t.Errorf("environment = %v, want 'production'", evt["environment"])
	}
	if evt["server_name"] != "ecom-core-service" {
		t.Errorf("server_name = %v, want 'ecom-core-service'", evt["server_name"])
	}

	// Check tags
	tags, ok := evt["tags"].(map[string]interface{})
	if !ok {
		t.Fatal("tags field missing or not a map")
	}
	if tags["request_id"] != "req-123" {
		t.Errorf("tags.request_id = %v, want 'req-123'", tags["request_id"])
	}
	if tags["path"] != "/api/v1/orders" {
		t.Errorf("tags.path = %v, want '/api/v1/orders'", tags["path"])
	}
}
