// Package store defines the Invoice domain type and the persistence interface.
// An in-memory/JSON implementation lives in memory.go and a Postgres (pgx)
// implementation in postgres.go — both satisfy Store, so the rest of the service
// is agnostic to where invoices are kept.
package store

import "time"

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
}
