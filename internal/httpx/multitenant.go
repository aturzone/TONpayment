package httpx

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aturzone/TONpayment/internal/auth"
	"github.com/aturzone/TONpayment/internal/idgen"
	"github.com/aturzone/TONpayment/internal/store"
	"github.com/aturzone/TONpayment/internal/tenant"
	"github.com/aturzone/TONpayment/internal/webhook"
)

// mtRoutes registers the multi-tenant control plane + public checkout endpoints.
// Called only when multi-tenant mode is enabled (Services.AuthSvc != nil).
func (a *api) mtRoutes(mux *http.ServeMux) {
	// Public (no auth): the hosted checkout + public link pages read these.
	mux.Handle("GET /v1/fee", a.limiter.wrap(http.HandlerFunc(a.getFee)))
	mux.Handle("GET /v1/checkout/{id}", a.limiter.wrap(http.HandlerFunc(a.checkout)))
	mux.Handle("GET /v1/link/{slug}", a.limiter.wrap(http.HandlerFunc(a.publicLink)))
	mux.Handle("POST /v1/donate/{slug}", a.limiter.wrap(http.HandlerFunc(a.donate)))
	mux.Handle("GET /v1/asset/{id}", a.limiter.wrap(http.HandlerFunc(a.getAsset)))

	// ton_proof sign-in.
	mux.Handle("POST /v1/auth/challenge", a.limiter.wrap(http.HandlerFunc(a.authChallenge)))
	mux.Handle("POST /v1/auth/verify", a.limiter.wrap(http.HandlerFunc(a.authVerify)))

	// Merchant control plane (session-authenticated).
	mux.Handle("GET /v1/me", a.session(a.me))
	mux.Handle("POST /v1/gateways", a.session(a.createGateway))
	mux.Handle("GET /v1/gateways", a.session(a.listGateways))
	mux.Handle("GET /v1/gateways/{id}", a.session(a.getGateway))
	mux.Handle("PATCH /v1/gateways/{id}", a.session(a.updateGateway))
	mux.Handle("DELETE /v1/gateways/{id}", a.session(a.deleteGateway))
	mux.Handle("POST /v1/gateways/{id}/webhooks", a.session(a.createWebhook))
	mux.Handle("GET /v1/gateways/{id}/webhooks", a.session(a.listWebhooks))
	mux.Handle("DELETE /v1/gateways/{id}/webhooks/{whid}", a.session(a.deleteWebhook))
	mux.Handle("POST /v1/keys", a.session(a.createKey))
	mux.Handle("GET /v1/keys", a.session(a.listKeys))
	mux.Handle("DELETE /v1/keys/{id}", a.session(a.revokeKey))
	mux.Handle("GET /v1/invoices", a.session(a.listInvoices))
	mux.Handle("POST /v1/assets", a.session(a.uploadAsset))
}

// session wraps a control-plane handler so it requires a valid merchant session
// token (Authorization: Bearer <token>) and passes the verified claims through.
func (a *api) session(next func(http.ResponseWriter, *http.Request, auth.Claims)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := auth.ParseSession(a.s.SessionSecret, bearerToken(r), time.Now())
		if !ok {
			writeError(w, http.StatusUnauthorized, "invalid or expired session")
			return
		}
		next(w, r, claims)
	}
}

func bearerToken(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// decodeBody is decodeJSON that tolerates an empty body (returns nil), for handlers
// whose fields are all optional.
func decodeBody(r *http.Request, v any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	return decodeJSON(r, v)
}

// --- ton_proof sign-in ---

func (a *api) authChallenge(w http.ResponseWriter, r *http.Request) {
	nonce, err := a.s.AuthSvc.Challenge(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not issue challenge")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"payload": nonce})
}

func (a *api) authVerify(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Address   string `json:"address"`
		PublicKey string `json:"publicKey"`
		Proof     struct {
			Timestamp int64                  `json:"timestamp"`
			Domain    struct{ Value string } `json:"domain"`
			Payload   string                 `json:"payload"`
			Signature string                 `json:"signature"`
		} `json:"proof"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	sig, err := decodeSignature(in.Proof.Signature)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid signature encoding")
		return
	}
	pub, _ := hex.DecodeString(strings.TrimPrefix(in.PublicKey, "0x")) // optional; nil if absent/bad
	token, m, err := a.s.AuthSvc.Verify(r.Context(), auth.VerifyInput{
		Address:   in.Address,
		PublicKey: pub,
		Proof: auth.Proof{
			Timestamp: in.Proof.Timestamp,
			Domain:    in.Proof.Domain.Value,
			Payload:   in.Proof.Payload,
			Signature: sig,
		},
	})
	if err != nil {
		writeError(w, authStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "merchantId": m.ID, "wallet": m.Wallet})
}

// decodeSignature accepts a ton_proof signature in standard or url-safe base64.
func decodeSignature(s string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawURLEncoding.DecodeString(s)
}

// authStatus maps verify errors to HTTP statuses: a not-deployed wallet is a 400
// (actionable client error); the rest are auth failures (401).
func authStatus(err error) int {
	if errors.Is(err, auth.ErrNoPubKey) {
		return http.StatusBadRequest
	}
	return http.StatusUnauthorized
}

// --- merchant control plane ---

func (a *api) me(w http.ResponseWriter, r *http.Request, claims auth.Claims) {
	m, ok, err := a.s.Tenant.GetMerchant(r.Context(), claims.MerchantID)
	if err != nil || !ok {
		writeError(w, http.StatusNotFound, "merchant not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"merchant": m, "admin": claims.Admin})
}

func (a *api) createGateway(w http.ResponseWriter, r *http.Request, claims auth.Claims) {
	var in struct {
		Kind             string         `json:"kind"`
		Slug             string         `json:"slug"`
		DisplayName      string         `json:"displayName"`
		Branding         map[string]any `json:"branding"`
		Contact          map[string]any `json:"contact"`
		ReceivingAddress string         `json:"receivingAddress"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// Default the receiving address to the merchant's own (login) wallet.
	addr := in.ReceivingAddress
	if addr == "" {
		addr = claims.Wallet
	}
	g, err := a.s.TenantSvc.CreateGateway(r.Context(), tenant.CreateGatewayInput{
		MerchantID:       claims.MerchantID,
		Kind:             in.Kind,
		Slug:             in.Slug,
		DisplayName:      in.DisplayName,
		Branding:         in.Branding,
		Contact:          in.Contact,
		ReceivingAddress: addr,
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrWalletTaken):
			writeError(w, http.StatusConflict, "this wallet already hosts a donation link or a payment gateway; use a different receiving wallet")
		case errors.Is(err, tenant.ErrSlugTaken):
			writeError(w, http.StatusConflict, "that gateway slug is taken")
		case errors.Is(err, tenant.ErrSlugEmpty):
			writeError(w, http.StatusBadRequest, "a gateway slug is required")
		default:
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusCreated, g)
}

func (a *api) listGateways(w http.ResponseWriter, r *http.Request, claims auth.Claims) {
	gws, err := a.s.Tenant.ListGatewaysByMerchant(r.Context(), claims.MerchantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"gateways": gws})
}

// ownedGateway loads a gateway and verifies the session merchant owns it.
func (a *api) ownedGateway(r *http.Request, id, merchantID string) (store.Gateway, bool) {
	g, ok, err := a.s.Tenant.GetGateway(r.Context(), id)
	if err != nil || !ok || g.MerchantID != merchantID {
		return store.Gateway{}, false
	}
	return g, true
}

func (a *api) getGateway(w http.ResponseWriter, r *http.Request, claims auth.Claims) {
	g, ok := a.ownedGateway(r, r.PathValue("id"), claims.MerchantID)
	if !ok {
		writeError(w, http.StatusNotFound, "gateway not found")
		return
	}
	writeJSON(w, http.StatusOK, g)
}

func (a *api) updateGateway(w http.ResponseWriter, r *http.Request, claims auth.Claims) {
	g, ok := a.ownedGateway(r, r.PathValue("id"), claims.MerchantID)
	if !ok {
		writeError(w, http.StatusNotFound, "gateway not found")
		return
	}
	var in struct {
		Slug        *string        `json:"slug"`
		Kind        *string        `json:"kind"`
		DisplayName *string        `json:"displayName"`
		Branding    map[string]any `json:"branding"`
		Contact     map[string]any `json:"contact"`
		Active      *bool          `json:"active"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	updated, err := a.s.TenantSvc.UpdateGateway(r.Context(), g, tenant.UpdateGatewayPatch{
		Slug:        in.Slug,
		Kind:        in.Kind,
		DisplayName: in.DisplayName,
		Branding:    in.Branding,
		Contact:     in.Contact,
		Active:      in.Active,
	})
	if err != nil {
		switch {
		case errors.Is(err, tenant.ErrSlugTaken):
			writeError(w, http.StatusConflict, "that gateway slug is taken")
		case errors.Is(err, tenant.ErrSlugEmpty):
			writeError(w, http.StatusBadRequest, "a gateway slug is required")
		default:
			writeError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// deleteGateway removes a link and frees its receiving wallet (so the merchant can
// create a new link, of either kind, on the same wallet). Webhook endpoints cascade;
// historical invoices are preserved.
func (a *api) deleteGateway(w http.ResponseWriter, r *http.Request, claims auth.Claims) {
	g, ok := a.ownedGateway(r, r.PathValue("id"), claims.MerchantID)
	if !ok {
		writeError(w, http.StatusNotFound, "gateway not found")
		return
	}
	if err := a.s.TenantSvc.DeleteGateway(r.Context(), g); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (a *api) createWebhook(w http.ResponseWriter, r *http.Request, claims auth.Claims) {
	g, ok := a.ownedGateway(r, r.PathValue("id"), claims.MerchantID)
	if !ok {
		writeError(w, http.StatusNotFound, "gateway not found")
		return
	}
	var in struct {
		URL    string `json:"url"`
		Secret string `json:"secret"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// Reject internal/loopback/private targets up front (delivery additionally refuses
	// to dial non-public IPs, defeating DNS rebinding). Require https in production.
	if err := webhook.ValidateURL(in.URL, a.s.Cfg.IsProd()); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	secret := in.Secret
	if secret == "" {
		secret = randSecret()
	}
	e := store.WebhookEndpoint{ID: idgen.New("whk"), GatewayID: g.ID, URL: in.URL, Secret: secret, Active: true}
	if err := a.s.Tenant.CreateWebhookEndpoint(r.Context(), e); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Return the secret once so the merchant can configure their receiver.
	writeJSON(w, http.StatusCreated, map[string]any{"id": e.ID, "gatewayId": e.GatewayID, "url": e.URL, "secret": secret, "active": true})
}

func (a *api) listWebhooks(w http.ResponseWriter, r *http.Request, claims auth.Claims) {
	g, ok := a.ownedGateway(r, r.PathValue("id"), claims.MerchantID)
	if !ok {
		writeError(w, http.StatusNotFound, "gateway not found")
		return
	}
	eps, err := a.s.Tenant.ListWebhookEndpoints(r.Context(), g.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"webhooks": eps}) // secrets are json:"-"
}

// deleteWebhook removes one endpoint from a gateway the session merchant owns. The
// store scopes the delete to gateway_id, so an id from another gateway no-ops.
func (a *api) deleteWebhook(w http.ResponseWriter, r *http.Request, claims auth.Claims) {
	g, ok := a.ownedGateway(r, r.PathValue("id"), claims.MerchantID)
	if !ok {
		writeError(w, http.StatusNotFound, "gateway not found")
		return
	}
	if err := a.s.Tenant.DeleteWebhookEndpoint(r.Context(), r.PathValue("whid"), g.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (a *api) createKey(w http.ResponseWriter, r *http.Request, claims auth.Claims) {
	var in struct {
		Mode  string `json:"mode"`
		Label string `json:"label"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	issued, err := a.s.TenantSvc.IssueAPIKey(r.Context(), claims.MerchantID, in.Mode, in.Label)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	// The raw key is shown exactly once.
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": issued.Key.ID, "key": issued.Raw, "keyPrefix": issued.Key.KeyPrefix,
		"mode": issued.Key.Mode, "label": issued.Key.Label, "createdAt": issued.Key.CreatedAt,
	})
}

func (a *api) listKeys(w http.ResponseWriter, r *http.Request, claims auth.Claims) {
	keys, err := a.s.Tenant.ListAPIKeysByMerchant(r.Context(), claims.MerchantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys}) // KeyHash is json:"-"
}

func (a *api) revokeKey(w http.ResponseWriter, r *http.Request, claims auth.Claims) {
	id := r.PathValue("id")
	keys, err := a.s.Tenant.ListAPIKeysByMerchant(r.Context(), claims.MerchantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	owned := false
	for i := range keys {
		if keys[i].ID == id {
			owned = true
			break
		}
	}
	if !owned {
		writeError(w, http.StatusNotFound, "key not found")
		return
	}
	if err := a.s.Tenant.RevokeAPIKey(r.Context(), id, time.Now().UTC()); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": true})
}

func (a *api) listInvoices(w http.ResponseWriter, r *http.Request, claims auth.Claims) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	invs, err := a.s.Tenant.ListInvoicesByMerchant(r.Context(), claims.MerchantID, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]map[string]any, 0, len(invs))
	for _, inv := range invs {
		out = append(out, a.invoiceView(inv))
	}
	writeJSON(w, http.StatusOK, map[string]any{"invoices": out})
}

// --- public checkout ---

func (a *api) getFee(w http.ResponseWriter, r *http.Request) {
	cfg, err := a.s.Tenant.GetPlatformConfig(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"feeBps": cfg.FeeBps, "feeWallet": cfg.FeeWallet})
}

// checkout returns everything the hosted checkout page needs for an invoice: the
// public invoice view, its gateway's branding, and the platform fee. It is PUBLIC
// by design — the invoice id is the capability the merchant shares with the payer
// (the one place an id legitimately authorizes a read). No secrets are exposed.
func (a *api) checkout(w http.ResponseWriter, r *http.Request) {
	inv, ok := a.s.Service.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	resp := map[string]any{"invoice": a.invoiceView(inv)}
	if inv.GatewayID != "" {
		if g, ok, _ := a.s.Tenant.GetGateway(r.Context(), inv.GatewayID); ok {
			resp["gateway"] = publicGatewayView(g)
		}
	}
	if cfg, err := a.s.Tenant.GetPlatformConfig(r.Context()); err == nil {
		resp["fee"] = map[string]any{"feeBps": cfg.FeeBps, "feeWallet": cfg.FeeWallet}
	}
	writeJSON(w, http.StatusOK, resp)
}

// publicGatewayView is the public-safe projection of a link: display + branding +
// kind only. It deliberately omits Contact (PII) and never appears behind a
// non-public route's response either.
func publicGatewayView(g store.Gateway) map[string]any {
	return map[string]any{
		"slug":        g.Slug,
		"displayName": g.DisplayName,
		"branding":    g.Branding,
		"kind":        g.Kind,
		"active":      g.Active,
	}
}

// publicLink returns a link's public view by slug, for its public page (donation
// tipping page or a payment gateway's landing). No auth, no PII.
func (a *api) publicLink(w http.ResponseWriter, r *http.Request) {
	g, ok, err := a.s.Tenant.GetGatewayBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, publicGatewayView(g))
}

// donate creates a tip invoice for a donation link (public, rate-limited, bounded
// by the engine's pending caps) so donations are invoice-tracked and fee'd exactly
// like payments. Only donation-kind, active links accept it.
func (a *api) donate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AmountNano int64             `json:"amountNano"`
		Metadata   map[string]string `json:"metadata"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	g, ok, err := a.s.Tenant.GetGatewayBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !ok || g.Kind != store.ProductDonation || !g.Active {
		writeError(w, http.StatusNotFound, "donation link not found")
		return
	}
	inv, err := a.s.Service.CreateInvoiceForGateway(g, in.AmountNano, 0, in.Metadata)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, a.invoiceView(inv))
}

// maxImageBytes bounds an uploaded link image; maxAssetsPerMerchant bounds how many
// a single merchant may store (uploaded bytes live in the ledger Postgres).
const (
	maxImageBytes        = 256 * 1024
	maxAssetsPerMerchant = 20
)

// Raster only — SVG is excluded deliberately (a stored SVG could carry script and
// run in our origin if opened directly).
var allowedImageTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
	"image/gif":  true,
}

// uploadAsset stores a small link image on our own server (Postgres) and returns
// its URL. The body is the raw image; Content-Type must be a supported raster type
// and the size is capped.
func (a *api) uploadAsset(w http.ResponseWriter, r *http.Request, claims auth.Claims) {
	ct := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if !allowedImageTypes[ct] {
		writeError(w, http.StatusUnsupportedMediaType, "image must be PNG, JPEG, WebP or GIF")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxImageBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "image too large (max 256 KB)")
		return
	}
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "empty image")
		return
	}
	// Trust the bytes, not the header: a declared type that doesn't actually sniff as
	// an allowed raster image is rejected, and we store the SNIFFED type so a disguised
	// payload can never be served back under an attacker-chosen content-type.
	sniff := http.DetectContentType(body)
	if i := strings.IndexByte(sniff, ';'); i >= 0 {
		sniff = strings.TrimSpace(sniff[:i])
	}
	if !allowedImageTypes[sniff] {
		writeError(w, http.StatusUnsupportedMediaType, "file content is not a supported image (PNG, JPEG, WebP or GIF)")
		return
	}
	ct = sniff
	// Bound how many images this merchant may keep in the ledger DB.
	if n, err := a.s.Tenant.CountAssetsByMerchant(r.Context(), claims.MerchantID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	} else if n >= maxAssetsPerMerchant {
		writeError(w, http.StatusConflict, "image limit reached; reuse or remove an existing image")
		return
	}
	id := idgen.New("img")
	if err := a.s.Tenant.CreateAsset(r.Context(), store.Asset{ID: id, MerchantID: claims.MerchantID, ContentType: ct, Bytes: body}); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"url": "/v1/asset/" + id})
}

// getAsset serves a stored image (public, long-cached). A restrictive CSP + nosniff
// neutralizes any active content even though only raster types are accepted.
func (a *api) getAsset(w http.ResponseWriter, r *http.Request) {
	as, ok, err := a.s.Tenant.GetAsset(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	h := w.Header()
	h.Set("Content-Type", as.ContentType)
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'")
	_, _ = w.Write(as.Bytes)
}

func randSecret() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic("httpx: crypto/rand failed: " + err.Error())
	}
	return "whsec_" + base64.RawURLEncoding.EncodeToString(b)
}
