package config

import "os"

type Config struct {
	Port             string
	MongoURI         string
	MongoDB          string
	RedisAddr        string
	JWTSecret        string
	Environment      string
	// OTP
	OTPService       string
	MSG91AuthKey     string
	MSG91TemplateID  string
	// Payment
	PaymentGateway   string // "razorpay" or "cashfree"
	RazorpayKeyID    string
	RazorpaySecret   string
	CashfreeAppID    string
	CashfreeSecret   string
	CashfreeEnv      string // "sandbox" or "production"
	// Shipping
	ShiprocketEmail  string
	ShiprocketPass   string
	ShiprocketToken  string
	// Notification
	NotificationURL  string
	// Storage
	S3Bucket         string
	S3Region         string
}

func Load() *Config {
	return &Config{
		Port:            getEnv("PORT", "8080"),
		MongoURI:        getEnv("MONGO_URI", "mongodb://localhost:27017/ecom"),
		MongoDB:         getEnv("MONGO_DB", "ecom"),
		RedisAddr:       getEnv("REDIS_ADDR", "localhost:6379"),
		JWTSecret:       getEnv("JWT_SECRET", "dev-secret-key"),
		Environment:     getEnv("ENVIRONMENT", "development"),
		OTPService:      getEnv("OTP_SERVICE", "mock"),
		MSG91AuthKey:    getEnv("MSG91_AUTH_KEY", ""),
		MSG91TemplateID: getEnv("MSG91_TEMPLATE_ID", ""),
		PaymentGateway:  getEnv("PAYMENT_GATEWAY", "razorpay"),
		RazorpayKeyID:   getEnv("RAZORPAY_KEY_ID", ""),
		RazorpaySecret:  getEnv("RAZORPAY_KEY_SECRET", ""),
		CashfreeAppID:   getEnv("CASHFREE_APP_ID", ""),
		CashfreeSecret:  getEnv("CASHFREE_SECRET_KEY", ""),
		CashfreeEnv:     getEnv("CASHFREE_ENV", "sandbox"),
		ShiprocketEmail: getEnv("SHIPROCKET_EMAIL", ""),
		ShiprocketPass:  getEnv("SHIPROCKET_PASSWORD", ""),
		ShiprocketToken: getEnv("SHIPROCKET_TOKEN", ""),
		NotificationURL: getEnv("NOTIFICATION_SERVICE_URL", "http://localhost:9090"),
		S3Bucket:        getEnv("S3_BUCKET", ""),
		S3Region:        getEnv("S3_REGION", "ap-south-1"),
	}
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}
