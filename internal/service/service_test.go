package service

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aturzone/TONpayment/internal/store"
	"github.com/aturzone/TONpayment/internal/tonaddr"
)

type fakeVerifier struct{ paid bool }

func (f fakeVerifier) Verify(_ store.Invoice) (bool, string, error) { return f.paid, "tx-1", nil }

// conflictOnceStore reports a memo collision on the first CreateInvoice call, then
// delegates — exercising the service's regenerate-and-retry path.
type conflictOnceStore struct {
	store.Store
	calls int
}

func (c *conflictOnceStore) CreateInvoice(inv store.Invoice) error {
	c.calls++
	if c.calls == 1 {
		return store.ErrMemoExists
	}
	return c.Store.CreateInvoice(inv)
}

// countingWebhook records how many times an invoice settlement notified us.
type countingWebhook struct{ n int32 }

func (c *countingWebhook) Fire(_ store.Invoice) { atomic.AddInt32(&c.n, 1) }

func newMem(t *testing.T) store.Store {
	t.Helper()
	st, err := store.NewMemory("")
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestCreateThenCheckStatusPaid(t *testing.T) {
	svc := New(newMem(t), fakeVerifier{paid: true}, "UQpay", time.Hour, nil)
	inv, err := svc.CreateInvoice("", 2_500_000_000, time.Hour, map[string]string{"orderId": "o1"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Status != store.StatusPending || inv.Memo == "" || inv.PayTo != "UQpay" || inv.Currency != "TON" {
		t.Fatalf("bad new invoice: %+v", inv)
	}
	got, err := svc.CheckStatus(inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusPaid || got.TxHash != "tx-1" || got.PaidAt.IsZero() {
		t.Fatalf("after payment: %+v", got)
	}
}

func TestCreateInvoiceRetriesOnMemoConflict(t *testing.T) {
	cs := &conflictOnceStore{Store: newMem(t)}
	svc := New(cs, fakeVerifier{}, "UQpay", time.Hour, nil)
	inv, err := svc.CreateInvoice("", 1_000_000_000, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cs.calls != 2 {
		t.Fatalf("expected 2 create attempts (1 conflict + 1 success), got %d", cs.calls)
	}
	if got, ok := svc.Get(inv.ID); !ok || got.Status != store.StatusPending {
		t.Fatalf("invoice not stored after retry: %+v ok=%v", got, ok)
	}
}

func TestCheckStatusStaysPendingWhenUnpaid(t *testing.T) {
	svc := New(newMem(t), fakeVerifier{paid: false}, "UQpay", time.Hour, nil)
	inv, _ := svc.CreateInvoice("", 1_000_000_000, time.Hour, nil)
	got, _ := svc.CheckStatus(inv.ID)
	if got.Status != store.StatusPending {
		t.Fatalf("status = %s, want pending", got.Status)
	}
}

func TestCreateInvoiceValidates(t *testing.T) {
	if _, err := New(newMem(t), fakeVerifier{}, "UQpay", time.Hour, nil).CreateInvoice("", 0, time.Hour, nil); err == nil {
		t.Fatal("expected an error for a non-positive amount")
	}
	if _, err := New(newMem(t), fakeVerifier{}, "", time.Hour, nil).CreateInvoice("", 1, time.Hour, nil); err == nil {
		t.Fatal("expected an error when no receiving address is provided or configured")
	}
}

// TestCreateInvoiceUsesValidatedPerRequestPayTo: with no default configured, a
// valid per-request payTo is accepted, canonicalized, and stamped on the invoice.
func TestCreateInvoiceUsesValidatedPerRequestPayTo(t *testing.T) {
	var a tonaddr.Address
	a.Hash[0] = 0xAB
	addr := a.String() // canonical, checksum-valid
	svc := New(newMem(t), fakeVerifier{}, "", time.Hour, nil)
	inv, err := svc.CreateInvoice(addr, 1_000_000_000, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if inv.PayTo != addr {
		t.Fatalf("payTo = %q, want %q", inv.PayTo, addr)
	}
}

func TestCreateInvoiceRejectsInvalidPayTo(t *testing.T) {
	svc := New(newMem(t), fakeVerifier{}, "", time.Hour, nil)
	if _, err := svc.CreateInvoice("not-a-ton-address", 1_000_000_000, time.Hour, nil); err == nil {
		t.Fatal("an invalid payTo must be rejected")
	}
}

func TestCreateInvoiceEnforcesPendingCap(t *testing.T) {
	svc := New(newMem(t), fakeVerifier{}, "UQpay", time.Hour, nil)
	svc.SetLimits(0, 0, 2) // per-address cap of 2
	for i := 0; i < 2; i++ {
		if _, err := svc.CreateInvoice("", 1_000_000_000, time.Hour, nil); err != nil {
			t.Fatalf("create %d should succeed: %v", i, err)
		}
	}
	if _, err := svc.CreateInvoice("", 1_000_000_000, time.Hour, nil); err == nil {
		t.Fatal("create past the per-address cap must be rejected")
	}
}

func TestSetLimitsCapsTTL(t *testing.T) {
	svc := New(newMem(t), fakeVerifier{}, "UQpay", time.Hour, nil)
	svc.SetLimits(time.Minute, 0, 0) // maxTTL = 1 minute
	inv, err := svc.CreateInvoice("", 1_000_000_000, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := inv.ExpiresAt.Sub(inv.CreatedAt); d > time.Minute+time.Second {
		t.Fatalf("ttl = %s, want capped at ~1m", d)
	}
}

func TestCheckStatusExpiresOverdueUnpaid(t *testing.T) {
	st := newMem(t)
	now := time.Now().UTC()
	inv := store.Invoice{
		ID: "inv_x", PayTo: "UQpay", Memo: "TON-1", AmountNano: 1_000_000_000,
		Currency: "TON", Status: store.StatusPending,
		CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Minute),
	}
	if err := st.CreateInvoice(inv); err != nil {
		t.Fatal(err)
	}
	got, err := New(st, fakeVerifier{paid: false}, "UQpay", time.Hour, nil).CheckStatus("inv_x")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusExpired {
		t.Fatalf("status = %s, want expired", got.Status)
	}
}

func TestPaymentBeatsExpiry(t *testing.T) {
	// An invoice already past its TTL but actually paid must settle, not expire.
	st := newMem(t)
	now := time.Now().UTC()
	inv := store.Invoice{
		ID: "inv_late", PayTo: "UQpay", Memo: "TON-2", AmountNano: 1_000_000_000,
		Currency: "TON", Status: store.StatusPending,
		CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Minute),
	}
	if err := st.CreateInvoice(inv); err != nil {
		t.Fatal(err)
	}
	got, _ := New(st, fakeVerifier{paid: true}, "UQpay", time.Hour, nil).CheckStatus("inv_late")
	if got.Status != store.StatusPaid {
		t.Fatalf("status = %s, want paid", got.Status)
	}
}

// TestSettleClaimsOnce guards the settle path directly: two settlement attempts
// on the same invoice (a concurrent-poll race) must fire the webhook exactly once.
func TestSettleClaimsOnce(t *testing.T) {
	cw := &countingWebhook{}
	svc := New(newMem(t), fakeVerifier{paid: true}, "UQpay", time.Hour, cw)
	inv, _ := svc.CreateInvoice("", 1_000_000_000, time.Hour, nil)
	if _, err := svc.settle(inv, "tx-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.settle(inv, "tx-1"); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&cw.n); n != 1 {
		t.Fatalf("webhook fired %d times, want exactly 1", n)
	}
}

// TestNoDoubleSettleUnderConcurrentCheckStatus is the headline idempotency
// guarantee: many concurrent CheckStatus calls settle the invoice once.
func TestNoDoubleSettleUnderConcurrentCheckStatus(t *testing.T) {
	cw := &countingWebhook{}
	svc := New(newMem(t), fakeVerifier{paid: true}, "UQpay", time.Hour, cw)
	inv, _ := svc.CreateInvoice("", 2_500_000_000, time.Hour, nil)

	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = svc.CheckStatus(inv.ID) }()
	}
	wg.Wait()

	got, ok := svc.Get(inv.ID)
	if !ok || got.Status != store.StatusPaid {
		t.Fatalf("status=%v ok=%v, want paid", got.Status, ok)
	}
	if n := atomic.LoadInt32(&cw.n); n != 1 {
		t.Fatalf("webhook fired %d times under concurrency, want exactly 1", n)
	}
}
