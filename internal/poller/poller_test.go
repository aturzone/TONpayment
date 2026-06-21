package poller

import (
	"context"
	"testing"
	"time"

	"github.com/aturzone/TONpayment/internal/service"
	"github.com/aturzone/TONpayment/internal/store"
)

type paidVerifier struct{}

func (paidVerifier) Verify(_ store.Invoice) (bool, string, error) { return true, "tx-poll", nil }

// One tick must settle every pending invoice (the verifier reports them paid),
// exercising the bounded-concurrency fan-out.
func TestTickSettlesPending(t *testing.T) {
	st, err := store.NewMemory("")
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(st, paidVerifier{}, "UQpay", time.Minute, nil)
	for i := 0; i < 5; i++ {
		if _, err := svc.CreateInvoice("", 1_000_000_000, time.Minute, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(svc.ListPending()); got != 5 {
		t.Fatalf("want 5 pending before tick, got %d", got)
	}

	New(svc, time.Second, 3).tick(context.Background())

	if got := len(svc.ListPending()); got != 0 {
		t.Fatalf("want 0 pending after tick (all settled), got %d", got)
	}
}

// tick with nothing pending must be a safe no-op.
func TestTickNoPending(t *testing.T) {
	st, err := store.NewMemory("")
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(st, paidVerifier{}, "UQpay", time.Minute, nil)
	New(svc, time.Second, 3).tick(context.Background()) // must not panic
}

// A cancelled context must stop the fan-out promptly without panicking.
func TestTickRespectsCancel(t *testing.T) {
	st, _ := store.NewMemory("")
	svc := service.New(st, paidVerifier{}, "UQpay", time.Minute, nil)
	for i := 0; i < 5; i++ {
		_, _ = svc.CreateInvoice("", 1_000_000_000, time.Minute, nil)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	New(svc, time.Second, 2).tick(ctx) // returns without panic
}
