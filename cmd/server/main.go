// Command server runs the TONpayment service: a non-custodial, watch-only TON
// payment verifier + invoicing API.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aturzone/TONpayment/internal/auth"
	"github.com/aturzone/TONpayment/internal/config"
	"github.com/aturzone/TONpayment/internal/httpx"
	"github.com/aturzone/TONpayment/internal/poller"
	"github.com/aturzone/TONpayment/internal/service"
	"github.com/aturzone/TONpayment/internal/store"
	"github.com/aturzone/TONpayment/internal/tenant"
	"github.com/aturzone/TONpayment/internal/tonaddr"
	"github.com/aturzone/TONpayment/internal/wallet"
	"github.com/aturzone/TONpayment/internal/webhook"
)

func main() {
	cfg := config.Load()
	if err := validate(cfg); err != nil {
		log.Fatalf("config: %v", err)
	}

	// Store: Postgres when a URL is set, otherwise in-memory/JSON.
	var st store.Store
	var pg *store.Postgres
	if cfg.DatabaseURL != "" {
		p, err := store.NewPostgres(context.Background(), cfg.DatabaseURL)
		if err != nil {
			log.Fatalf("postgres init: %v", err)
		}
		pg = p
		st = p
		log.Printf("store: postgres")
	} else {
		mem, err := store.NewMemory(cfg.DataDir)
		if err != nil {
			log.Fatalf("store init: %v", err)
		}
		st = mem
		log.Printf("store: in-memory/json dir=%s", cfg.DataDir)
	}

	// Multi-tenant mode (the hosted gateway platform) requires Postgres; apply the
	// additive tenant schema. Single-tenant boot never touches these tables.
	if cfg.Multitenant {
		if pg == nil {
			log.Fatalf("config: TON_MULTITENANT=1 requires TON_DATABASE_URL (Postgres)")
		}
		if err := pg.MigrateTenant(context.Background()); err != nil {
			log.Fatalf("postgres tenant migrate: %v", err)
		}
		log.Printf("mode: MULTI-TENANT gateway platform")
	}

	// Validate/normalize the receiving address at the boundary. A malformed address
	// would otherwise be handed verbatim to toncenter, match nothing, and leave every
	// invoice stuck pending (the verifier fails closed). Fail fast in prod; in dev the
	// mock verifier ignores the address, so only warn.
	if cfg.TONReceiving != "" {
		norm, err := tonaddr.NormalizeOnNetwork(cfg.TONReceiving, cfg.IsTestnet())
		if err != nil {
			if cfg.IsProd() {
				log.Fatalf("config: TON_RECEIVING_ADDRESS %q is not a valid TON address: %v", cfg.TONReceiving, err)
			}
			log.Printf("WARNING: TON_RECEIVING_ADDRESS %q is not a valid TON address (%v); the dev mock ignores it, but prod will reject it.", cfg.TONReceiving, err)
		} else {
			cfg.TONReceiving = norm
		}
	}

	// Verifier selection. Production ALWAYS uses the real toncenter verifier — we must
	// never fall back to the mock (which auto-confirms payments) in prod. Dev uses the
	// mock so the create -> pending -> paid flow can be exercised without real funds.
	var ver wallet.Verifier
	if cfg.IsProd() {
		ver = wallet.NewTonVerifier(cfg.TONAPIBase, cfg.TONAPIKey, nil)
		log.Printf("payments: toncenter verifier (%s)", cfg.TONAPIBase)
	} else {
		ver = wallet.NewMockVerifier(2)
		log.Printf("payments: MOCK verifier (dev; auto-confirms after 2 polls — NEVER used in prod)")
	}
	if cfg.TONReceiving == "" {
		log.Printf("payments: no default receiving address — every invoice must supply its own payTo")
	}

	// Optional signed webhook on settlement (prod requirements are enforced in
	// validate; the sender refuses to follow redirects). A dev webhook without a
	// secret is allowed but unsigned — warn.
	if cfg.WebhookURL != "" && cfg.WebhookSecret == "" {
		log.Printf("WARNING: TON_WEBHOOK_URL set without TON_WEBHOOK_SECRET; deliveries are UNSIGNED (dev only)")
	}
	sender := webhook.New(cfg.WebhookURL, cfg.WebhookSecret, nil)
	var globalWH service.Webhook
	if sender != nil {
		globalWH = sender
		log.Printf("webhook: global sink enabled -> %s", cfg.WebhookURL)
	}

	// In multi-tenant mode, settlement webhooks fan out to each gateway's own
	// endpoints (the router satisfies service.Webhook); the global sink, if any, is
	// the fallback for untagged invoices.
	var router *tenant.WebhookRouter
	wh := globalWH
	if cfg.Multitenant {
		router = tenant.NewWebhookRouter(pg, globalWH)
		wh = router
	}

	svc := service.New(st, ver, cfg.TONReceiving, cfg.DefaultTTL, wh)
	svc.SetLimits(cfg.MaxTTL, cfg.MaxPending, cfg.MaxPendingPerAddr)
	svc.SetNetwork(cfg.IsTestnet())
	log.Printf("limits: maxTTL=%s maxPending=%d maxPendingPerAddress=%d (0 = unlimited); network=%s", cfg.MaxTTL, cfg.MaxPending, cfg.MaxPendingPerAddr, cfg.Network)

	// Assemble HTTP services. Multi-tenant adds ton_proof sign-in, the control plane,
	// per-merchant data-plane auth, and scoped reads; single-tenant leaves these
	// nil (NewServer defaults to the single-key gate — exactly today's behavior).
	svcs := httpx.Services{Cfg: cfg, Service: svc}
	if cfg.Multitenant {
		resolver := wallet.NewPubKeyClient(cfg.TONAPIBase, cfg.TONAPIKey, nil)
		domains := map[string]bool{}
		for _, d := range cfg.AuthDomains {
			domains[strings.ToLower(d)] = true
		}
		adminWallets := map[string]bool{}
		for _, wstr := range cfg.AdminWallets {
			if c, err := tonaddr.Canonical(wstr, cfg.IsTestnet()); err == nil {
				adminWallets[c] = true
			} else {
				log.Printf("WARNING: ignoring invalid TON_ADMIN_WALLETS entry %q: %v", wstr, err)
			}
		}
		svcs.AuthSvc = &auth.Service{
			Store:         pg,
			Resolve:       resolver.GetPublicKey,
			SessionSecret: []byte(cfg.SessionSecret),
			Domains:       domains,
			AdminWallets:  adminWallets,
		}
		svcs.Auth = auth.TenantKeyAuth{Store: pg}
		svcs.Tenant = pg
		svcs.TenantSvc = tenant.New(pg, cfg.IsTestnet())
		svcs.SessionSecret = []byte(cfg.SessionSecret)
		log.Printf("auth: ton_proof sign-in (domains=%v, admins=%d)", cfg.AuthDomains, len(adminWallets))
	}
	srv := httpx.NewServer(svcs)

	// Background poller settles/expires pending invoices so callers needn't poll.
	pollCtx, stopPoller := context.WithCancel(context.Background())
	if cfg.PollEnabled {
		go poller.New(svc, cfg.PollInterval, cfg.PollConcurrency).Run(pollCtx)
	}

	go func() {
		log.Printf("tonpayment listening on %s (env=%s)", cfg.Addr, cfg.Env)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	stopPoller()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	// Drain in-flight webhooks, but don't let slow/retrying deliveries block shutdown
	// forever (no-op if webhooks are disabled).
	drained := make(chan struct{})
	go func() {
		if router != nil {
			router.Wait() // drain per-gateway fan-outs
		}
		sender.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		log.Printf("shutdown: webhook drain timed out after 5s")
	}
	if pg != nil {
		pg.Close()
	}
	log.Printf("shut down cleanly")
}
