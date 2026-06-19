package store

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres is a durable Store backed by PostgreSQL (pgx). It satisfies the same
// interface as the in-memory store, so the rest of the service is unchanged.
type Postgres struct {
	pool *pgxpool.Pool
}

var _ Store = (*Postgres)(nil)

// scanner is satisfied by both pgx.Row (QueryRow) and pgx.Rows (Query).
type scanner interface {
	Scan(dest ...any) error
}

// NewPostgres connects, verifies the connection, and applies the schema.
func NewPostgres(ctx context.Context, url string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	p := &Postgres{pool: pool}
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		pool.Close()
		return nil, err
	}
	return p, nil
}

func (p *Postgres) Close() { p.pool.Close() }

func bg() context.Context { return context.Background() }

// nt returns nil for a zero time so it stores as SQL NULL.
func nt(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// The poller queries ListPending repeatedly, so invoices are indexed on status.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS invoices (
  id text PRIMARY KEY,
  pay_to text NOT NULL DEFAULT '',
  memo text NOT NULL DEFAULT '',
  amount_nano bigint NOT NULL DEFAULT 0,
  currency text NOT NULL DEFAULT 'TON',
  status text NOT NULL DEFAULT 'pending',
  tx_hash text NOT NULL DEFAULT '',
  metadata jsonb NOT NULL DEFAULT '{}',
  created_at timestamptz NOT NULL DEFAULT now(),
  paid_at timestamptz,
  expires_at timestamptz
);
CREATE INDEX IF NOT EXISTS invoices_status ON invoices(status);
`

const invoiceCols = `id,pay_to,memo,amount_nano,currency,status,tx_hash,metadata,created_at,paid_at,expires_at`

func scanInvoice(s scanner) (Invoice, error) {
	var inv Invoice
	var meta []byte
	var paid, expires *time.Time
	if err := s.Scan(&inv.ID, &inv.PayTo, &inv.Memo, &inv.AmountNano, &inv.Currency, &inv.Status, &inv.TxHash, &meta, &inv.CreatedAt, &paid, &expires); err != nil {
		return Invoice{}, err
	}
	if len(meta) > 0 {
		_ = json.Unmarshal(meta, &inv.Metadata)
	}
	if paid != nil {
		inv.PaidAt = *paid
	}
	if expires != nil {
		inv.ExpiresAt = *expires
	}
	return inv, nil
}

func metaJSON(m map[string]string) string {
	if m == nil {
		return "{}"
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func (p *Postgres) CreateInvoice(inv Invoice) error {
	_, err := p.pool.Exec(bg(), `
INSERT INTO invoices (id,pay_to,memo,amount_nano,currency,status,tx_hash,metadata,created_at,paid_at,expires_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		inv.ID, inv.PayTo, inv.Memo, inv.AmountNano, inv.Currency, inv.Status, inv.TxHash, metaJSON(inv.Metadata), inv.CreatedAt, nt(inv.PaidAt), nt(inv.ExpiresAt))
	return err
}

func (p *Postgres) GetInvoice(id string) (Invoice, bool) {
	inv, err := scanInvoice(p.pool.QueryRow(bg(), `SELECT `+invoiceCols+` FROM invoices WHERE id=$1`, id))
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			log.Printf("store: GetInvoice %s: %v", id, err) // a real DB error, not just "not found"
		}
		return Invoice{}, false
	}
	return inv, true
}

func (p *Postgres) ListInvoices() []Invoice {
	rows, err := p.pool.Query(bg(), `SELECT `+invoiceCols+` FROM invoices ORDER BY created_at DESC`)
	if err != nil {
		log.Printf("store: ListInvoices: %v", err)
		return nil
	}
	defer rows.Close()
	out := []Invoice{}
	for rows.Next() {
		if inv, err := scanInvoice(rows); err == nil {
			out = append(out, inv)
		}
	}
	return out
}

func (p *Postgres) ListPending() []Invoice {
	rows, err := p.pool.Query(bg(), `SELECT `+invoiceCols+` FROM invoices WHERE status='pending' ORDER BY created_at`)
	if err != nil {
		log.Printf("store: ListPending: %v", err)
		return nil
	}
	defer rows.Close()
	out := []Invoice{}
	for rows.Next() {
		if inv, err := scanInvoice(rows); err == nil {
			out = append(out, inv)
		}
	}
	return out
}

func (p *Postgres) ClaimInvoiceForSettlement(id, txHash string, paidAt time.Time) (bool, error) {
	tag, err := p.pool.Exec(bg(), `UPDATE invoices SET status='paid', tx_hash=$2, paid_at=$3 WHERE id=$1 AND status='pending'`,
		id, txHash, nt(paidAt))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (p *Postgres) ExpireInvoice(id string) (bool, error) {
	tag, err := p.pool.Exec(bg(), `UPDATE invoices SET status='expired' WHERE id=$1 AND status='pending'`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
