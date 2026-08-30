package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// WhatsApp channel via Meta's Cloud API (graph.facebook.com), stdlib HTTP —
// same no-SDK convention as Stripe/S3/Sentry. Business-initiated messages must
// use pre-approved templates; template names are configurable so they can match
// whatever gets approved in Meta Business Manager.
//
// Mock mode: when WHATSAPP_ACCESS_TOKEN or WHATSAPP_PHONE_NUMBER_ID is unset,
// messages are logged instead of sent (mirrors sendEmail's SMTP mock mode).

const whatsappSendTimeout = 10 * time.Second

// whatsappAPIBase allows tests to point at a local httptest server.
var whatsappAPIBase = "https://graph.facebook.com/v20.0"

func (n *Notifier) whatsappEnabled() bool {
	return n.cfg.WhatsAppAccessToken != "" && n.cfg.WhatsAppPhoneNumberID != ""
}

// normalizeIndianPhone converts checkout phone input into E.164 (+91XXXXXXXXXX).
// Accepts: bare 10-digit, 0-prefixed, 91-prefixed, +91-prefixed, and formatting
// noise (spaces, dashes, parens). Returns "" when it can't produce a valid
// Indian mobile number — caller must skip sending in that case.
func normalizeIndianPhone(raw string) string {
	var digits strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	d := digits.String()
	switch {
	case len(d) == 10:
		// fall through
	case len(d) == 11 && strings.HasPrefix(d, "0"):
		d = d[1:]
	case len(d) == 12 && strings.HasPrefix(d, "91"):
		d = d[2:]
	default:
		return ""
	}
	// Indian mobiles start 6-9
	if d[0] < '6' {
		return ""
	}
	return "+91" + d
}

// waTemplatePayload is Meta's template-message request shape.
type waTemplatePayload struct {
	MessagingProduct string     `json:"messaging_product"`
	To               string     `json:"to"`
	Type             string     `json:"type"`
	Template         waTemplate `json:"template"`
}

type waTemplate struct {
	Name       string        `json:"name"`
	Language   waLanguage    `json:"language"`
	Components []waComponent `json:"components,omitempty"`
}

type waLanguage struct {
	Code string `json:"code"`
}

type waComponent struct {
	Type       string        `json:"type"`
	Parameters []waParameter `json:"parameters"`
}

type waParameter struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// sendWhatsAppTemplate dispatches one template message (or logs in mock mode).
// params fill the template's {{1}}..{{n}} body placeholders in order.
func (n *Notifier) sendWhatsAppTemplate(rawPhone, templateName string, params []string) {
	to := normalizeIndianPhone(rawPhone)
	if to == "" {
		log.Warn("sendWhatsAppTemplate", "Skipping WhatsApp: phone not normalizable", "template", templateName)
		return
	}
	if !n.whatsappEnabled() {
		log.Info("sendWhatsAppTemplate", "Mock WhatsApp sent",
			"to", to, "template", templateName, "params", strings.Join(params, " | "))
		return
	}

	waParams := make([]waParameter, 0, len(params))
	for _, p := range params {
		waParams = append(waParams, waParameter{Type: "text", Text: p})
	}
	payload := waTemplatePayload{
		MessagingProduct: "whatsapp",
		To:               to,
		Type:             "template",
		Template: waTemplate{
			Name:     templateName,
			Language: waLanguage{Code: n.cfg.WhatsAppTemplateLang},
			Components: []waComponent{
				{Type: "body", Parameters: waParams},
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		log.Error("sendWhatsAppTemplate", "Marshal failed", "err", err)
		return
	}

	url := fmt.Sprintf("%s/%s/messages", whatsappAPIBase, n.cfg.WhatsAppPhoneNumberID)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		log.Error("sendWhatsAppTemplate", "Request build failed", "err", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+n.cfg.WhatsAppAccessToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: whatsappSendTimeout}
	resp, err := client.Do(req)
	if err != nil {
		log.Error("sendWhatsAppTemplate", "Send failed", "template", templateName, "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var apiErr bytes.Buffer
		apiErr.ReadFrom(resp.Body)
		log.Error("sendWhatsAppTemplate", "Meta API rejected message",
			"template", templateName, "status", resp.StatusCode, "body", truncate(apiErr.String(), 300))
		return
	}
	log.Info("sendWhatsAppTemplate", "WhatsApp sent", "to", to, "template", templateName)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// rupees renders paise as a plain rupee string for template params (₹ symbol
// lives in the approved template copy, not the parameter).
func rupees(paise int) string {
	return fmt.Sprintf("%.2f", float64(paise)/100)
}
