// Package store defines the Invoice domain type and the persistence interface.
// An in-memory/JSON implementation lives in memory.go and a Postgres (pgx)
// implementation in postgres.go — both satisfy Store, so the rest of the service
// is agnostic to where invoices are kept.
package store

import (
	"errors"
	"time"
)

// ErrMemoExists is returned by CreateInvoice when an invoice with the same
// (PayTo, Memo) already exists. It makes memo uniqueness a hard guarantee rather
// than a probabilistic one: the caller regenerates the memo and retries. Two
// invoices that shared a memo on the same address could otherwise both be settled
// by a single on-chain payment (a duplicate credit).
var ErrMemoExists = errors.New("an invoice with this memo already exists for this address")

// Invoice statuses.
const (
	StatusPending = "pending"
	StatusPaid    = "paid"
	StatusExpired = "expired"
)

// Invoice is a request for an on-chain TON payment. The service is watch-only:
// it never holds keys or moves funds — it only confirms that a payment matching
// the (PayTo, Memo, AmountNano) triple landed on-chain and records the tx hash.
//
// Metadata is the caller's own reference (orderID, userID, …); it is opaque to
// this service and is echoed back verbatim on every read and webhook.
type Invoice struct {
	ID         string            `json:"id"`
	PayTo      string            `json:"payTo"`      // receiving address
	Memo       string            `json:"memo"`       // unique comment the payer must include
	AmountNano int64             `json:"amountNano"` // required amount in nanoTON (overpayment accepted)
	Currency   string            `json:"currency"`   // always "TON"
	Status     string            `json:"status"`     // pending | paid | expired
	TxHash     string            `json:"txHash,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	CreatedAt  time.Time         `json:"createdAt"`
	PaidAt     time.Time         `json:"paidAt,omitempty"`
	ExpiresAt  time.Time         `json:"expiresAt,omitempty"`
}

// Store is the persistence contract. Implementations must be safe for concurrent
// use.
type Store interface {
	CreateInvoice(inv Invoice) error
	GetInvoice(id string) (Invoice, bool)
	// ListInvoices returns every invoice (newest first).
	ListInvoices() []Invoice
	// ListPending returns only invoices still awaiting payment — the poller's hot
	// path, so it is the query that must stay cheap (indexed on status).
	ListPending() []Invoice
	// ClaimInvoiceForSettlement atomically flips a pending invoice to paid (setting
	// txHash/paidAt) and reports whether THIS call won the transition, so settlement
	// effects (e.g. firing the webhook) happen exactly once even under concurrent
	// callers racing to settle the same invoice.
	ClaimInvoiceForSettlement(id, txHash string, paidAt time.Time) (claimed bool, err error)
	// ExpireInvoice atomically flips a pending invoice to expired and reports whether
	// THIS call won the transition. A paid invoice is never expired.
	ExpireInvoice(id string) (claimed bool, err error)
	// PendingCounts returns how many invoices are pending in total and for the given
	// receiving address, so creation can be bounded — an unbounded pending set is
	// unbounded verification work against toncenter.
	PendingCounts(payTo string) (total int, forAddr int, err error)
}
