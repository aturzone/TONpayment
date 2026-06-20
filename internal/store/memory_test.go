package store

import (
	"errors"
	"testing"
	"time"
)

func TestCreateInvoiceRejectsDuplicateMemo(t *testing.T) {
	m, err := NewMemory("")
	if err != nil {
		t.Fatal(err)
	}
	inv := Invoice{ID: "inv_1", PayTo: "EQpay", Memo: "TON-dup", Status: StatusPending, CreatedAt: time.Now()}
	if err := m.CreateInvoice(inv); err != nil {
		t.Fatal(err)
	}
	// Same (PayTo, Memo) with a different id must be rejected: one payment could
	// otherwise settle both invoices.
	dup := Invoice{ID: "inv_2", PayTo: "EQpay", Memo: "TON-dup", Status: StatusPending, CreatedAt: time.Now()}
	if err := m.CreateInvoice(dup); !errors.Is(err, ErrMemoExists) {
		t.Fatalf("duplicate memo: err = %v, want ErrMemoExists", err)
	}
	// The same memo on a different receiving address is fine.
	other := Invoice{ID: "inv_3", PayTo: "EQother", Memo: "TON-dup", Status: StatusPending, CreatedAt: time.Now()}
	if err := m.CreateInvoice(other); err != nil {
		t.Fatalf("same memo on a different address should be allowed: %v", err)
	}
}

// TestMemoIndexSurvivesReload guards the derived memo index: after persisting and
// reloading from disk, uniqueness is still enforced.
func TestMemoIndexSurvivesReload(t *testing.T) {
	dir := t.TempDir()
	m, err := NewMemory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CreateInvoice(Invoice{ID: "inv_1", PayTo: "EQpay", Memo: "TON-x", Status: StatusPending, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewMemory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := reloaded.CreateInvoice(Invoice{ID: "inv_2", PayTo: "EQpay", Memo: "TON-x", Status: StatusPending, CreatedAt: time.Now()}); !errors.Is(err, ErrMemoExists) {
		t.Fatalf("after reload, duplicate memo err = %v, want ErrMemoExists", err)
	}
}
