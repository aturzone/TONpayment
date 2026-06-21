package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/aturzone/TONpayment/internal/idgen"
	"github.com/aturzone/TONpayment/internal/store"
	"github.com/aturzone/TONpayment/internal/tonaddr"
)

// PubKeyResolver returns the ed25519 public key for a TON address — normally by
// calling the wallet contract's get_public_key get-method. wallet.PubKeyClient
// satisfies this structurally, so this package does not import wallet.
type PubKeyResolver func(ctx context.Context, address string) (ed25519.PublicKey, error)

// Errors surfaced to the verify handler (mapped to HTTP statuses there).
var (
	ErrBadProof   = errors.New("ton_proof verification failed")
	ErrBadDomain  = errors.New("ton_proof domain not allowed")
	ErrStaleProof = errors.New("ton_proof timestamp is stale")
	ErrUsedNonce  = errors.New("challenge is unknown, expired, or already used")
	ErrNoPubKey   = errors.New("could not resolve the wallet public key — the wallet must be deployed (send any transaction first)")
	ErrSuspended  = errors.New("merchant account is suspended")
)

// Default TTLs.
const (
	defaultChallengeTTL = 5 * time.Minute
	defaultSessionTTL   = 24 * time.Hour
	proofSkew           = 5 * time.Minute // allowed |now - proof.timestamp|
)

// Service runs ton_proof sign-in: it issues single-use challenges and verifies the
// returned proof, upserting a merchant for the proven wallet and minting a session
// token. Admin status is granted to wallets in AdminWallets.
type Service struct {
	Store         store.TenantStore
	Resolve       PubKeyResolver
	SessionSecret []byte
	Domains       map[string]bool // allowed ton_proof domains (lowercased)
	AdminWallets  map[string]bool // canonical admin wallets
	ChallengeTTL  time.Duration
	SessionTTL    time.Duration
	Now           func() time.Time // injectable clock (tests)
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) challengeTTL() time.Duration {
	if s.ChallengeTTL > 0 {
		return s.ChallengeTTL
	}
	return defaultChallengeTTL
}

func (s *Service) sessionTTL() time.Duration {
	if s.SessionTTL > 0 {
		return s.SessionTTL
	}
	return defaultSessionTTL
}

// Challenge issues a fresh single-use nonce for the client to embed as the
// ton_proof payload.
func (s *Service) Challenge(ctx context.Context) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	nonce := base64.RawURLEncoding.EncodeToString(b)
	if err := s.Store.PutChallenge(ctx, nonce, s.challengeTTL()); err != nil {
		return "", err
	}
	return nonce, nil
}

// VerifyInput is a decoded verify request. PublicKey is the wallet's key as
// reported by TON Connect (account.publicKey); when present and bound to Address it
// is preferred over the on-chain lookup, so sign-in works for undeployed wallets.
type VerifyInput struct {
	Address   string
	PublicKey ed25519.PublicKey
	Proof     Proof
}

// Verify validates the proof end-to-end and returns a session token plus the
// merchant. Cheap checks run first; the single-use nonce is consumed before the
// network round-trip so a nonce can never be replayed even if later steps fail.
func (s *Service) Verify(ctx context.Context, in VerifyInput) (string, store.Merchant, error) {
	// 1. domain allow-list (cheap, no state)
	if len(s.Domains) > 0 && !s.Domains[strings.ToLower(in.Proof.Domain)] {
		return "", store.Merchant{}, ErrBadDomain
	}
	// 2. timestamp freshness (cheap)
	skew := int64(proofSkew / time.Second)
	if d := s.now().Unix() - in.Proof.Timestamp; d > skew || d < -skew {
		return "", store.Merchant{}, ErrStaleProof
	}
	// 3. address parses (cheap)
	addr, err := tonaddr.Parse(in.Address)
	if err != nil {
		return "", store.Merchant{}, ErrBadProof
	}
	// 4. consume the single-use nonce BEFORE the expensive resolve step (anti-replay)
	won, err := s.Store.ConsumeChallenge(ctx, in.Proof.Payload)
	if err != nil {
		return "", store.Merchant{}, err
	}
	if !won {
		return "", store.Merchant{}, ErrUsedNonce
	}
	// 5. resolve the public key. Prefer the TON Connect-supplied key when it is
	// cryptographically bound to the claimed address (works for undeployed wallets);
	// otherwise fall back to the on-chain get_public_key (deployed wallets only).
	var pub ed25519.PublicKey
	if len(in.PublicKey) == ed25519.PublicKeySize && PubKeyOwnsAddress(in.PublicKey, addr) {
		pub = in.PublicKey
	} else if s.Resolve != nil {
		if p, err := s.Resolve(ctx, in.Address); err == nil && p != nil {
			pub = p
		}
	}
	if pub == nil {
		return "", store.Merchant{}, ErrNoPubKey
	}
	if !VerifyProof(pub, addr, in.Proof) {
		return "", store.Merchant{}, ErrBadProof
	}
	// 6. upsert the merchant for this wallet (canonical, non-bounceable form)
	wallet := canonicalWallet(addr)
	m, err := s.upsertMerchant(ctx, wallet)
	if err != nil {
		return "", store.Merchant{}, err
	}
	if m.Status != store.MerchantActive {
		return "", store.Merchant{}, ErrSuspended
	}
	// 7. mint the session
	now := s.now()
	c := Claims{
		MerchantID: m.ID,
		Wallet:     wallet,
		Admin:      s.AdminWallets[wallet],
		IssuedAt:   now.Unix(),
		ExpiresAt:  now.Add(s.sessionTTL()).Unix(),
	}
	return SignSession(s.SessionSecret, c), m, nil
}

func (s *Service) upsertMerchant(ctx context.Context, wallet string) (store.Merchant, error) {
	if m, ok, err := s.Store.GetMerchantByWallet(ctx, wallet); err != nil {
		return store.Merchant{}, err
	} else if ok {
		return m, nil
	}
	m := store.Merchant{ID: idgen.New("mer"), Wallet: wallet, Status: store.MerchantActive, CreatedAt: s.now().UTC()}
	if err := s.Store.CreateMerchant(ctx, m); err != nil {
		// Lost a concurrent create race? The wallet's unique index rejected us; the
		// existing row is the source of truth.
		if existing, ok, err2 := s.Store.GetMerchantByWallet(ctx, wallet); err2 == nil && ok {
			return existing, nil
		}
		return store.Merchant{}, err
	}
	return m, nil
}

// canonicalWallet is the single identity form for a wallet across merchants and the
// wallet_ownership registry: non-bounceable user-friendly (UQ…), which is the
// conventional receiving form. On-chain matching compares account identity, not
// this string, so the display form is purely for consistent keying.
func canonicalWallet(a tonaddr.Address) string { return a.Friendly(false) }
