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
// invoice. Four random bytes are enough: the memo is only meaningful alongside
// the receiving address and amount.
func NewMemo() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
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
