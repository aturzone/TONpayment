// Package tenant is the multi-tenant control plane: creating gateways (enforcing
// the cross-product wallet-uniqueness rule), issuing per-merchant API keys, and
// routing settlement webhooks to a gateway's own endpoints. It builds on the
// store.TenantStore contract and the existing auth/webhook primitives.
package tenant

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aturzone/TONpayment/internal/auth"
	"github.com/aturzone/TONpayment/internal/idgen"
	"github.com/aturzone/TONpayment/internal/store"
	"github.com/aturzone/TONpayment/internal/tonaddr"
)

// Service is the control plane for merchant gateways and API keys.
type Service struct {
	store   store.TenantStore
	testnet bool
}

func New(ts store.TenantStore, testnet bool) *Service {
	return &Service{store: ts, testnet: testnet}
}

// Store exposes the underlying tenant store for read handlers (lists, lookups).
func (s *Service) Store() store.TenantStore { return s.store }

var (
	ErrSlugTaken = errors.New("gateway slug is already taken")
	ErrSlugEmpty = errors.New("a gateway slug is required")
)

// CreateGatewayInput carries the fields for a new link (donation or payment).
type CreateGatewayInput struct {
	MerchantID       string
	Kind             string // "donation" | "payment"; defaults to payment
	Slug             string
	DisplayName      string
	Branding         map[string]any
	Contact          map[string]any // PII for payment links (email, name); unverified
	ReceivingAddress string
}

// CreateGateway claims the receiving wallet for its product (donation XOR payment —
// a wallet hosts at most one) then creates the link. A wallet already used by the
// other product, or another link, is rejected with store.ErrWalletTaken (-> 409).
// If the insert fails the wallet claim is rolled back, so a failed create never
// leaves an orphan claim.
func (s *Service) CreateGateway(ctx context.Context, in CreateGatewayInput) (store.Gateway, error) {
	addr, err := tonaddr.Canonical(in.ReceivingAddress, s.testnet)
	if err != nil {
		return store.Gateway{}, fmt.Errorf("receivingAddress: %w", err)
	}
	kind := store.ProductPayment
	if in.Kind == store.ProductDonation {
		kind = store.ProductDonation
	}
	slug := NormalizeSlug(in.Slug)
	if slug == "" {
		return store.Gateway{}, ErrSlugEmpty
	}
	// Pre-check the slug for a clean error; the unique index is the real guard and
	// catches a race below.
	if _, ok, err := s.store.GetGatewayBySlug(ctx, slug); err != nil {
		return store.Gateway{}, err
	} else if ok {
		return store.Gateway{}, ErrSlugTaken
	}
	// Claim the wallet for this product before creating the link.
	if err := s.store.ClaimWallet(ctx, store.WalletOwner{Address: addr, Product: kind, OwnerID: in.MerchantID}); err != nil {
		return store.Gateway{}, err // ErrWalletTaken bubbles to a 409
	}
	g := store.Gateway{
		ID:               idgen.New("gw"),
		MerchantID:       in.MerchantID,
		Kind:             kind,
		Slug:             slug,
		DisplayName:      in.DisplayName,
		Branding:         in.Branding,
		Contact:          in.Contact,
		ReceivingAddress: addr,
		Active:           true,
		CreatedAt:        time.Now().UTC(),
	}
	if err := s.store.CreateGateway(ctx, g); err != nil {
		_ = s.store.ReleaseWallet(ctx, addr, in.MerchantID) // roll back the claim
		return store.Gateway{}, err
	}
	return g, nil
}

// IssuedKey is a freshly created key: the raw secret (shown once) and its record.
type IssuedKey struct {
	Raw string
	Key store.APIKey
}

// IssueAPIKey mints a new API key for a merchant. The raw secret is returned once
// and never stored; only its SHA-256 hash is persisted (so the DB lookup is not a
// timing oracle on the secret).
func (s *Service) IssueAPIKey(ctx context.Context, merchantID, mode, label string) (IssuedKey, error) {
	if mode != store.KeyModeTest {
		mode = store.KeyModeLive
	}
	raw, err := newKeySecret(mode)
	if err != nil {
		return IssuedKey{}, err
	}
	k := store.APIKey{
		ID:         idgen.New("ak"),
		MerchantID: merchantID,
		KeyHash:    auth.HashKey(raw),
		KeyPrefix:  keyPrefix(raw),
		Mode:       mode,
		Label:      label,
		CreatedAt:  time.Now().UTC(),
	}
	if err := s.store.CreateAPIKey(ctx, k); err != nil {
		return IssuedKey{}, err
	}
	return IssuedKey{Raw: raw, Key: k}, nil
}

// newKeySecret builds a key like "tpk_live_<32 url-safe chars>" (192-bit random).
func newKeySecret(mode string) (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "tpk_" + mode + "_" + base64.RawURLEncoding.EncodeToString(b), nil
}

func keyPrefix(raw string) string {
	if len(raw) > 16 {
		return raw[:16]
	}
	return raw
}

// NormalizeSlug lowercases and keeps only url-safe characters for a public gateway
// slug (spaces become hyphens).
func NormalizeSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			out = append(out, r)
		case r == ' ':
			out = append(out, '-')
		}
	}
	return string(out)
}
