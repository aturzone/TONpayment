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
	a := &api{s: s, limiter: newRateLimiter(5, 40)}

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

	// Create is rate-limited and (optionally) API-key gated. Status triggers an
	// on-demand verify+settle, so it is rate-limited too. Reads (get, qr) are cheap.
	mux.Handle("POST /v1/invoices", a.limiter.wrap(http.HandlerFunc(a.createInvoice)))
	mux.HandleFunc("GET /v1/invoices/{id}", a.getInvoice)
	mux.Handle("GET /v1/invoices/{id}/status", a.limiter.wrap(http.HandlerFunc(a.invoiceStatus)))
	mux.HandleFunc("GET /v1/invoices/{id}/qr", a.invoiceQR)
}

func (a *api) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"service": "tonpayment",
		"env":     a.s.Cfg.Env,
	})
}
