package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Memory is an in-memory Store with optional JSON persistence to a directory.
// It mirrors the read-light RWMutex shape of the production store: reads take the
// read lock, mutations take the write lock and then persist.
type Memory struct {
	mu       sync.RWMutex
	dir      string
	invoices map[string]Invoice
}

var _ Store = (*Memory)(nil)

type snapshot struct {
	Invoices map[string]Invoice `json:"invoices"`
}

// NewMemory creates the store, loading prior state from dir/store.json if present.
// Pass an empty dir to disable persistence (useful in tests).
func NewMemory(dir string) (*Memory, error) {
	m := &Memory{dir: dir, invoices: map[string]Invoice{}}
	if dir != "" {
		if err := m.load(); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func (m *Memory) file() string { return filepath.Join(m.dir, "store.json") }

func (m *Memory) load() error {
	b, err := os.ReadFile(m.file())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var s snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s.Invoices != nil {
		m.invoices = s.Invoices
	}
	return nil
}

// persist writes the current state atomically. Callers must hold the write lock.
func (m *Memory) persist() error {
	if m.dir == "" {
		return nil
	}
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(snapshot{Invoices: m.invoices}, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.file() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.file())
}

func (m *Memory) CreateInvoice(inv Invoice) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.invoices[inv.ID] = inv
	if err := m.persist(); err != nil {
		delete(m.invoices, inv.ID) // roll back so memory and disk agree
		return err
	}
	return nil
}

func (m *Memory) GetInvoice(id string) (Invoice, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	inv, ok := m.invoices[id]
	return inv, ok
}

func (m *Memory) ListInvoices() []Invoice {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Invoice, 0, len(m.invoices))
	for _, inv := range m.invoices {
		out = append(out, inv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (m *Memory) ListPending() []Invoice {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []Invoice{}
	for _, inv := range m.invoices {
		if inv.Status == StatusPending {
			out = append(out, inv)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func (m *Memory) ClaimInvoiceForSettlement(id, txHash string, paidAt time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.invoices[id]
	if !ok || cur.Status != StatusPending {
		return false, nil
	}
	updated := cur
	updated.Status = StatusPaid
	updated.TxHash = txHash
	updated.PaidAt = paidAt
	m.invoices[id] = updated
	if err := m.persist(); err != nil {
		m.invoices[id] = cur // roll back: don't report a claim we couldn't durably write
		return false, err
	}
	return true, nil
}

func (m *Memory) ExpireInvoice(id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.invoices[id]
	if !ok || cur.Status != StatusPending {
		return false, nil
	}
	updated := cur
	updated.Status = StatusExpired
	m.invoices[id] = updated
	if err := m.persist(); err != nil {
		m.invoices[id] = cur // roll back
		return false, err
	}
	return true, nil
}
