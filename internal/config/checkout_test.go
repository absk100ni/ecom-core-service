package config

import (
	"os"
	"testing"
)

func withoutCheckoutEnv(t *testing.T) {
	t.Helper()
	value, existed := os.LookupEnv("CHECKOUT_ENABLED")
	if err := os.Unsetenv("CHECKOUT_ENABLED"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv("CHECKOUT_ENABLED", value)
		} else {
			_ = os.Unsetenv("CHECKOUT_ENABLED")
		}
	})
}

func TestCheckoutDefaultsEnabledInDevelopment(t *testing.T) {
	t.Setenv("ENVIRONMENT", "development")
	withoutCheckoutEnv(t)
	cfg := Load()
	if !cfg.CheckoutEnabled || cfg.CheckoutEnabledSet {
		t.Fatalf("development default should be enabled and implicit: %+v", cfg)
	}
}

func TestCheckoutDefaultsDisabledInProduction(t *testing.T) {
	t.Setenv("ENVIRONMENT", "production")
	withoutCheckoutEnv(t)
	cfg := Load()
	if cfg.CheckoutEnabled || cfg.CheckoutEnabledSet {
		t.Fatalf("production default should fail closed and remain implicit: %+v", cfg)
	}
}

func TestCheckoutExplicitProductionControl(t *testing.T) {
	t.Setenv("ENVIRONMENT", "production")
	t.Setenv("CHECKOUT_ENABLED", "false")
	cfg := Load()
	if cfg.CheckoutEnabled || !cfg.CheckoutEnabledSet {
		t.Fatalf("explicit false should be recognized: %+v", cfg)
	}
}
