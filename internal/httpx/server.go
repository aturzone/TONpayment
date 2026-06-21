// Package httpx wires the HTTP API: router, middleware, and invoice handlers.
package httpx

import (
	"net/http"
	"time"

	"github.com/aturzone/TONpayment/internal/auth"
	"github.com/aturzone/TONpayment/internal/config"
	"github.com/aturzone/TONpayment/internal/service"
	"github.com/aturzone/TONpayment/internal/store"
	"github.com/aturzone/TONpayment/internal/tenant"
)

// Services holds the dependencies the handlers need. The multi-tenant fields are
// nil in single-tenant / OSS mode, where the server behaves exactly as before.
type Services struct {
	Cfg     *config.Config
	Service *service.Service

	// Multi-tenant (all nil/zero in single-tenant mode):
	Auth          auth.Authenticator // data-plane authenticator; defaults to SingleKeyAuth
	AuthSvc       *auth.Service      // ton_proof sign-in (challenge/verify)
	Tenant        store.TenantStore  // tenant reads (scoped invoices, gateways, fee)
	TenantSvc     *tenant.Service    // control-plane writes (gateways, keys, webhooks)
	SessionSecret []byte             // verifies merchant session tokens
}

type api struct {
	s       Services
	limiter *rateLimiter
}

// NewServer builds the configured *http.Server.
func NewServer(s Services) *http.Server {
	// Default the data-plane authenticator to the single-key gate, preserving exact
	// single-tenant behavior when no multi-tenant Authenticator is injected.
	if s.Auth == nil {
		s.Auth = auth.SingleKeyAuth{Key: s.Cfg.CreateAPIKey}
	}
	a := &api{s: s, limiter: newRateLimiter(5, 40, s.Cfg.TrustProxy)}

	mux := http.NewServeMux()
	a.routes(mux)

	handler := chain(mux,
		recoverMW,
		requestID,
		logger,
		securityHeaders(s.Cfg.IsProd()),
		cors(s.Cfg.AllowedOrigins),
	)

	return &http.Server{
		Addr:              s.Cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

func (a *api) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", a.healthz)

	// Every /v1/invoices endpoint shares the same auth gate: an invoice ID is an
	// unguessable token, not an authorization, so reads are gated too — no
	// unauthenticated IDOR on metadata and no unauthenticated, status-triggered
	// toncenter round-trips. Create + status are also rate-limited because they do
	// real work. The auth wrapper resolves the Principal (single-key -> empty
	// principal; per-merchant key -> merchant scope) and stashes it for the handler.
	mux.Handle("POST /v1/invoices", a.limiter.wrap(a.auth(a.createInvoice)))
	mux.Handle("GET /v1/invoices/{id}", a.auth(a.getInvoice))
	mux.Handle("GET /v1/invoices/{id}/status", a.limiter.wrap(a.auth(a.invoiceStatus)))
	mux.Handle("GET /v1/invoices/{id}/qr", a.auth(a.invoiceQR))

	// Multi-tenant control plane + public checkout endpoints, only when enabled.
	if a.s.AuthSvc != nil {
		a.mtRoutes(mux)
	}
}

// auth wraps a handler so it authenticates via the configured Authenticator and
// stashes the resulting Principal in the request context. In single-tenant mode
// SingleKeyAuth yields an empty Principal (open when no key is set, or a
// constant-time key check) — exactly today's behavior.
func (a *api) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := a.s.Auth.Authenticate(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "invalid or missing API key")
			return
		}
		next(w, withPrincipal(r, p))
	}
}

func (a *api) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"service": "tonpayment",
		"env":     a.s.Cfg.Env,
	})
}
