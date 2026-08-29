package config

import (
	"errors"
	"testing"
	"time"
)

func TestLoadDefaultsToFoundationSafeConfiguration(t *testing.T) {
	cfg, err := load(mapLookup(nil))
	if err != nil {
		t.Fatalf("load defaults: %v", err)
	}
	if cfg.Port != 8086 {
		t.Fatalf("default port = %d, want 8086", cfg.Port)
	}
	if cfg.Features.AnyEnabled() {
		t.Fatal("a feature flag defaulted to enabled")
	}
	if err := cfg.ValidateFoundationMode(); err != nil {
		t.Fatalf("validate safe foundation config: %v", err)
	}
	if err := cfg.ValidateRuntime(); err != nil {
		t.Fatalf("validate safe runtime config: %v", err)
	}
}

func TestFoundationRejectsEveryEnabledFlag(t *testing.T) {
	names := []string{
		"LOYALTY_READ_API_ENABLED",
		"LOYALTY_INTERNAL_API_ENABLED",
		"LOYALTY_EVENT_CONSUMPTION_ENABLED",
		"LOYALTY_EARNING_ENABLED",
		"LOYALTY_REDEMPTION_ENABLED",
		"LOYALTY_REFERRALS_ENABLED",
		"LOYALTY_BIRTHDAY_ENABLED",
		"LOYALTY_COUPON_ADAPTER_ENABLED",
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			cfg, err := load(mapLookup(map[string]string{name: "true"}))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if !errors.Is(cfg.ValidateFoundationMode(), ErrFoundationFlagEnabled) {
				t.Fatalf("enabled %s was not rejected", name)
			}
		})
	}
}

func TestRuntimeAllowsConfiguredReadAPI(t *testing.T) {
	cfg, err := load(mapLookup(map[string]string{
		"LOYALTY_READ_API_ENABLED": "true",
		"DATABASE_URL":             "postgres://loyalty@example.test/loyalty",
		"CUSTOMER_JWT_SECRET":      "0123456789abcdef0123456789abcdef",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := cfg.ValidateRuntime(); err != nil {
		t.Fatalf("validate read API: %v", err)
	}
}

func TestRuntimeAllowsConfiguredEventConsumerWithOptionalEarning(t *testing.T) {
	for _, earning := range []string{"false", "true"} {
		cfg, err := load(mapLookup(map[string]string{
			"LOYALTY_EVENT_CONSUMPTION_ENABLED": "true",
			"LOYALTY_EARNING_ENABLED":           earning,
			"DATABASE_URL":                      "postgres://loyalty@example.test/loyalty",
			"AWS_REGION":                        "ap-south-1",
			"LOYALTY_ORDER_EVENTS_QUEUE_URL":    "https://sqs.ap-south-1.amazonaws.com/123/loyalty",
		}))
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if err := cfg.ValidateRuntime(); err != nil {
			t.Fatalf("validate event consumer (earning=%s): %v", earning, err)
		}
	}
}

func TestRuntimeAllowsConfiguredInternalRedemption(t *testing.T) {
	cfg, err := load(mapLookup(map[string]string{
		"LOYALTY_INTERNAL_API_ENABLED": "true",
		"LOYALTY_REDEMPTION_ENABLED":   "true",
		"DATABASE_URL":                 "postgres://loyalty@example.test/loyalty",
		"INTERNAL_SERVICE_TOKEN":       "0123456789abcdef0123456789abcdef",
		"LOYALTY_RESERVATION_TTL":      "90s",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := cfg.ValidateRuntime(); err != nil {
		t.Fatalf("validate redemption: %v", err)
	}
	if cfg.ReservationTTL != 90*time.Second {
		t.Fatalf("reservation TTL = %v", cfg.ReservationTTL)
	}
}

func TestRuntimeRejectsIncompleteOrUnsupportedConfiguration(t *testing.T) {
	tests := []map[string]string{
		{"LOYALTY_READ_API_ENABLED": "true"},
		{
			"LOYALTY_READ_API_ENABLED": "true",
			"DATABASE_URL":             "postgres://loyalty@example.test/loyalty",
			"CUSTOMER_JWT_SECRET":      "too-short",
		},
		{"LOYALTY_EARNING_ENABLED": "true"},
		{"LOYALTY_REDEMPTION_ENABLED": "true"},
		{
			"LOYALTY_EVENT_CONSUMPTION_ENABLED": "true",
			"DATABASE_URL":                      "postgres://loyalty@example.test/loyalty",
		},
	}
	for _, values := range tests {
		cfg, err := load(mapLookup(values))
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if err := cfg.ValidateRuntime(); err == nil {
			t.Fatalf("expected runtime validation failure: %#v", values)
		}
	}
}

func TestLoadRejectsMalformedConfiguration(t *testing.T) {
	tests := []map[string]string{
		{"PORT": "0"},
		{"PORT": "not-a-port"},
		{"LOG_LEVEL": "TRACE"},
		{"LOYALTY_EARNING_ENABLED": "sometimes"},
	}

	for _, values := range tests {
		if _, err := load(mapLookup(values)); err == nil {
			t.Fatalf("expected invalid configuration to fail: %#v", values)
		}
	}
}

func mapLookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}
