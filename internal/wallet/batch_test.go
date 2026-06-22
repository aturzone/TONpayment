package wallet

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aturzone/TONpayment/internal/store"
)

// The whole point of the batch path: many invoices on one receiving address cost a
// SINGLE upstream read, not one per invoice. This is what keeps the poller under
// toncenter's rate limit when a merchant has a backlog of pending invoices.
func TestVerifyAddressOneRequestForManyInvoices(t *testing.T) {
	var calls int32
	body := `{"ok":true,"result":[
		{"transaction_id":{"hash":"H1"},"in_msg":{"value":"1000000000","message":"TON-a"}},
		{"transaction_id":{"hash":"H2"},"in_msg":{"value":"2000000000","message":"TON-b"}}
	]}`
	client := &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	v := NewTonVerifier("b", "", client)

	invs := []store.Invoice{
		{ID: "i1", PayTo: "x", Memo: "TON-a", AmountNano: 1_000_000_000},
		{ID: "i2", PayTo: "x", Memo: "TON-b", AmountNano: 1_000_000_000},
		{ID: "i3", PayTo: "x", Memo: "TON-missing", AmountNano: 1_000_000_000}, // not in the tx list
	}
	res := v.VerifyAddress(context.Background(), "x", invs)

	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("want 1 upstream call for 3 invoices on one address, got %d", n)
	}
	if len(res) != 3 {
		t.Fatalf("want 3 results, got %d", len(res))
	}
	if !res[0].Paid || res[0].TxHash != "H1" {
		t.Errorf("invoice 0 should be paid via H1: %+v", res[0])
	}
	if !res[1].Paid || res[1].TxHash != "H2" {
		t.Errorf("invoice 1 should be paid via H2: %+v", res[1])
	}
	if res[2].Paid {
		t.Errorf("invoice 2 has no matching tx and must stay unpaid: %+v", res[2])
	}
}

// A read error must fail closed for EVERY invoice in the batch, so the whole group
// simply stays pending and retries next tick (never a false settle).
func TestVerifyAddressFailsClosedForWholeBatch(t *testing.T) {
	client := &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})}
	v := NewTonVerifier("b", "", client)
	invs := []store.Invoice{
		{ID: "i1", PayTo: "x", Memo: "TON-a", AmountNano: 1},
		{ID: "i2", PayTo: "x", Memo: "TON-b", AmountNano: 1},
	}
	for _, r := range v.VerifyAddress(context.Background(), "x", invs) {
		if r.Paid || r.Err == nil {
			t.Fatalf("a read error must fail closed for every invoice, got %+v", r)
		}
	}
}
