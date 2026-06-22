package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aturzone/TONpayment/internal/store"
	"github.com/aturzone/TONpayment/internal/wallet"
)

// countingBatchVerifier records how many upstream reads each address incurs, so a
// test can prove CheckPending batches by address instead of polling per invoice.
type countingBatchVerifier struct {
	mu        sync.Mutex
	addrCalls map[string]int
}

func (c *countingBatchVerifier) Verify(inv store.Invoice) (bool, string, error) {
	// Should never be hit when CheckPending uses the batch path; make it loud if it is.
	c.mu.Lock()
	c.addrCalls["__per_invoice__"]++
	c.mu.Unlock()
	return true, "tx", nil
}

func (c *countingBatchVerifier) VerifyAddress(_ context.Context, payTo string, invs []store.Invoice) []wallet.AddrResult {
	c.mu.Lock()
	c.addrCalls[payTo]++
	c.mu.Unlock()
	out := make([]wallet.AddrResult, len(invs))
	for i, inv := range invs {
		out[i] = wallet.AddrResult{Invoice: inv, Paid: true, TxHash: "tx-" + inv.ID}
	}
	return out
}

// Five pending invoices that share one receiving address must settle with exactly
// ONE upstream read — the O(invoices) -> O(addresses) reduction that lets the
// poller survive a backlog without tripping toncenter's rate limit.
func TestCheckPendingBatchesByAddress(t *testing.T) {
	st, err := store.NewMemory("")
	if err != nil {
		t.Fatal(err)
	}
	cv := &countingBatchVerifier{addrCalls: map[string]int{}}
	svc := New(st, cv, "UQpay", time.Minute, nil)
	for i := 0; i < 5; i++ {
		if _, err := svc.CreateInvoice("", 1_000_000_000, time.Minute, nil); err != nil {
			t.Fatal(err)
		}
	}

	svc.CheckPending(context.Background(), 4)

	if got := len(svc.ListPending()); got != 0 {
		t.Fatalf("want 0 pending after batch poll (all settled), got %d", got)
	}
	if cv.addrCalls["__per_invoice__"] != 0 {
		t.Fatalf("batch-capable verifier must not be polled per invoice, got %d per-invoice calls", cv.addrCalls["__per_invoice__"])
	}
	if cv.addrCalls["UQpay"] != 1 {
		t.Fatalf("want exactly 1 upstream call for 5 invoices sharing one address, got %d", cv.addrCalls["UQpay"])
	}
}
