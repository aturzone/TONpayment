// Package wallet verifies, without custody, that an invoice was paid on-chain.
// The service never holds keys or moves funds; a Verifier only reports whether a
// matching transfer has landed on the receiving address.
package wallet

import (
	"crypto/rand"
	"encoding/hex"
	"sync"

	"github.com/aturzone/TONpayment/internal/store"
)

// Verifier reports whether an invoice has been paid on-chain.
type Verifier interface {
	Verify(inv store.Invoice) (paid bool, txHash string, err error)
}

// NewMemo returns a unique payment comment used to match a TON transfer to an
// invoice. It draws 16 random bytes (128 bits) so collisions are not merely
// improbable but effectively impossible — the store additionally enforces memo
// uniqueness per receiving address, so the "duplicate credit" path the memo
// guards against cannot occur. The memo is only meaningful alongside the
// receiving address and amount.
func NewMemo() string {
	b := make([]byte, 16)
	// The memo is the per-invoice security primitive; never emit a weak/zero one.
	// crypto/rand failing means system entropy is broken — fail loud.
	if _, err := rand.Read(b); err != nil {
		panic("tonpayment: crypto/rand failed generating memo: " + err.Error())
	}
	return "TON-" + hex.EncodeToString(b)
}

// MockVerifier confirms an invoice after it has been polled `after` times, so a
// caller can observe the pending -> paid progression in dev without real funds.
type MockVerifier struct {
	after int
	mu    sync.Mutex
	polls map[string]int
}

var _ Verifier = (*MockVerifier)(nil)

func NewMockVerifier(after int) *MockVerifier {
	if after < 1 {
		after = 1
	}
	return &MockVerifier{after: after, polls: map[string]int{}}
}

func (m *MockVerifier) Verify(inv store.Invoice) (bool, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.polls[inv.ID]++
	if m.polls[inv.ID] >= m.after {
		return true, "mock-tx-" + inv.Memo, nil
	}
	return false, "", nil
}
