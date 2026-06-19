package wallet

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aturzone/TONpayment/internal/store"
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

func TestTonVerifierFailsClosed(t *testing.T) {
	v := NewTonVerifier("b", "", &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})})
	paid, _, err := v.Verify(store.Invoice{PayTo: "x", Memo: "m", AmountNano: 1})
	if paid || err == nil {
		t.Fatalf("expected fail-closed (paid=false, err!=nil): paid=%v err=%v", paid, err)
	}
}
