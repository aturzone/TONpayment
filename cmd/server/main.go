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
	"syscall"
	"time"

	"github.com/aturzone/TONpayment/internal/config"
	"github.com/aturzone/TONpayment/internal/httpx"
	"github.com/aturzone/TONpayment/internal/poller"
	"github.com/aturzone/TONpayment/internal/service"
	"github.com/aturzone/TONpayment/internal/store"
	"github.com/aturzone/TONpayment/internal/tonaddr"
	"github.com/aturzone/TONpayment/internal/wallet"
	"github.com/aturzone/TONpayment/internal/webhook"
)

func main() {
	cfg := config.Load()

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

	// Validate/normalize the receiving address at the boundary. A malformed address
	// would otherwise be handed verbatim to toncenter, match nothing, and leave every
	// invoice stuck pending (the verifier fails closed). Fail fast in prod; in dev the
	// mock verifier ignores the address, so only warn.
	if cfg.TONReceiving != "" {
		norm, err := tonaddr.Normalize(cfg.TONReceiving)
		if err != nil {
			if cfg.IsProd() {
				log.Fatalf("config: TON_RECEIVING_ADDRESS %q is not a valid TON address: %v", cfg.TONReceiving, err)
			}
			log.Printf("WARNING: TON_RECEIVING_ADDRESS %q is not a valid TON address (%v); the dev mock ignores it, but prod will reject it.", cfg.TONReceiving, err)
		} else {
			cfg.TONReceiving = norm
		}
	}

	// Per-request payTo mode: the receiving address may be supplied per invoice, so a
	// default TON_RECEIVING_ADDRESS is optional. But an open create endpoint that
	// accepts arbitrary addresses with no auth lets anyone make the poller watch
	// addresses of their choosing (burning the toncenter quota) — refuse to run that
	// wide open in prod. Require either a default address or a create-API-key gate.
	if cfg.IsProd() && cfg.TONReceiving == "" && cfg.CreateAPIKey == "" {
		log.Fatalf("config: in prod set TON_RECEIVING_ADDRESS (a default receiving address) or TON_CREATE_API_KEY (to gate invoice creation); refusing to run an open, arbitrary-address invoice minter")
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

	// Optional signed webhook on settlement.
	sender := webhook.New(cfg.WebhookURL, cfg.WebhookSecret, nil)
	var wh service.Webhook
	if sender != nil {
		wh = sender
		log.Printf("webhook: enabled -> %s", cfg.WebhookURL)
	}

	svc := service.New(st, ver, cfg.TONReceiving, cfg.DefaultTTL, wh)
	svc.SetLimits(cfg.MaxTTL, cfg.MaxPending, cfg.MaxPendingPerAddr)
	log.Printf("limits: maxTTL=%s maxPending=%d maxPendingPerAddress=%d (0 = unlimited)", cfg.MaxTTL, cfg.MaxPending, cfg.MaxPendingPerAddr)
	srv := httpx.NewServer(httpx.Services{Cfg: cfg, Service: svc})

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
	go func() { sender.Wait(); close(drained) }()
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
