// Package httpx wires the HTTP API: router, middleware, and invoice handlers.
package httpx

import (
	"net/http"
	"time"

	"github.com/aturzone/TONpayment/internal/config"
	"github.com/aturzone/TONpayment/internal/service"
)

// Services holds the dependencies the handlers need.
type Services struct {
	Cfg     *config.Config
	Service *service.Service
}

type api struct {
	s       Services
	limiter *rateLimiter
}

// NewServer builds the configured *http.Server.
func NewServer(s Services) *http.Server {
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

	// Every /v1/invoices endpoint shares the same API-key gate (when a key is
	// configured): an invoice ID is an unguessable token, not an authorization,
	// so reads are gated too — no unauthenticated IDOR on metadata and no
	// unauthenticated, status-triggered toncenter round-trips. Create + status
	// are also rate-limited because they do real work.
	mux.Handle("POST /v1/invoices", a.limiter.wrap(http.HandlerFunc(a.createInvoice)))
	mux.Handle("GET /v1/invoices/{id}", a.auth(a.getInvoice))
	mux.Handle("GET /v1/invoices/{id}/status", a.limiter.wrap(a.auth(a.invoiceStatus)))
	mux.Handle("GET /v1/invoices/{id}/qr", a.auth(a.invoiceQR))
}

// auth wraps a handler so it requires the API key whenever one is configured
// (the same constant-time gate as create). With no key set the endpoint stays
// open (dev / single-tenant).
func (a *api) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.authorized(r) {
			writeError(w, http.StatusUnauthorized, "invalid or missing API key")
			return
		}
		next(w, r)
	}
}

func (a *api) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"service": "tonpayment",
		"env":     a.s.Cfg.Env,
	})
}
