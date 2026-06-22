package tenant

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/aturzone/TONpayment/internal/service"
	"github.com/aturzone/TONpayment/internal/store"
	"github.com/aturzone/TONpayment/internal/webhook"
)

// maxConcurrentDeliveries bounds outbound webhook deliveries across ALL fan-outs, so
// a burst of settlements (or many slow merchant endpoints) cannot spawn unbounded
// goroutines/connections and starve the host.
const maxConcurrentDeliveries = 64

// WebhookRouter satisfies service.Webhook (Fire(inv)) so the settlement call site is
// unchanged, but it fans a settled invoice out to its gateway's own active webhook
// endpoints — each signed with that endpoint's secret — and records every delivery
// for the dashboard's webhook-health view. An invoice with no gateway (single-tenant
// row in a mixed deployment) falls back to the global sender, if configured.
type WebhookRouter struct {
	store  store.TenantStore
	global service.Webhook // optional fallback for inv.GatewayID == ""
	client *http.Client
	sem    chan struct{} // global cap on concurrent outbound deliveries
	wg     sync.WaitGroup
}

var _ service.Webhook = (*WebhookRouter)(nil)

// NewWebhookRouter builds the router with one shared HTTP client that refuses both
// redirects and non-public dial targets — either could leak the signed invoice to
// an internal host (SSRF) — matching webhook.New's default protection.
func NewWebhookRouter(ts store.TenantStore, global service.Webhook) *WebhookRouter {
	return &WebhookRouter{
		store:  ts,
		global: global,
		client: webhook.SecureClient(10 * time.Second),
		sem:    make(chan struct{}, maxConcurrentDeliveries),
	}
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
	// Deliver to a gateway's endpoints concurrently (bounded by the global semaphore)
	// so one slow endpoint never delays the others, and the whole fan-out is capped by
	// ctx so it can't outlive a reasonable window.
	var wg sync.WaitGroup
	for _, ep := range eps {
		select {
		case r.sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func(ep store.WebhookEndpoint) {
			defer wg.Done()
			defer func() { <-r.sem }()
			// webhook.New with the shared (redirect- and private-IP-refusing) client
			// signs with this endpoint's own secret and retries with bounded backoff.
			ok := webhook.New(ep.URL, ep.Secret, r.client).DeliverSyncContext(ctx, inv)
			if err := r.store.RecordWebhookDelivery(ctx, inv.ID, ep.ID, 1, 0, ok); err != nil {
				log.Printf("webhook: record delivery inv=%s ep=%s: %v", inv.ID, ep.ID, err)
			}
		}(ep)
	}
	wg.Wait()
}

// Wait drains in-flight fan-outs on shutdown.
func (r *WebhookRouter) Wait() { r.wg.Wait() }
