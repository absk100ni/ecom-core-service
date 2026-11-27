package config

import "testing"

func TestValidateProductionAllowsDevelopment(t *testing.T) {
	if err := ValidateProduction(&Config{Environment: "development"}); err != nil {
		t.Fatalf("development should not require production integrations: %v", err)
	}
}

func TestValidateProductionRejectsUnsafeConfiguration(t *testing.T) {
	cfg := &Config{Environment: "production", PaymentGateway: "razorpay", CORSOrigins: "*", JWTSecret: "dev-secret-key", GuestTokenSecret: "dev-guest-secret"}
	if err := ValidateProduction(cfg); err == nil {
		t.Fatal("expected unsafe production configuration to fail")
	}
}

func TestValidateProductionAcceptsCompleteConfiguration(t *testing.T) {
	cfg := &Config{
		Environment: "production", BindHost: "0.0.0.0", BindHostSet: true, CheckoutEnabled: false, CheckoutEnabledSet: true,
		JWTSecret: "strong-jwt", GuestTokenSecret: "strong-guest", CORSOrigins: "https://lucubraelec.in",
		PaymentGateway: "razorpay", RazorpayKeyID: "configured", RazorpaySecret: "configured", RazorpayWebhookSecret: "configured",
		ShipmozoPublicKey: "configured", ShipmozoPrivateKey: "configured", ShipmozoWarehouseID: "configured", ShippingWebhookToken: "configured",
		S3Bucket: "assets", S3Region: "ap-south-1", S3PublicBaseURL: "https://assets.example.com",
		BusinessLegalName: "Business", BusinessAddress: "Address", BusinessGSTIN: "06ABCDE1234F1Z5", BusinessState: "Haryana", BusinessStateCode: "06",
	}
	if err := ValidateProduction(cfg); err != nil {
		t.Fatalf("complete configuration should pass: %v", err)
	}
}
