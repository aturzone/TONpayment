package wallet

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aturzone/TONpayment/internal/store"
	"github.com/aturzone/TONpayment/internal/tonaddr"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func clientReturning(jsonBody string) *http.Client {
	return &http.Client{Transport: rtFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(jsonBody)), Header: make(http.Header)}, nil
	})}
}

func TestTonVerifierMatchesByMemoAndAmount(t *testing.T) {
	body := `{"ok":true,"result":[{"transaction_id":{"lt":"1","hash":"HASH1"},"in_msg":{"source":"x","destination":"UQpay","value":"2500000000","message":"TON-abc"}}]}`
	v := NewTonVerifier("https://toncenter.test/api/v2", "", clientReturning(body))
	paid, hash, err := v.Verify(store.Invoice{PayTo: "UQpay", Memo: "TON-abc", AmountNano: 2_500_000_000})
	if err != nil || !paid || hash != "HASH1" {
		t.Fatalf("paid=%v hash=%q err=%v", paid, hash, err)
	}
}

func TestTonVerifierAcceptsOverpayment(t *testing.T) {
	body := `{"ok":true,"result":[{"transaction_id":{"hash":"H"},"in_msg":{"value":"9000000000","message":"TON-abc"}}]}`
	v := NewTonVerifier("b", "", clientReturning(body))
	if paid, _, _ := v.Verify(store.Invoice{PayTo: "x", Memo: "TON-abc", AmountNano: 2_500_000_000}); !paid {
		t.Fatal("overpayment should be accepted")
	}
}

func TestTonVerifierRejectsWrongMemoAndUnderpayment(t *testing.T) {
	wrongMemo := `{"ok":true,"result":[{"transaction_id":{"hash":"H"},"in_msg":{"value":"5000000000","message":"OTHER"}}]}`
	if paid, _, _ := NewTonVerifier("b", "", clientReturning(wrongMemo)).Verify(store.Invoice{PayTo: "x", Memo: "TON-abc", AmountNano: 1}); paid {
		t.Fatal("wrong memo must not match")
	}
	under := `{"ok":true,"result":[{"transaction_id":{"hash":"H"},"in_msg":{"value":"1000000000","message":"TON-abc"}}]}`
	if paid, _, _ := NewTonVerifier("b", "", clientReturning(under)).Verify(store.Invoice{PayTo: "x", Memo: "TON-abc", AmountNano: 2_500_000_000}); paid {
		t.Fatal("underpayment must not match")
	}
}

// TestTonVerifierMatchesDestinationAcrossRepresentations: the invoice stores a
// canonical (user-friendly) address while toncenter returns the destination in raw
// form — the same account. They must still match (compared by account identity).
func TestTonVerifierMatchesDestinationAcrossRepresentations(t *testing.T) {
	var a tonaddr.Address
	a.Hash[0] = 0x11
	friendly, raw := a.String(), a.Raw()
	created := time.Now()
	body := fmt.Sprintf(`{"ok":true,"result":[{"utime":%d,"transaction_id":{"hash":"H"},"in_msg":{"destination":%q,"value":"1000000000","message":"TON-abc"}}]}`, created.Unix(), raw)
	v := NewTonVerifier("b", "", clientReturning(body))
	inv := store.Invoice{PayTo: friendly, Memo: "TON-abc", AmountNano: 1_000_000_000, CreatedAt: created}
	if paid, _, err := v.Verify(inv); !paid || err != nil {
		t.Fatalf("same account in raw form should match: paid=%v err=%v", paid, err)
	}
}

func TestTonVerifierRejectsWrongDestination(t *testing.T) {
	var a, b tonaddr.Address
	a.Hash[0] = 1
	b.Hash[0] = 2
	body := fmt.Sprintf(`{"ok":true,"result":[{"transaction_id":{"hash":"H"},"in_msg":{"destination":%q,"value":"1000000000","message":"TON-abc"}}]}`, b.String())
	v := NewTonVerifier("x", "", clientReturning(body))
	if paid, _, _ := v.Verify(store.Invoice{PayTo: a.String(), Memo: "TON-abc", AmountNano: 1_000_000_000}); paid {
		t.Fatal("a payment to a different account must not match")
	}
}

func TestTonVerifierRejectsTxBeforeInvoice(t *testing.T) {
	var a tonaddr.Address
	a.Hash[0] = 3
	created := time.Now()
	old := created.Add(-2 * time.Hour).Unix() // older than the 1h skew
	body := fmt.Sprintf(`{"ok":true,"result":[{"utime":%d,"transaction_id":{"hash":"H"},"in_msg":{"destination":%q,"value":"1000000000","message":"TON-abc"}}]}`, old, a.String())
	v := NewTonVerifier("x", "", clientReturning(body))
	inv := store.Invoice{PayTo: a.String(), Memo: "TON-abc", AmountNano: 1_000_000_000, CreatedAt: created}
	if paid, _, _ := v.Verify(inv); paid {
		t.Fatal("a transaction older than the invoice must not match")
	}
}

func TestTonVerifierFailsClosed(t *testing.T) {
	v := NewTonVerifier("b", "", &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})})
	paid, _, err := v.Verify(store.Invoice{PayTo: "x", Memo: "m", AmountNano: 1})
	if paid || err == nil {
		t.Fatalf("expected fail-closed (paid=false, err!=nil): paid=%v err=%v", paid, err)
	}
}
