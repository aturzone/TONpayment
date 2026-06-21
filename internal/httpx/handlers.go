package httpx

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/aturzone/TONpayment/internal/auth"
	"github.com/aturzone/TONpayment/internal/deeplink"
	"github.com/aturzone/TONpayment/internal/money"
	"github.com/aturzone/TONpayment/internal/store"
)

// invoiceView is the public JSON shape of an invoice: the stored fields plus the
// derived deeplink, a QR endpoint, and a human-readable TON amount.
func (a *api) invoiceView(inv store.Invoice) map[string]any {
	md := inv.Metadata
	if md == nil {
		md = map[string]string{}
	}
	return map[string]any{
		"id":         inv.ID,
		"status":     inv.Status,
		"payTo":      inv.PayTo,
		"memo":       inv.Memo,
		"amountNano": inv.AmountNano,
		"amount":     money.NanoToTONString(inv.AmountNano),
		"currency":   inv.Currency,
		"txHash":     inv.TxHash,
		"metadata":   md,
		"deeplink":   deeplink.Build(inv.PayTo, inv.AmountNano, inv.Memo),
		"qr":         "/v1/invoices/" + inv.ID + "/qr",
		"createdAt":  inv.CreatedAt,
		"paidAt":     inv.PaidAt,
		"expiresAt":  inv.ExpiresAt,
	}
}

// createInvoice creates a pending invoice and returns it with a payment deeplink.
// Single-tenant body: { "amountNano": <int>, "ttlSeconds"?: <int>, "payTo"?,
// "metadata"? }. Multi-tenant (merchant API key): the body must name a "gatewayId"
// the merchant owns; the invoice is paid to that gateway's receiving wallet.
func (a *api) createInvoice(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r) // set by the auth wrapper
	var in struct {
		PayTo      string            `json:"payTo"`
		AmountNano int64             `json:"amountNano"`
		TTLSeconds int               `json:"ttlSeconds"`
		Metadata   map[string]string `json:"metadata"`
		GatewayID  string            `json:"gatewayId"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	ttl := time.Duration(in.TTLSeconds) * time.Second

	var inv store.Invoice
	var err error
	if p.MerchantID != "" {
		// Multi-tenant: resolve the gateway and confirm the caller owns it.
		if in.GatewayID == "" {
			writeError(w, http.StatusBadRequest, "gatewayId is required")
			return
		}
		g, ok, gerr := a.s.Tenant.GetGateway(r.Context(), in.GatewayID)
		if gerr != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !ok || g.MerchantID != p.MerchantID {
			writeError(w, http.StatusNotFound, "gateway not found")
			return
		}
		if !g.Active {
			writeError(w, http.StatusBadRequest, "gateway is inactive")
			return
		}
		inv, err = a.s.Service.CreateInvoiceForGateway(g, in.AmountNano, ttl, in.Metadata)
	} else {
		inv, err = a.s.Service.CreateInvoice(in.PayTo, in.AmountNano, ttl, in.Metadata)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, a.invoiceView(inv))
}

func (a *api) getInvoice(w http.ResponseWriter, r *http.Request) {
	inv, ok := a.lookup(r, r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, a.invoiceView(inv))
}

// invoiceStatus triggers an on-demand verify+settle, then returns the invoice. In
// multi-tenant mode it scope-checks ownership first, so a cross-tenant id is a 404
// rather than a toncenter round-trip.
func (a *api) invoiceStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := a.lookup(r, id); !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	inv, err := a.s.Service.CheckStatus(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, a.invoiceView(inv))
}

// invoiceQR renders the invoice's payment deeplink as a QR-code PNG. Optional
// ?size=<px> (64..1024).
func (a *api) invoiceQR(w http.ResponseWriter, r *http.Request) {
	inv, ok := a.lookup(r, r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	size, _ := strconv.Atoi(r.URL.Query().Get("size"))
	png, err := deeplink.PNG(deeplink.Build(inv.PayTo, inv.AmountNano, inv.Memo), size)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "qr render failed")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

// principalKey carries the authenticated Principal through the request context.
type principalKey struct{}

func withPrincipal(r *http.Request, p auth.Principal) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), principalKey{}, p))
}

func principalOf(r *http.Request) auth.Principal {
	p, _ := r.Context().Value(principalKey{}).(auth.Principal)
	return p
}

// lookup fetches an invoice, scoped to the caller's merchant in multi-tenant mode:
// a merchant principal only ever sees its own invoices (a cross-tenant or unknown
// id is indistinguishably "not found", closing IDOR on the shared id space). In
// single-tenant mode (empty MerchantID) it is the plain store read, as before.
func (a *api) lookup(r *http.Request, id string) (store.Invoice, bool) {
	if p := principalOf(r); p.MerchantID != "" && a.s.Tenant != nil {
		inv, ok, err := a.s.Tenant.GetInvoiceForMerchant(r.Context(), id, p.MerchantID)
		if err != nil || !ok {
			return store.Invoice{}, false
		}
		return inv, true
	}
	return a.s.Service.Get(id)
}
