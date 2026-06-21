package tenant

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/aturzone/TONpayment/internal/service"
	"github.com/aturzone/TONpayment/internal/store"
	"github.com/aturzone/TONpayment/internal/webhook"
)

// WebhookRouter satisfies service.Webhook (Fire(inv)) so the settlement call site is
// unchanged, but it fans a settled invoice out to its gateway's own active webhook
// endpoints — each signed with that endpoint's secret — and records every delivery
// for the dashboard's webhook-health view. An invoice with no gateway (single-tenant
// row in a mixed deployment) falls back to the global sender, if configured.
type WebhookRouter struct {
	store  store.TenantStore
	global service.Webhook // optional fallback for inv.GatewayID == ""
	client *http.Client
	wg     sync.WaitGroup
}

var _ service.Webhook = (*WebhookRouter)(nil)

// NewWebhookRouter builds the router with one shared HTTP client that refuses
// redirects — an outbound redirect could leak the signed invoice to an unintended
// (e.g. internal) host (SSRF), matching webhook.New's default protection.
func NewWebhookRouter(ts store.TenantStore, global service.Webhook) *WebhookRouter {
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("webhook: redirects are not followed")
		},
	}
	return &WebhookRouter{store: ts, global: global, client: client}
}

// Fire routes a settled invoice. It returns immediately; delivery runs in a tracked
// goroutine so shutdown can drain it.
func (r *WebhookRouter) Fire(inv store.Invoice) {
	if inv.GatewayID == "" {
		if r.global != nil {
			r.global.Fire(inv)
		}
		return
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.fanout(inv)
	}()
}

func (r *WebhookRouter) fanout(inv store.Invoice) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	eps, err := r.store.ListActiveWebhookEndpoints(ctx, inv.GatewayID)
	if err != nil {
		log.Printf("webhook: list endpoints for gateway %s: %v", inv.GatewayID, err)
		return
	}
	for _, ep := range eps {
		// webhook.New with the shared (redirect-refusing) client signs with this
		// endpoint's own secret and retries with backoff.
		ok := webhook.New(ep.URL, ep.Secret, r.client).DeliverSync(inv)
		if err := r.store.RecordWebhookDelivery(ctx, inv.ID, ep.ID, 1, 0, ok); err != nil {
			log.Printf("webhook: record delivery inv=%s ep=%s: %v", inv.ID, ep.ID, err)
		}
	}
}

// Wait drains in-flight fan-outs on shutdown.
func (r *WebhookRouter) Wait() { r.wg.Wait() }
