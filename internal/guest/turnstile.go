package guest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func verifyTurnstile(ctx context.Context, secret, token, remoteIP string, bypass bool) bool {
	if bypass {
		return true
	}
	if secret == "" || token == "" {
		return false
	}
	form := url.Values{"secret": {secret}, "response": {token}}
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://challenges.cloudflare.com/turnstile/v0/siteverify", strings.NewReader(form.Encode()))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var result struct {
		Success bool `json:"success"`
	}
	return json.NewDecoder(resp.Body).Decode(&result) == nil && result.Success
}
