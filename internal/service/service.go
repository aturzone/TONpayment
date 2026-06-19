// Package service is the invoice lifecycle: create pending invoices, verify
// payment on-chain (or via the mock), and settle them exactly once. It strips
// out all of the donor project's domain (products, balances, subscriptions) and
// keeps only the proven create -> verify -> claim-once -> settle machinery.
package service

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aturzone/TONpayment/internal/idgen"
	"github.com/aturzone/TONpayment/internal/store"
	"github.com/aturzone/TONpayment/internal/wallet"
)

const (
	// MaxTTL bounds how long an invoice may stay pending, so a caller cannot mint
	// effectively immortal invoices that the poller re-checks (and bills toncenter
	// for) forever.
	MaxTTL = 7 * 24 * time.Hour
	// metadata bounds: the caller's reference is opaque, but it is stored and
	// echoed on every read/webhook, so keep it small.
	maxMetadataKeys  = 64
	maxMetadataBytes = 8 * 1024
)

// Webhook is the optional sink notified when an invoice settles. It is an
// interface so the service does not depend on the concrete webhook package
// (and so tests can substitute a counter). A nil Webhook means "no webhook".
type Webhook interface {
	Fire(inv store.Invoice)
}

type Service struct {
	st         store.Store
	verifier   wallet.Verifier
	payTo      string
	currency   string
	defaultTTL time.Duration
	webhook    Webhook
	locks      keyedLocks
}

// New builds the service. payTo is the receiving address stamped on every
// invoice; defaultTTL is used when a create request omits a TTL; wh may be nil.
func New(st store.Store, v wallet.Verifier, payTo string, defaultTTL time.Duration, wh Webhook) *Service {
	if defaultTTL <= 0 {
		defaultTTL = 15 * time.Minute
	}
	return &Service{st: st, verifier: v, payTo: payTo, currency: "TON", defaultTTL: defaultTTL, webhook: wh}
}

// keyedLocks serializes critical sections per key (here: per invoice ID) so
// settlement is atomic within this process — closing the double-settle window
// when callers poll status concurrently. Correct for a single instance; running
// multiple instances additionally relies on the store's atomic claim (the
// ClaimInvoiceForSettlement / ExpireInvoice transitions) for correctness.
type keyedLocks struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func (k *keyedLocks) lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*sync.Mutex{}
	}
	l := k.m[key]
	if l == nil {
		l = &sync.Mutex{}
		k.m[key] = l
	}
	k.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// CreateInvoice builds a pending invoice for amountNano (nanoTON), valid for ttl
// (or the configured default if ttl <= 0). metadata is the caller's own opaque
// reference and is echoed back unchanged.
func (s *Service) CreateInvoice(amountNano int64, ttl time.Duration, metadata map[string]string) (store.Invoice, error) {
	if amountNano <= 0 {
		return store.Invoice{}, errors.New("amountNano must be a positive integer (nanoTON)")
	}
	if s.payTo == "" {
		return store.Invoice{}, errors.New("no receiving address configured (set TON_RECEIVING_ADDRESS)")
	}
	if err := validateMetadata(metadata); err != nil {
		return store.Invoice{}, err
	}
	if ttl <= 0 {
		ttl = s.defaultTTL
	}
	if ttl > MaxTTL {
		ttl = MaxTTL
	}
	now := time.Now().UTC()
	inv := store.Invoice{
		ID:         idgen.New("inv"),
		PayTo:      s.payTo,
		Memo:       wallet.NewMemo(),
		AmountNano: amountNano,
		Currency:   s.currency,
		Status:     store.StatusPending,
		Metadata:   metadata,
		CreatedAt:  now,
		ExpiresAt:  now.Add(ttl),
	}
	if err := s.st.CreateInvoice(inv); err != nil {
		return store.Invoice{}, err
	}
	return inv, nil
}

// Get returns an invoice without touching the chain.
func (s *Service) Get(id string) (store.Invoice, bool) { return s.st.GetInvoice(id) }

func validateMetadata(md map[string]string) error {
	if len(md) > maxMetadataKeys {
		return fmt.Errorf("metadata has too many keys (max %d)", maxMetadataKeys)
	}
	total := 0
	for k, v := range md {
		total += len(k) + len(v)
	}
	if total > maxMetadataBytes {
		return fmt.Errorf("metadata too large: %d bytes (max %d)", total, maxMetadataBytes)
	}
	return nil
}

// ListPending exposes the store's pending set for the background poller.
func (s *Service) ListPending() []store.Invoice { return s.st.ListPending() }

// CheckStatus verifies payment on-chain (or via the mock) and settles if paid.
// Payment takes priority over expiry: an invoice that was actually paid settles
// even if we only notice slightly past its TTL; an unpaid invoice past its TTL
// is expired. Any verifier error leaves the invoice pending (fail closed).
func (s *Service) CheckStatus(id string) (store.Invoice, error) {
	inv, ok := s.st.GetInvoice(id)
	if !ok {
		return store.Invoice{}, errors.New("not found")
	}
	if inv.Status != store.StatusPending {
		return inv, nil // paid or expired: terminal
	}

	paid, tx, err := s.verifier.Verify(inv) // network read — outside the lock
	if err == nil && paid {
		// Settle atomically per invoice; re-read under the lock so a concurrent poll
		// that already settled this invoice cannot make us settle (and webhook) twice.
		unlock := s.locks.lock(inv.ID)
		defer unlock()
		inv, ok = s.st.GetInvoice(id)
		if !ok {
			return store.Invoice{}, errors.New("not found")
		}
		if inv.Status != store.StatusPending {
			return inv, nil
		}
		return s.settle(inv, tx)
	}

	// Not paid (or a verifier error): expire if the TTL has passed.
	if !inv.ExpiresAt.IsZero() && time.Now().After(inv.ExpiresAt) {
		return s.expire(inv)
	}
	return inv, nil // stay pending
}

// settle marks the invoice paid exactly once and fires the webhook. It is safe
// to call without holding the per-invoice lock: the store's atomic claim is the
// real guard, so concurrent or repeated calls settle (and notify) only once.
func (s *Service) settle(inv store.Invoice, txHash string) (store.Invoice, error) {
	now := time.Now().UTC()
	claimed, err := s.st.ClaimInvoiceForSettlement(inv.ID, txHash, now)
	if err != nil {
		return inv, err
	}
	if !claimed {
		if cur, ok := s.st.GetInvoice(inv.ID); ok {
			return cur, nil // already settled by a concurrent caller
		}
		return inv, nil
	}
	inv.Status = store.StatusPaid
	inv.TxHash = txHash
	inv.PaidAt = now
	if s.webhook != nil {
		s.webhook.Fire(inv) // only the claim winner reaches here -> fires exactly once
	}
	return inv, nil
}

// expire marks an overdue, still-pending invoice expired (atomic claim-once).
func (s *Service) expire(inv store.Invoice) (store.Invoice, error) {
	claimed, err := s.st.ExpireInvoice(inv.ID)
	if err != nil {
		return inv, err
	}
	if !claimed {
		if cur, ok := s.st.GetInvoice(inv.ID); ok {
			return cur, nil
		}
		return inv, nil
	}
	inv.Status = store.StatusExpired
	return inv, nil
}
