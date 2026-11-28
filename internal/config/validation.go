package config

import (
	"fmt"
	"strings"
)

// Check reports whether a dependency or production requirement is ready.
// Detail must describe the requirement without including credential values.
type Check struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
	Ready    bool   `json:"ready"`
	Detail   string `json:"detail"`
}

func ProductionChecks(c *Config) []Check {
	checks := []Check{
		{Name: "bind_host", Required: true, Ready: c.BindHostSet && c.BindHost != "", Detail: "BIND_HOST must be explicitly configured"},
		{Name: "jwt_secret", Required: true, Ready: c.JWTSecret != "" && c.JWTSecret != "dev-secret-key", Detail: "JWT_SECRET must be non-default"},
		{Name: "guest_token_secret", Required: true, Ready: c.GuestTokenSecret != "" && c.GuestTokenSecret != "dev-guest-secret" && c.GuestTokenSecret != "dev-secret-key", Detail: "GUEST_TOKEN_SECRET must be non-default"},
		{Name: "cors", Required: true, Ready: explicitHTTPSOrigins(c.CORSOrigins), Detail: "CORS_ORIGINS must list explicit HTTPS origins"},
		{Name: "checkout_control", Required: true, Ready: c.CheckoutEnabledSet, Detail: "CHECKOUT_ENABLED must be explicitly configured in production"},
		{Name: "shipping", Required: true, Ready: c.ShipmozoPublicKey != "" && c.ShipmozoPrivateKey != "" && c.ShipmozoWarehouseID != "", Detail: "Shipmozo credentials and warehouse are required; tracking uses authenticated polling"},
		{Name: "storage", Required: true, Ready: c.S3Bucket != "" && c.S3Region != "" && c.S3PublicBaseURL != "", Detail: "S3 bucket, region and controlled public CDN origin are required"},
		{Name: "invoice_identity", Required: true, Ready: c.BusinessLegalName != "" && c.BusinessAddress != "" && c.BusinessGSTIN != "" && c.BusinessState != "" && c.BusinessStateCode != "", Detail: "Business identity and GST fields are required"},
		{Name: "redis", Required: false, Ready: c.RedisAddr != "", Detail: "Redis is optional; the service falls back to process-local controls"},
		{Name: "notifications", Required: false, Ready: c.NotificationURL != "" || c.SMTPHost != "" || c.WhatsAppAccessToken != "", Detail: "At least one notification channel is recommended"},
		{Name: "sentry", Required: false, Ready: c.SentryDSN != "", Detail: "SENTRY_DSN is recommended for production error reporting"},
	}

	paymentReady := false
	paymentDetail := "selected payment gateway credentials and webhook secret are required"
	switch strings.ToLower(c.PaymentGateway) {
	case "razorpay":
		paymentReady = c.RazorpayKeyID != "" && c.RazorpaySecret != "" && c.RazorpayWebhookSecret != ""
	case "stripe":
		paymentReady = c.StripeSecretKey != "" && c.StripePublishableKey != "" && c.StripeWebhookSecret != ""
	case "cashfree":
		paymentReady = c.CashfreeAppID != "" && c.CashfreeSecret != "" && c.CashfreeReturnURL != ""
	default:
		paymentDetail = "PAYMENT_GATEWAY must be razorpay, stripe, or cashfree"
	}
	checks = append(checks, Check{Name: "payment", Required: true, Ready: paymentReady, Detail: paymentDetail})
	return checks
}

func ValidateProduction(c *Config) error {
	if c.Environment != "production" {
		return nil
	}
	var failed []string
	for _, check := range ProductionChecks(c) {
		if check.Required && !check.Ready {
			failed = append(failed, check.Name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("unsafe production configuration: %s", strings.Join(failed, ", "))
	}
	return nil
}

func explicitHTTPSOrigins(value string) bool {
	origins := strings.Split(value, ",")
	if len(origins) == 0 {
		return false
	}
	for _, origin := range origins {
		origin = strings.TrimSpace(origin)
		if origin == "" || origin == "*" || !strings.HasPrefix(origin, "https://") {
			return false
		}
	}
	return true
}
