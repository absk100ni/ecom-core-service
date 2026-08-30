// Package sentry provides a minimal Sentry error reporter using the Store API.
// No SDK — stdlib net/http only. Total no-op when DSN is unset or environment != production.
package sentry

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"ecom-core-service/pkg/logger"
)

var log = logger.New("SENTRY", "CLIENT")

// Client sends error events to Sentry via the Store API.
type Client struct {
	enabled    bool
	endpoint   string // POST URL: {scheme}://{host}/api/{projectID}/store/
	authHeader string // X-Sentry-Auth value
	env        string // production, development, etc.

	mu       sync.Mutex
	count    int       // events in current window
	windowAt time.Time // start of current rate-limit window

	dedupeMap map[string]time.Time // hash -> last sent time
}

// parsedDSN holds parts extracted from SENTRY_DSN.
type parsedDSN struct {
	publicKey string
	host      string
	scheme    string
	projectID string
}

// ParseDSN extracts scheme, public key, host, and project ID from a Sentry DSN.
// Format: https://PUBLIC_KEY@HOST/PROJECT_ID
func ParseDSN(dsn string) (*parsedDSN, error) {
	if dsn == "" {
		return nil, fmt.Errorf("empty DSN")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, fmt.Errorf("invalid DSN URL: %w", err)
	}
	if u.User == nil || u.User.Username() == "" {
		return nil, fmt.Errorf("DSN missing public key")
	}
	projectID := strings.TrimPrefix(u.Path, "/")
	if projectID == "" {
		return nil, fmt.Errorf("DSN missing project ID")
	}
	return &parsedDSN{
		publicKey: u.User.Username(),
		host:      u.Host,
		scheme:    u.Scheme,
		projectID: projectID,
	}, nil
}

const (
	maxEventsPerHour = 5
	dedupeWindow     = 1 * time.Hour
	maxDedupeEntries = 100
	sendTimeout      = 5 * time.Second
)

// New creates a Sentry client. Returns a disabled no-op client if dsn is empty
// or environment is not "production".
func New(dsn, environment string) *Client {
	c := &Client{
		env:       environment,
		dedupeMap: make(map[string]time.Time),
		windowAt:  time.Now(),
	}

	if dsn == "" || environment != "production" {
		c.enabled = false
		return c
	}

	parsed, err := ParseDSN(dsn)
	if err != nil {
		log.Warn("INIT", "Invalid SENTRY_DSN, client disabled", "err", err)
		c.enabled = false
		return c
	}

	c.enabled = true
	c.endpoint = fmt.Sprintf("%s://%s/api/%s/store/", parsed.scheme, parsed.host, parsed.projectID)
	c.authHeader = fmt.Sprintf(
		"Sentry sentry_version=7, sentry_key=%s, sentry_client=ecom-go/1.0",
		parsed.publicKey,
	)
	log.Info("INIT", "Sentry client enabled", "endpoint", c.endpoint)
	return c
}

// Enabled returns whether the client will send events.
func (c *Client) Enabled() bool { return c.enabled }

// CaptureError sends an error event to Sentry asynchronously.
// Never blocks or fails the caller. Tags: request_id, path, method, status.
func (c *Client) CaptureError(message string, tags map[string]string) {
	if !c.enabled {
		return
	}

	sig := dedupeKey(message, tags["path"])

	c.mu.Lock()
	// Rate cap: fixed window per hour
	now := time.Now()
	if now.Sub(c.windowAt) >= time.Hour {
		c.count = 0
		c.windowAt = now
	}
	if c.count >= maxEventsPerHour {
		c.mu.Unlock()
		log.Warn("RATE", "Sentry event dropped (rate cap)", "message", truncate(message, 80))
		return
	}
	// Dedupe: same signature within 1 hour
	if lastSent, ok := c.dedupeMap[sig]; ok && now.Sub(lastSent) < dedupeWindow {
		c.mu.Unlock()
		return
	}
	c.count++
	c.dedupeMap[sig] = now
	c.evictDedupe()
	c.mu.Unlock()

	go c.send(message, tags)
}

// CapturePanic is like CaptureError but sets exception type/value.
func (c *Client) CapturePanic(panicVal interface{}, tags map[string]string) {
	if !c.enabled {
		return
	}
	msg := fmt.Sprintf("panic: %v", panicVal)
	c.CaptureError(msg, tags)
}

// event is the Sentry Store API payload.
type event struct {
	EventID     string            `json:"event_id"`
	Timestamp   string            `json:"timestamp"`
	Platform    string            `json:"platform"`
	Level       string            `json:"level"`
	Environment string            `json:"environment"`
	ServerName  string            `json:"server_name"`
	Message     string            `json:"message"`
	Tags        map[string]string `json:"tags,omitempty"`
	Exception   *exception        `json:"exception,omitempty"`
}

type exception struct {
	Values []exceptionValue `json:"values"`
}

type exceptionValue struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

func (c *Client) send(message string, tags map[string]string) {
	defer func() { recover() }() // never crash from reporting

	evt := event{
		EventID:     generateEventID(),
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		Platform:    "go",
		Level:       "error",
		Environment: c.env,
		ServerName:  "ecom-core-service",
		Message:     message,
		Tags:        tags,
	}

	// If it looks like a panic, also set exception
	if strings.HasPrefix(message, "panic:") {
		evt.Exception = &exception{
			Values: []exceptionValue{{Type: "panic", Value: message}},
		}
	}

	body, err := json.Marshal(evt)
	if err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Sentry-Auth", c.authHeader)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Warn("SEND", "Sentry send failed", "err", err)
		return
	}
	resp.Body.Close()
}

func generateEventID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func dedupeKey(message, path string) string {
	return message + "|" + path
}

func (c *Client) evictDedupe() {
	if len(c.dedupeMap) <= maxDedupeEntries {
		return
	}
	// Evict oldest entry
	var oldestKey string
	var oldestTime time.Time
	for k, v := range c.dedupeMap {
		if oldestKey == "" || v.Before(oldestTime) {
			oldestKey = k
			oldestTime = v
		}
	}
	if oldestKey != "" {
		delete(c.dedupeMap, oldestKey)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
