package httpx

import (
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"
)

// readyz reports whether the service can serve traffic: the process is up AND (when
// backed by Postgres) the database is reachable. A reverse proxy / orchestrator
// points its health check here so a replica with a broken DB is pulled from
// rotation instead of silently failing requests. Always unauthenticated.
func (a *api) readyz(w http.ResponseWriter, r *http.Request) {
	if a.s.DB != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := a.s.DB.Ping(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ready": false, "error": "database unreachable"})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ready": true})
}

// metrics exposes operational counters in Prometheus text format so we can SEE
// load: pending-invoice depth (the poller's backlog), DB pool saturation (the API
// tier's first limit), and goroutine count (webhook/poller pile-ups). It is gated
// by the admin bearer token; with no TON_ADMIN_TOKEN configured it is disabled
// (404) so a public host never leaks operational data.
func (a *api) metrics(w http.ResponseWriter, r *http.Request) {
	tok := a.s.Cfg.AdminToken
	if tok == "" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if !bearerEquals(r, tok) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var b strings.Builder
	g := func(name, help string, v int64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", name, help, name, name, v)
	}

	g("tonpayment_pending_invoices", "Invoices awaiting on-chain payment.", int64(len(a.s.Service.ListPending())))
	if a.s.DB != nil {
		st := a.s.DB.Stat()
		g("tonpayment_db_conns_total", "Open Postgres connections in the pool.", int64(st.TotalConns()))
		g("tonpayment_db_conns_acquired", "Connections currently checked out.", int64(st.AcquiredConns()))
		g("tonpayment_db_conns_idle", "Idle connections in the pool.", int64(st.IdleConns()))
		g("tonpayment_db_conns_max", "Configured pool ceiling.", int64(st.MaxConns()))
		g("tonpayment_db_acquire_total", "Total successful pool acquires.", st.AcquireCount())
		g("tonpayment_db_acquire_empty_total", "Acquires that had to wait for an empty pool.", st.EmptyAcquireCount())
		g("tonpayment_db_canceled_acquire_total", "Acquires canceled before completing.", st.CanceledAcquireCount())
	}
	g("tonpayment_goroutines", "Live goroutines.", int64(runtime.NumGoroutine()))

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = io.WriteString(w, b.String())
}

// bearerEquals reports whether the request carries Authorization: Bearer <want>,
// compared in constant time so the token can't be recovered by timing.
func bearerEquals(r *http.Request, want string) bool {
	const p = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, p) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(h[len(p):]), []byte(want)) == 1
}
