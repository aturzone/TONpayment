package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/aturzone/TONpayment/internal/config"
)

// validate runs the production "refuse to start" safety gates. It returns a
// descriptive error instead of exiting, so the rules are unit-testable; main()
// turns a non-nil result into log.Fatalf. Dev is permissive (and uses the mock
// verifier), so only prod is gated here.
func validate(cfg *config.Config) error {
	// Multi-tenant gates apply in dev and prod alike: the control plane needs a
	// durable database, a session-signing secret, and a ton_proof domain allow-list.
	if cfg.Multitenant {
		if cfg.DatabaseURL == "" {
			return fmt.Errorf("TON_MULTITENANT=1 requires TON_DATABASE_URL (Postgres) — the tenant control plane is not supported on the file store")
		}
		if len(cfg.SessionSecret) < 16 {
			return fmt.Errorf("TON_MULTITENANT=1 requires TON_SESSION_SECRET of at least 16 chars (signs merchant session tokens)")
		}
		if len(cfg.AuthDomains) == 0 {
			return fmt.Errorf("TON_MULTITENANT=1 requires TON_AUTH_DOMAINS (the allowed ton_proof domain(s), e.g. tonpayment.net)")
		}
	}
	if !cfg.IsProd() {
		return nil
	}
	// Don't run an open, arbitrary-payTo minter with no auth — that lets anyone
	// make the poller watch addresses of their choosing and burn the toncenter quota.
	// Multi-tenant mode is exempt: every create is gated by a per-merchant API key
	// (TenantKeyAuth) and is scoped to a gateway the merchant owns, so there is no
	// open minter even without a global key or default address.
	if !cfg.Multitenant && cfg.TONReceiving == "" && cfg.CreateAPIKey == "" {
		return fmt.Errorf("set TON_RECEIVING_ADDRESS or TON_CREATE_API_KEY (refusing to run an open, arbitrary-address invoice minter)")
	}
	// A payment ledger needs a durable, multi-instance-safe store in prod.
	if cfg.DatabaseURL == "" && os.Getenv("TON_ALLOW_FILE_STORE") != "1" {
		return fmt.Errorf("set TON_DATABASE_URL (Postgres) — the JSON file store is single-node and non-durable; set TON_ALLOW_FILE_STORE=1 to use it deliberately")
	}
	// Webhooks must be signed and sent over TLS in prod.
	if cfg.WebhookURL != "" {
		if cfg.WebhookSecret == "" {
			return fmt.Errorf("set TON_WEBHOOK_SECRET when TON_WEBHOOK_URL is set (unsigned webhooks let a receiver be spoofed)")
		}
		if !strings.HasPrefix(strings.ToLower(cfg.WebhookURL), "https://") {
			return fmt.Errorf("TON_WEBHOOK_URL must be https in prod (got %q) — invoice data must not be sent in cleartext", cfg.WebhookURL)
		}
	}
	return nil
}
