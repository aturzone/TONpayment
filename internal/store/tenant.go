package store

import (
	"context"
	"errors"
	"time"
)

// This file defines the multi-tenant control plane: the merchants, gateways, API
// keys, webhook endpoints, wallet-ownership registry, platform config, audit log
// and ton_proof challenges that turn the single-tenant engine into a hosted
// gateway platform.
//
// TenantStore is a SUPERSET added alongside Store, not a replacement: the base
// Store interface (store.go) is untouched, single-tenant mode never references any
// of this, and only *Postgres implements TenantStore. Multi-tenant mode therefore
// requires Postgres (the in-memory/JSON store is intentionally left out, mirroring
// the existing prod file-store gate).

// Product is the kind of thing a wallet is registered to. A wallet may belong to
// exactly one product (enforced by wallet_ownership.address being a primary key),
// so a wallet running a donation link can never also run a payment gateway.
const (
	ProductDonation = "donation"
	ProductPayment  = "payment"
)

// ErrWalletTaken is returned by ClaimWallet when the address is already registered
// — to another product, or to another owner of the same product. It is the
// concrete signal the control plane turns into an HTTP 409.
var ErrWalletTaken = errors.New("wallet is already registered to a donation link or payment gateway")

// Merchant is an account, identified by the wallet that proved ownership via
// ton_proof. A merchant owns gateways and API keys.
type Merchant struct {
	ID        string    `json:"id"`
	Wallet    string    `json:"wallet"` // tonaddr-normalized
	Status    string    `json:"status"` // "active" | "suspended"
	CreatedAt time.Time `json:"createdAt"`
}

// Merchant statuses.
const (
	MerchantActive    = "active"
	MerchantSuspended = "suspended"
)

// Gateway is one payment/donation link belonging to a merchant. Kind is the link
// type (ProductDonation | ProductPayment) and matches the wallet_ownership product
// for ReceivingAddress, so a wallet hosts EITHER a donation link OR a payment
// gateway, never both. ReceivingAddress is where funds land (non-custodial — the
// platform never holds them). Branding is an opaque blob the web app owns the
// meaning of (logo, banner, colors, …). Contact is optional PII (email, name)
// collected for payment links — unverified, stored, and NEVER returned publicly.
type Gateway struct {
	ID               string         `json:"id"`
	MerchantID       string         `json:"merchantId"`
	Kind             string         `json:"kind"` // ProductDonation | ProductPayment
	Slug             string         `json:"slug"`
	DisplayName      string         `json:"displayName"`
	Branding         map[string]any `json:"branding"`
	Contact          map[string]any `json:"contact,omitempty"`
	ReceivingAddress string         `json:"receivingAddress"`
	Active           bool           `json:"active"`
	CreatedAt        time.Time      `json:"createdAt"`
}

// APIKey authenticates a merchant's data-plane calls. Only the SHA-256 hash of the
// raw key is stored; the raw key is shown once at creation and never again.
type APIKey struct {
	ID         string     `json:"id"`
	MerchantID string     `json:"merchantId"`
	KeyHash    []byte     `json:"-"`
	KeyPrefix  string     `json:"keyPrefix"` // display hint, e.g. "pk_live_3Fa9"
	Mode       string     `json:"mode"`      // "live" | "test"
	Label      string     `json:"label"`
	CreatedAt  time.Time  `json:"createdAt"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
}

// API key modes.
const (
	KeyModeLive = "live"
	KeyModeTest = "test"
)

// WebhookEndpoint is a per-gateway callback. Each settled invoice is POSTed to its
// gateway's active endpoints, signed with that endpoint's secret (HMAC-SHA256).
type WebhookEndpoint struct {
	ID        string    `json:"id"`
	GatewayID string    `json:"gatewayId"`
	URL       string    `json:"url"`
	Secret    string    `json:"-"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"createdAt"`
}

// WalletOwner is one row of the cross-product registry: which product (and owner)
// has claimed a given wallet address.
type WalletOwner struct {
	Address   string    `json:"address"`
	Product   string    `json:"product"` // ProductDonation | ProductPayment
	OwnerID   string    `json:"ownerId"`
	CreatedAt time.Time `json:"createdAt"`
}

// PlatformConfig holds the runtime-configurable platform fee. FeeBps is basis
// points (100 = 1%); FeeWallet is where the fee message is sent. Both default to
// zero/empty so a self-hosted engine charges nothing until an operator sets them.
type PlatformConfig struct {
	FeeBps    int       `json:"feeBps"`
	FeeWallet string    `json:"feeWallet"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Asset is a small uploaded image (a link's logo) stored on our own server in
// Postgres and served at /v1/asset/{id}. Size is bounded by the upload handler.
type Asset struct {
	ID          string
	MerchantID  string
	ContentType string
	Bytes       []byte
	CreatedAt   time.Time
}

// TenantStore is the persistence contract for the multi-tenant control plane.
// Implementations must be safe for concurrent use. Every method takes a context so
// callers (HTTP handlers) can bound latency; implementations additionally cap each
// query with the store's own timeout.
type TenantStore interface {
	// --- merchants ---
	CreateMerchant(ctx context.Context, m Merchant) error
	GetMerchant(ctx context.Context, id string) (Merchant, bool, error)
	GetMerchantByWallet(ctx context.Context, wallet string) (Merchant, bool, error)
	ListMerchants(ctx context.Context, limit, offset int) ([]Merchant, error)
	SetMerchantStatus(ctx context.Context, id, status string) error

	// --- gateways ---
	CreateGateway(ctx context.Context, g Gateway) error
	GetGateway(ctx context.Context, id string) (Gateway, bool, error)
	GetGatewayBySlug(ctx context.Context, slug string) (Gateway, bool, error)
	ListGatewaysByMerchant(ctx context.Context, merchantID string) ([]Gateway, error)
	// UpdateGateway updates the mutable, display-only fields (display_name,
	// branding, active). The receiving address is immutable after creation because
	// it is bound to a wallet_ownership claim.
	UpdateGateway(ctx context.Context, g Gateway) error

	// --- api keys ---
	CreateAPIKey(ctx context.Context, k APIKey) error
	// GetAPIKeyByHash is the auth hot path: one indexed lookup per data-plane
	// request. Returns the key even if revoked; the caller checks RevokedAt.
	GetAPIKeyByHash(ctx context.Context, hash []byte) (APIKey, bool, error)
	ListAPIKeysByMerchant(ctx context.Context, merchantID string) ([]APIKey, error)
	RevokeAPIKey(ctx context.Context, id string, at time.Time) error

	// --- webhooks ---
	CreateWebhookEndpoint(ctx context.Context, e WebhookEndpoint) error
	ListWebhookEndpoints(ctx context.Context, gatewayID string) ([]WebhookEndpoint, error)
	ListActiveWebhookEndpoints(ctx context.Context, gatewayID string) ([]WebhookEndpoint, error)
	RecordWebhookDelivery(ctx context.Context, invoiceID, endpointID string, attempt, statusCode int, ok bool) error

	// --- tenant-scoped invoice reads (defense-in-depth against cross-tenant access) ---
	GetInvoiceForMerchant(ctx context.Context, id, merchantID string) (Invoice, bool, error)
	ListInvoicesByMerchant(ctx context.Context, merchantID string, limit, offset int) ([]Invoice, error)

	// --- wallet ownership (cross-product uniqueness) ---
	GetWalletOwner(ctx context.Context, address string) (WalletOwner, bool, error)
	// ClaimWallet inserts a registration; a primary-key conflict on the address
	// returns ErrWalletTaken (the wallet already belongs to some product).
	ClaimWallet(ctx context.Context, w WalletOwner) error
	ReleaseWallet(ctx context.Context, address, ownerID string) error

	// --- platform config (configurable fee) ---
	GetPlatformConfig(ctx context.Context) (PlatformConfig, error)
	SetPlatformConfig(ctx context.Context, feeBps int, feeWallet string) error

	// --- assets (uploaded link images) ---
	CreateAsset(ctx context.Context, a Asset) error
	GetAsset(ctx context.Context, id string) (Asset, bool, error)
	// CountAssetsByMerchant bounds how many images a single merchant may store in the
	// ledger DB (uploaded bytes live in Postgres), so one account can't bloat it.
	CountAssetsByMerchant(ctx context.Context, merchantID string) (int, error)

	// --- audit + ton_proof challenges ---
	AppendAudit(ctx context.Context, actor, action, target string, meta map[string]any) error
	// PutChallenge stores a single-use nonce valid for ttl.
	PutChallenge(ctx context.Context, nonce string, ttl time.Duration) error
	// ConsumeChallenge atomically marks a nonce used and reports whether THIS call
	// won — an unexpired, previously-unused nonce yields true exactly once, which is
	// the anti-replay guarantee for ton_proof.
	ConsumeChallenge(ctx context.Context, nonce string) (bool, error)
}
