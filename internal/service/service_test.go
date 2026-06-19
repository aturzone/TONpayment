package service

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aturzone/TONpayment/internal/store"
)

type fakeVerifier struct{ paid bool }

func (f fakeVerifier) Verify(_ store.Invoice) (bool, string, error) { return f.paid, "tx-1", nil }

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
	inv, err := svc.CreateInvoice(2_500_000_000, time.Hour, map[string]string{"orderId": "o1"})
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

func TestCheckStatusStaysPendingWhenUnpaid(t *testing.T) {
	svc := New(newMem(t), fakeVerifier{paid: false}, "UQpay", time.Hour, nil)
	inv, _ := svc.CreateInvoice(1_000_000_000, time.Hour, nil)
	got, _ := svc.CheckStatus(inv.ID)
	if got.Status != store.StatusPending {
		t.Fatalf("status = %s, want pending", got.Status)
	}
}

func TestCreateInvoiceValidates(t *testing.T) {
	if _, err := New(newMem(t), fakeVerifier{}, "UQpay", time.Hour, nil).CreateInvoice(0, time.Hour, nil); err == nil {
		t.Fatal("expected an error for a non-positive amount")
	}
	if _, err := New(newMem(t), fakeVerifier{}, "", time.Hour, nil).CreateInvoice(1, time.Hour, nil); err == nil {
		t.Fatal("expected an error when no receiving address is configured")
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
	inv, _ := svc.CreateInvoice(1_000_000_000, time.Hour, nil)
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
	inv, _ := svc.CreateInvoice(2_500_000_000, time.Hour, nil)

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
