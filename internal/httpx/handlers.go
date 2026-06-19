package httpx

import (
	"crypto/subtle"
	"net/http"
	"strconv"
	"strings"
	"time"

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
// Body: { "amountNano": <int>, "ttlSeconds": <int?>, "metadata": {..}? }.
func (a *api) createInvoice(w http.ResponseWriter, r *http.Request) {
	if !a.authorized(r) {
		writeError(w, http.StatusUnauthorized, "invalid or missing API key")
		return
	}
	var in struct {
		AmountNano int64             `json:"amountNano"`
		TTLSeconds int               `json:"ttlSeconds"`
		Metadata   map[string]string `json:"metadata"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if in.AmountNano <= 0 {
		writeError(w, http.StatusBadRequest, "amountNano must be a positive integer (nanoTON)")
		return
	}
	inv, err := a.s.Service.CreateInvoice(in.AmountNano, time.Duration(in.TTLSeconds)*time.Second, in.Metadata)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, a.invoiceView(inv))
}

func (a *api) getInvoice(w http.ResponseWriter, r *http.Request) {
	inv, ok := a.s.Service.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, a.invoiceView(inv))
}

// invoiceStatus triggers an on-demand verify+settle, then returns the invoice.
func (a *api) invoiceStatus(w http.ResponseWriter, r *http.Request) {
	inv, err := a.s.Service.CheckStatus(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, a.invoiceView(inv))
}

// invoiceQR renders the invoice's payment deeplink as a QR-code PNG. Optional
// ?size=<px> (64..1024).
func (a *api) invoiceQR(w http.ResponseWriter, r *http.Request) {
	inv, ok := a.s.Service.Get(r.PathValue("id"))
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

// authorized reports whether the request may create invoices. If no create key
// is configured the endpoint is open; otherwise the request must present the key
// as "Authorization: Bearer <key>" or "X-API-Key: <key>". The comparison is
// constant-time.
func (a *api) authorized(r *http.Request) bool {
	want := a.s.Cfg.CreateAPIKey
	if want == "" {
		return true
	}
	got := r.Header.Get("X-API-Key")
	if got == "" {
		got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
