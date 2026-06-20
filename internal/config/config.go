// Package config is the single source of truth for runtime configuration. All
// values come from TON_*-prefixed environment variables; there are no secrets to
// generate and nothing is read from disk.
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr           string
	Env            string // "dev" or "prod"
	AllowedOrigins []string

	// Storage: Postgres if DatabaseURL is set, else in-memory/JSON under DataDir.
	DatabaseURL string
	DataDir     string

	// TrustProxy: honor X-Forwarded-For for client-IP (rate-limit) keying. Only
	// enable when actually behind a trusted reverse proxy.
	TrustProxy bool

	// Payments (all public, safe values).
	TONReceiving string
	TONAPIBase   string
	TONAPIKey    string

	// Invoicing.
	DefaultTTL   time.Duration
	CreateAPIKey string // if set, POST /v1/invoices requires this key

	// Resource bounds (0 = unlimited for the pending caps).
	MaxTTL            time.Duration
	MaxPending        int
	MaxPendingPerAddr int

	// Background poller.
	PollEnabled     bool
	PollInterval    time.Duration
	PollConcurrency int

	// Webhook (optional).
	WebhookURL    string
	WebhookSecret string
}

func (c *Config) IsProd() bool { return c.Env == "prod" || c.Env == "production" }

// Load reads configuration from the environment, applying sensible defaults.
func Load() *Config {
	c := &Config{
		Addr:              firstNonEmpty(os.Getenv("TON_HTTP_ADDR"), portAddr(os.Getenv("PORT")), ":8080"),
		Env:               getenv("TON_ENV", "dev"),
		DatabaseURL:       os.Getenv("TON_DATABASE_URL"),
		DataDir:           getenv("TON_DATA_DIR", "data"),
		TrustProxy:        boolDef(os.Getenv("TON_TRUST_PROXY"), false),
		TONReceiving:      os.Getenv("TON_RECEIVING_ADDRESS"),
		TONAPIBase:        getenv("TON_API_BASE", "https://toncenter.com/api/v2"),
		TONAPIKey:         os.Getenv("TON_API_KEY"),
		DefaultTTL:        time.Duration(atoiDef(os.Getenv("TON_DEFAULT_TTL_SECONDS"), 900)) * time.Second,
		CreateAPIKey:      os.Getenv("TON_CREATE_API_KEY"),
		MaxTTL:            time.Duration(atoiDef(os.Getenv("TON_MAX_TTL_SECONDS"), 86400)) * time.Second,
		MaxPending:        atoiDef(os.Getenv("TON_MAX_PENDING"), 10000),
		MaxPendingPerAddr: atoiDef(os.Getenv("TON_MAX_PENDING_PER_ADDRESS"), 200),
		PollEnabled:       boolDef(os.Getenv("TON_POLL_ENABLED"), true),
		PollInterval:      time.Duration(atoiDef(os.Getenv("TON_POLL_INTERVAL_SECONDS"), 10)) * time.Second,
		PollConcurrency:   atoiDef(os.Getenv("TON_POLL_CONCURRENCY"), 4),
		WebhookURL:        os.Getenv("TON_WEBHOOK_URL"),
		WebhookSecret:     os.Getenv("TON_WEBHOOK_SECRET"),
	}
	c.AllowedOrigins = splitTrim(getenv("TON_ALLOWED_ORIGINS", "http://localhost:5173,http://localhost:4173"))
	return c
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

func portAddr(p string) string {
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, ":") {
		return p
	}
	return ":" + p
}

func atoiDef(s string, def int) int {
	if s == "" {
		return def
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func boolDef(s string, def bool) bool {
	if s == "" {
		return def
	}
	if b, err := strconv.ParseBool(s); err == nil {
		return b
	}
	return def
}

func splitTrim(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
