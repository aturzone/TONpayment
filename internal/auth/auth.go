// Package auth provides multi-tenant authentication for the gateway platform:
// per-merchant API-key authentication for the data plane, ton_proof wallet
// sign-in for the control plane, and opaque HMAC session tokens. It is pure logic
// (no HTTP server wiring) so the HTTP layer can depend on it without a cycle.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/aturzone/TONpayment/internal/store"
)

// Principal is the authenticated identity behind a data-plane request. MerchantID
// is empty in single-tenant / open mode (no tenant scoping).
type Principal struct {
	MerchantID string
	Mode       string // "live" | "test" | ""
}

// Authenticator decides whether a data-plane request is allowed and, if so, who it
// is. SingleKeyAuth preserves today's behavior; TenantKeyAuth resolves a
// per-merchant key.
type Authenticator interface {
	Authenticate(r *http.Request) (Principal, bool)
}

// ExtractKey pulls an API key from X-API-Key (preferred) or an Authorization
// Bearer token. Factored out of the original single-tenant handler so both
// authenticators share one extraction rule.
func ExtractKey(r *http.Request) string {
	if k := r.Header.Get("X-API-Key"); k != "" {
		return k
	}
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// HashKey is the stored form of an API key: SHA-256 of the raw key. The raw key is
// shown once at creation and never stored; lookups are by this hash. Hashing the
// full key means the DB lookup is not a timing oracle on the secret.
func HashKey(raw string) []byte {
	h := sha256.Sum256([]byte(raw))
	return h[:]
}

// SingleKeyAuth is the single-tenant gate, reproduced exactly: an empty key leaves
// the endpoint open (dev / OSS); otherwise the request must present the configured
// key, compared in constant time. It always yields an empty Principal.
type SingleKeyAuth struct{ Key string }

func (s SingleKeyAuth) Authenticate(r *http.Request) (Principal, bool) {
	if s.Key == "" {
		return Principal{}, true
	}
	got := ExtractKey(r)
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.Key)) == 1 {
		return Principal{}, true
	}
	return Principal{}, false
}

// TenantKeyAuth resolves a per-merchant API key to its owning merchant. A missing
// key, an unknown or revoked key, or a suspended/unknown merchant is denied. A
// store error is treated as a denial (fail-closed).
type TenantKeyAuth struct{ Store store.TenantStore }

func (t TenantKeyAuth) Authenticate(r *http.Request) (Principal, bool) {
	raw := ExtractKey(r)
	if raw == "" {
		return Principal{}, false
	}
	ctx := r.Context()
	k, ok, err := t.Store.GetAPIKeyByHash(ctx, HashKey(raw))
	if err != nil || !ok || k.RevokedAt != nil {
		return Principal{}, false
	}
	m, ok, err := t.Store.GetMerchant(ctx, k.MerchantID)
	if err != nil || !ok || m.Status != store.MerchantActive {
		return Principal{}, false
	}
	return Principal{MerchantID: k.MerchantID, Mode: k.Mode}, true
}
