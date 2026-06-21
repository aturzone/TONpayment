package config

import (
	"testing"
	"time"
)

// TestLoadDefaultsAndOverrides covers the security-relevant knobs: defaults,
// env overrides, and that a malformed int silently falls back to its default.
func TestLoadDefaultsAndOverrides(t *testing.T) {
	for _, k := range []string{
		"TON_ENV", "TON_NETWORK", "TON_DATABASE_URL", "TON_DEFAULT_TTL_SECONDS",
		"TON_TRUST_PROXY", "PORT", "TON_HTTP_ADDR", "TON_CREATE_API_KEY",
	} {
		t.Setenv(k, "")
	}

	c := Load()
	if c.IsProd() {
		t.Error("default env should be dev (not prod)")
	}
	if c.IsTestnet() {
		t.Error("default network should be mainnet")
	}
	if c.Addr != ":8080" {
		t.Errorf("default addr = %q, want :8080", c.Addr)
	}
	if c.DefaultTTL != 900*time.Second {
		t.Errorf("default ttl = %v, want 900s", c.DefaultTTL)
	}
	if c.TrustProxy {
		t.Error("TrustProxy should default to false")
	}

	t.Setenv("TON_ENV", "prod")
	t.Setenv("TON_NETWORK", "testnet")
	t.Setenv("TON_DEFAULT_TTL_SECONDS", "not-a-number")
	c2 := Load()
	if !c2.IsProd() {
		t.Error("env=prod not detected")
	}
	if !c2.IsTestnet() {
		t.Error("network=testnet not detected")
	}
	if c2.DefaultTTL != 900*time.Second {
		t.Errorf("malformed ttl should fall back to 900s, got %v", c2.DefaultTTL)
	}
}
