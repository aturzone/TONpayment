package store

import (
	"context"
	"os"
	"testing"
	"time"
)

// Runs only when TON_TEST_DATABASE_URL points at a disposable Postgres (CI or a
// local docker container). Skipped otherwise so `go test ./...` stays green.
func TestPostgresRoundTrip(t *testing.T) {
	url := os.Getenv("TON_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TON_TEST_DATABASE_URL to run the Postgres integration test")
	}
	ctx := context.Background()
	pg, err := NewPostgres(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()

	// start clean (re-runnable)
	if _, err := pg.pool.Exec(ctx, `DELETE FROM invoices WHERE id LIKE 'pg-%'`); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	inv := Invoice{
		ID: "pg-inv", PayTo: "UQpay", Memo: "TON-abc", AmountNano: 2_500_000_000,
		Currency: "TON", Status: StatusPending, Metadata: map[string]string{"orderId": "o-1"},
		CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute),
	}
	if err := pg.CreateInvoice(inv); err != nil {
		t.Fatal(err)
	}

	got, ok := pg.GetInvoice("pg-inv")
	if !ok || got.Memo != "TON-abc" || got.Metadata["orderId"] != "o-1" || got.ExpiresAt.IsZero() {
		t.Fatalf("invoice roundtrip: %+v ok=%v", got, ok)
	}

	if n := len(pg.ListPending()); n < 1 {
		t.Fatalf("ListPending returned %d, want >= 1", n)
	}

	// claim-once: the first claim wins, the second is a no-op.
	claimed, err := pg.ClaimInvoiceForSettlement("pg-inv", "HASH1", now)
	if err != nil || !claimed {
		t.Fatalf("first claim: claimed=%v err=%v", claimed, err)
	}
	again, err := pg.ClaimInvoiceForSettlement("pg-inv", "HASH2", now)
	if err != nil || again {
		t.Fatalf("second claim must lose: claimed=%v err=%v", again, err)
	}
	if g, _ := pg.GetInvoice("pg-inv"); g.Status != StatusPaid || g.TxHash != "HASH1" || g.PaidAt.IsZero() {
		t.Fatalf("after claim: %+v", g)
	}

	// a paid invoice cannot be expired.
	if exp, err := pg.ExpireInvoice("pg-inv"); err != nil || exp {
		t.Fatalf("paid invoice must not expire: expired=%v err=%v", exp, err)
	}
}
