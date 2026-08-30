package config

import "os"

type Config struct {
	Port        string
	MongoURI    string
	MongoDB     string
	RedisAddr   string
	JWTSecret   string
	Environment string
	CORSOrigins string // comma-separated allowed origins
	LogLevel    string // debug, info, warn, error
	LogFormat   string // "json" or "text"
	// OTP
	OTPService      string
	MSG91AuthKey    string
	MSG91TemplateID string
	// Google Auth
	GoogleClientID string
	// Payment
	PaymentGateway        string // "stripe", "razorpay", or "cashfree"
	StripeSecretKey       string // sk_test_... or sk_live_...
	StripePublishableKey  string // pk_test_... or pk_live_... (returned to clients)
	StripeWebhookSecret   string // whsec_... (verifies webhook signatures)
	RazorpayKeyID         string
	RazorpaySecret        string
	RazorpayWebhookSecret string // dedicated secret for webhook HMAC (Razorpay dashboard → Webhooks)
	CashfreeAppID         string
	CashfreeSecret        string // also used to verify Cashfree webhook signature
	CashfreeEnv           string // "sandbox" or "production"
	CashfreeReturnURL     string // storefront URL Cashfree redirects to after checkout
	// Partial Payment
	PartialPaymentEnabled        bool
	PartialPaymentDefaultPercent int
	AllowFullCOD                 bool
	// Shipping
	ShipmozoBaseURL      string
	ShipmozoPublicKey    string
	ShipmozoPrivateKey   string
	ShipmozoWarehouseID  string
	ShippingWebhookToken string // shared secret sent as x-api-key on webhooks
	WarehousePincode     string // origin pincode for serviceability/EDD lookups
	// Legacy Shiprocket (removed, backward-compat fallback for webhook token)
	ShiprocketToken string
	// Notification
	NotificationURL string
	// Storage
	S3Bucket        string
	S3Region        string
	S3PublicBaseURL string // CDN base URL for public access
	// Admin
	AdminEmail    string
	AdminPassword string
	// SMTP (Notification Service)
	SMTPHost     string
	SMTPPort     string
	SMTPUser     string
	SMTPPass     string
	SMTPFrom     string
	SMTPFromName string
	// WhatsApp (Meta Cloud API)
	WhatsAppAccessToken       string
	WhatsAppPhoneNumberID     string
	WhatsAppTemplateLang      string
	WhatsAppTplOrderConfirmed string
	WhatsAppTplOrderShipped   string
	WhatsAppTplRefund         string
	// Store branding
	StoreName string
	StoreURL  string
	// Contact
	ContactNotifyEmail string
	// GST / Invoice
	BusinessLegalName string
	BusinessAddress   string
	BusinessGSTIN     string
	BusinessState     string
	BusinessStateCode string
	DefaultGSTPercent int
	DefaultHSNCode    string
	// Returns
	ReturnWindowDays int
	// Order Expiry
	PendingOrderTTLMinutes int
	// Guest Checkout
	GuestTokenSecret string
	// Sentry
	SentryDSN string
}

func Load() *Config {
	return &Config{
		Port:                         getEnv("PORT", "8080"),
		MongoURI:                     getEnv("MONGO_URI", "mongodb://localhost:27017/ecom"),
		MongoDB:                      getEnv("MONGO_DB", "ecom"),
		RedisAddr:                    getEnv("REDIS_ADDR", "localhost:6379"),
		JWTSecret:                    getEnv("JWT_SECRET", "dev-secret-key"),
		Environment:                  getEnv("ENVIRONMENT", "development"),
		CORSOrigins:                  getEnv("CORS_ORIGINS", "*"),
		LogLevel:                     getEnv("LOG_LEVEL", "info"),
		LogFormat:                    getEnv("LOG_FORMAT", "text"),
		OTPService:                   getEnv("OTP_SERVICE", "mock"),
		MSG91AuthKey:                 getEnv("MSG91_AUTH_KEY", ""),
		MSG91TemplateID:              getEnv("MSG91_TEMPLATE_ID", ""),
		GoogleClientID:               getEnv("GOOGLE_CLIENT_ID", ""),
		PaymentGateway:               getEnv("PAYMENT_GATEWAY", "razorpay"),
		StripeSecretKey:              getEnv("STRIPE_SECRET_KEY", ""),
		StripePublishableKey:         getEnv("STRIPE_PUBLISHABLE_KEY", ""),
		StripeWebhookSecret:          getEnv("STRIPE_WEBHOOK_SECRET", ""),
		RazorpayKeyID:                getEnv("RAZORPAY_KEY_ID", ""),
		RazorpaySecret:               getEnv("RAZORPAY_KEY_SECRET", ""),
		RazorpayWebhookSecret:        getEnv("RAZORPAY_WEBHOOK_SECRET", ""),
		CashfreeAppID:                getEnv("CASHFREE_APP_ID", ""),
		CashfreeSecret:               getEnv("CASHFREE_SECRET_KEY", ""),
		CashfreeEnv:                  getEnv("CASHFREE_ENV", "sandbox"),
		CashfreeReturnURL:            getEnv("CASHFREE_RETURN_URL", "https://yourstore.com/orders"),
		PartialPaymentEnabled:        getEnv("PARTIAL_PAYMENT_ENABLED", "true") == "true",
		PartialPaymentDefaultPercent: atoiDefault(getEnv("PARTIAL_PAYMENT_DEFAULT_PERCENT", "20"), 20),
		AllowFullCOD:                 getEnv("ALLOW_FULL_COD", "false") == "true",
		ShipmozoBaseURL:              getEnv("SHIPMOZO_BASE_URL", "https://shipping-api.com/app/api/v1"),
		ShipmozoPublicKey:            getEnv("SHIPMOZO_PUBLIC_KEY", ""),
		ShipmozoPrivateKey:           getEnv("SHIPMOZO_PRIVATE_KEY", ""),
		ShipmozoWarehouseID:          getEnv("SHIPMOZO_WAREHOUSE_ID", ""),
		ShippingWebhookToken:         getEnvWithFallback("SHIPPING_WEBHOOK_TOKEN", "SHIPROCKET_WEBHOOK_TOKEN", ""),
		WarehousePincode:             getEnv("WAREHOUSE_PINCODE", "110001"),
		ShiprocketToken:              getEnv("SHIPROCKET_TOKEN", ""),
		NotificationURL:              getEnv("NOTIFICATION_SERVICE_URL", "http://localhost:9090"),
		S3Bucket:                     getEnv("S3_BUCKET", ""),
		S3Region:                     getEnv("S3_REGION", "ap-south-1"),
		S3PublicBaseURL:              getEnv("S3_PUBLIC_BASE_URL", ""),
		AdminEmail:                   getEnv("ADMIN_EMAIL", ""),
		AdminPassword:                getEnv("ADMIN_PASSWORD", ""),
		SMTPHost:                     getEnv("SMTP_HOST", ""),
		SMTPPort:                     getEnv("SMTP_PORT", "587"),
		SMTPUser:                     getEnv("SMTP_USER", ""),
		SMTPPass:                     getEnv("SMTP_PASS", ""),
		SMTPFrom:                     getEnv("SMTP_FROM", ""),
		SMTPFromName:                 getEnv("SMTP_FROM_NAME", ""),
		WhatsAppAccessToken:          getEnv("WHATSAPP_ACCESS_TOKEN", ""),
		WhatsAppPhoneNumberID:        getEnv("WHATSAPP_PHONE_NUMBER_ID", ""),
		WhatsAppTemplateLang:         getEnv("WHATSAPP_TEMPLATE_LANG", "en"),
		WhatsAppTplOrderConfirmed:    getEnv("WHATSAPP_TPL_ORDER_CONFIRMED", "order_confirmed"),
		WhatsAppTplOrderShipped:      getEnv("WHATSAPP_TPL_ORDER_SHIPPED", "order_shipped"),
		WhatsAppTplRefund:            getEnv("WHATSAPP_TPL_REFUND", "refund_processed"),
		StoreName:                    getEnv("STORE_NAME", "ElectroMart"),
		StoreURL:                     getEnv("STORE_URL", "https://electromart.in"),
		ContactNotifyEmail:           getEnv("CONTACT_NOTIFY_EMAIL", ""),
		BusinessLegalName:            getEnv("BUSINESS_LEGAL_NAME", "ElectroMart Private Limited"),
		BusinessAddress:              getEnv("BUSINESS_ADDRESS", ""),
		BusinessGSTIN:                getEnv("BUSINESS_GSTIN", ""),
		BusinessState:                getEnv("BUSINESS_STATE", "Delhi"),
		BusinessStateCode:            getEnv("BUSINESS_STATE_CODE", "07"),
		DefaultGSTPercent:            atoiDefault(getEnv("DEFAULT_GST_PERCENT", "18"), 18),
		DefaultHSNCode:               getEnv("DEFAULT_HSN_CODE", "8471"),
		ReturnWindowDays:             atoiDefault(getEnv("RETURN_WINDOW_DAYS", "7"), 7),
		PendingOrderTTLMinutes:       atoiDefault(getEnv("PENDING_ORDER_TTL_MINUTES", "30"), 30),
		GuestTokenSecret:             getEnvWithFallback("GUEST_TOKEN_SECRET", "JWT_SECRET", "dev-guest-secret"),
		SentryDSN:                    getEnv("SENTRY_DSN", ""),
	}
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

func getEnvWithFallback(primary, fallback, def string) string {
	if val := os.Getenv(primary); val != "" {
		return val
	}
	if val := os.Getenv(fallback); val != "" {
		return val
	}
	return def
}

func atoiDefault(s string, def int) int {
	n := 0
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		} else {
			return def
		}
	}
	if n == 0 && s != "0" {
		return def
	}
	return n
}
