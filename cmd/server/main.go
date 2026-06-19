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

	// Verifier selection: the real toncenter verifier in prod once a receiving
	// address is set; otherwise a mock that auto-confirms after a couple of polls
	// so the create -> pending -> paid flow can be exercised without real funds.
	var ver wallet.Verifier
	if cfg.IsProd() && cfg.TONReceiving != "" {
		ver = wallet.NewTonVerifier(cfg.TONAPIBase, cfg.TONAPIKey, nil)
		log.Printf("payments: toncenter verifier (%s)", cfg.TONAPIBase)
	} else {
		ver = wallet.NewMockVerifier(2)
		log.Printf("payments: MOCK verifier (dev; auto-confirms after 2 polls)")
	}
	if cfg.TONReceiving == "" {
		log.Printf("WARNING: TON_RECEIVING_ADDRESS not set; invoices cannot be created until it is.")
	}

	// Optional signed webhook on settlement.
	sender := webhook.New(cfg.WebhookURL, cfg.WebhookSecret, nil)
	var wh service.Webhook
	if sender != nil {
		wh = sender
		log.Printf("webhook: enabled -> %s", cfg.WebhookURL)
	}

	svc := service.New(st, ver, cfg.TONReceiving, cfg.DefaultTTL, wh)
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
	sender.Wait() // drain in-flight webhooks (no-op if disabled)
	if pg != nil {
		pg.Close()
	}
	log.Printf("shut down cleanly")
}
