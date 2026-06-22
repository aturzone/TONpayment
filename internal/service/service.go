// Package service is the invoice lifecycle: create pending invoices, verify
// payment on-chain (or via the mock), and settle them exactly once. It strips
// out all of the donor project's domain (products, balances, subscriptions) and
// keeps only the proven create -> verify -> claim-once -> settle machinery.
package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aturzone/TONpayment/internal/idgen"
	"github.com/aturzone/TONpayment/internal/store"
	"github.com/aturzone/TONpayment/internal/tonaddr"
	"github.com/aturzone/TONpayment/internal/wallet"
)

const (
	// MaxTTL is the default hard cap on how long an invoice may stay pending, so a
	// caller cannot mint long-lived invoices that the poller re-checks (and bills
	// toncenter for). Overridable via SetLimits / TON_MAX_TTL_SECONDS.
	MaxTTL = 24 * time.Hour
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
	st                store.Store
	verifier          wallet.Verifier
	defaultPayTo      string
	currency          string
	defaultTTL        time.Duration
	maxTTL            time.Duration
	maxPending        int // global cap on pending invoices; 0 = unlimited
	maxPendingPerAddr int // per-address cap on pending invoices; 0 = unlimited
	webhook           Webhook
	testnet           bool
	locks             keyedLocks
}

// SetNetwork sets whether payTo addresses must be testnet (default: mainnet).
// A per-request payTo whose network tag disagrees is rejected at create time.
func (s *Service) SetNetwork(testnet bool) { s.testnet = testnet }

// New builds the service. defaultPayTo is the fallback receiving address used when
// a create request omits payTo (it may be empty — then every request must supply
// its own payTo); defaultTTL is used when a create request omits a TTL; wh may be
// nil.
func New(st store.Store, v wallet.Verifier, defaultPayTo string, defaultTTL time.Duration, wh Webhook) *Service {
	if defaultTTL <= 0 {
		defaultTTL = 15 * time.Minute
	}
	return &Service{st: st, verifier: v, defaultPayTo: defaultPayTo, currency: "TON", defaultTTL: defaultTTL, maxTTL: MaxTTL, webhook: wh}
}

// SetLimits configures resource bounds: the maximum invoice TTL, and caps on the
// number of pending invoices in total and per receiving address (0 = unlimited).
// A non-positive maxTTL keeps the default. Call once at startup, before serving.
func (s *Service) SetLimits(maxTTL time.Duration, maxPending, maxPendingPerAddr int) {
	if maxTTL > 0 {
		s.maxTTL = maxTTL
	}
	s.maxPending = maxPending
	s.maxPendingPerAddr = maxPendingPerAddr
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
// reference and is echoed back unchanged. This is the single-tenant entry point: a
// per-request payTo (untrusted) is validated here, otherwise the configured
// default is used; the resulting invoice carries no merchant/gateway tag.
func (s *Service) CreateInvoice(payTo string, amountNano int64, ttl time.Duration, metadata map[string]string) (store.Invoice, error) {
	// Resolve the receiving address: a per-request payTo wins, otherwise the
	// configured default. The per-request value is untrusted input, so validate and
	// canonicalize it here; the default was already validated at startup.
	addr := s.defaultPayTo
	if payTo != "" {
		norm, err := tonaddr.NormalizeOnNetwork(payTo, s.testnet)
		if err != nil {
			return store.Invoice{}, fmt.Errorf("payTo: %w", err)
		}
		addr = norm
	}
	return s.create(addr, amountNano, ttl, metadata, "", "")
}

// CreateInvoiceForGateway is the multi-tenant entry point: it creates an invoice
// paid to a gateway's receiving address and tags it with the gateway + merchant so
// reads, webhooks and analytics can be scoped. The address was validated and
// canonicalized at gateway creation, so it is trusted here.
func (s *Service) CreateInvoiceForGateway(g store.Gateway, amountNano int64, ttl time.Duration, metadata map[string]string) (store.Invoice, error) {
	return s.create(g.ReceivingAddress, amountNano, ttl, metadata, g.MerchantID, g.ID)
}

// create is the shared invoice-creation core: it validates the amount, metadata and
// pending caps, clamps the TTL, and allocates a unique (PayTo, Memo). addr must
// already be resolved and canonical. merchantID/gatewayID are empty in
// single-tenant mode.
func (s *Service) create(addr string, amountNano int64, ttl time.Duration, metadata map[string]string, merchantID, gatewayID string) (store.Invoice, error) {
	if amountNano <= 0 {
		return store.Invoice{}, errors.New("amountNano must be a positive integer (nanoTON)")
	}
	if addr == "" {
		return store.Invoice{}, errors.New("no receiving address: provide payTo, or configure a default (TON_RECEIVING_ADDRESS)")
	}
	if err := validateMetadata(metadata); err != nil {
		return store.Invoice{}, err
	}
	// Bound the pending set: an unbounded number of pending invoices is unbounded
	// verification work (each is re-checked against toncenter every poll tick).
	if s.maxPending > 0 || s.maxPendingPerAddr > 0 {
		total, forAddr, err := s.st.PendingCounts(addr)
		if err != nil {
			return store.Invoice{}, err
		}
		if s.maxPending > 0 && total >= s.maxPending {
			return store.Invoice{}, errors.New("too many pending invoices on this server; try again later")
		}
		if s.maxPendingPerAddr > 0 && forAddr >= s.maxPendingPerAddr {
			return store.Invoice{}, errors.New("too many pending invoices for this address; try again later")
		}
	}
	if ttl <= 0 {
		ttl = s.defaultTTL
	}
	if ttl > s.maxTTL {
		ttl = s.maxTTL
	}
	now := time.Now().UTC()
	inv := store.Invoice{
		PayTo:      addr,
		AmountNano: amountNano,
		Currency:   s.currency,
		Status:     store.StatusPending,
		Metadata:   metadata,
		CreatedAt:  now,
		ExpiresAt:  now.Add(ttl),
		MerchantID: merchantID,
		GatewayID:  gatewayID,
	}
	// Allocate a unique (PayTo, Memo). The store rejects a duplicate memo, so the
	// uniqueness the verifier relies on is a hard guarantee, not a probability.
	// With a 128-bit memo the first attempt essentially always wins.
	for attempt := 0; attempt < 5; attempt++ {
		inv.ID = idgen.New("inv")
		inv.Memo = wallet.NewMemo()
		err := s.st.CreateInvoice(inv)
		if err == nil {
			return inv, nil
		}
		if errors.Is(err, store.ErrMemoExists) {
			continue
		}
		return store.Invoice{}, err
	}
	return store.Invoice{}, errors.New("could not allocate a unique memo; please retry")
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

// CheckPending re-verifies every pending invoice for the background poller,
// batching upstream reads by receiving address: invoices that share a PayTo (all of
// a merchant's invoices land on its one wallet) are resolved with a single network
// round-trip via a BatchVerifier, turning O(invoices) toncenter calls per tick into
// O(distinct addresses). concurrency bounds how many addresses are in flight; the
// verifier's own rate limiter caps the actual upstream QPS, so concurrency only
// overlaps local settle work. A verifier that is not batch-capable (the dev mock)
// is polled per invoice. ctx cancellation stops the sweep promptly.
func (s *Service) CheckPending(ctx context.Context, concurrency int) {
	pending := s.st.ListPending()
	if len(pending) == 0 {
		return
	}
	byAddr := map[string][]store.Invoice{}
	order := make([]string, 0)
	for _, inv := range pending {
		if _, seen := byAddr[inv.PayTo]; !seen {
			order = append(order, inv.PayTo)
		}
		byAddr[inv.PayTo] = append(byAddr[inv.PayTo], inv)
	}
	if concurrency < 1 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, addr := range order {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(addr string, invs []store.Invoice) {
			defer wg.Done()
			defer func() { <-sem }()
			s.checkAddress(ctx, addr, invs)
		}(addr, byAddr[addr])
	}
	wg.Wait()
}

// checkAddress verifies every invoice on one receiving address with a single
// upstream read (or per-invoice if the verifier isn't batch-capable) and applies
// each verdict.
func (s *Service) checkAddress(ctx context.Context, addr string, invs []store.Invoice) {
	if bv, ok := s.verifier.(wallet.BatchVerifier); ok {
		for _, r := range bv.VerifyAddress(ctx, addr, invs) {
			_, _ = s.apply(r.Invoice, r.Paid, r.TxHash, r.Err)
		}
		return
	}
	for _, inv := range invs {
		paid, tx, err := s.verifier.Verify(inv)
		_, _ = s.apply(inv, paid, tx, err)
	}
}

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
	return s.apply(inv, paid, tx, err)
}

// apply turns a verify verdict into a state transition, shared by the on-demand
// CheckStatus path and the batched poller path so both settle identically. A paid
// invoice settles exactly once (re-read under the per-invoice lock; the store's
// atomic claim is the real guard); an unpaid invoice past its TTL expires;
// otherwise it stays pending. A verifier error is treated as "not paid" — fail
// closed.
func (s *Service) apply(inv store.Invoice, paid bool, tx string, verr error) (store.Invoice, error) {
	if verr == nil && paid {
		// Re-read under the lock so a concurrent poll that already settled this invoice
		// cannot make us settle (and webhook) twice.
		unlock := s.locks.lock(inv.ID)
		defer unlock()
		cur, ok := s.st.GetInvoice(inv.ID)
		if !ok {
			return store.Invoice{}, errors.New("not found")
		}
		if cur.Status != store.StatusPending {
			return cur, nil
		}
		return s.settle(cur, tx)
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
