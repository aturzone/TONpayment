package store

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

// opTimeout bounds every query so a stalled database cannot block a request handler
// or the poller goroutine indefinitely.
const opTimeout = 10 * time.Second

func opCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), opTimeout)
}

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
-- A memo must be unique per receiving address: it is the per-invoice key the
-- verifier matches within an address, so a collision could let one on-chain
-- payment settle two invoices. The unique index makes that impossible.
CREATE UNIQUE INDEX IF NOT EXISTS invoices_payto_memo ON invoices(pay_to, memo);
-- PendingCounts filters pending invoices by pay_to (the per-address cap); a
-- partial index keeps that from scanning every pending row as volume grows.
CREATE INDEX IF NOT EXISTS invoices_pending_payto ON invoices(pay_to) WHERE status='pending';
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
	ctx, cancel := opCtx()
	defer cancel()
	_, err := p.pool.Exec(ctx, `
INSERT INTO invoices (id,pay_to,memo,amount_nano,currency,status,tx_hash,metadata,created_at,paid_at,expires_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		inv.ID, inv.PayTo, inv.Memo, inv.AmountNano, inv.Currency, inv.Status, inv.TxHash, metaJSON(inv.Metadata), inv.CreatedAt, nt(inv.PaidAt), nt(inv.ExpiresAt))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
		return ErrMemoExists
	}
	return err
}

func (p *Postgres) GetInvoice(id string) (Invoice, bool) {
	ctx, cancel := opCtx()
	defer cancel()
	inv, err := scanInvoice(p.pool.QueryRow(ctx, `SELECT `+invoiceCols+` FROM invoices WHERE id=$1`, id))
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			log.Printf("store: GetInvoice %s: %v", id, err) // a real DB error, not just "not found"
		}
		return Invoice{}, false
	}
	return inv, true
}

func (p *Postgres) ListInvoices() []Invoice {
	ctx, cancel := opCtx()
	defer cancel()
	rows, err := p.pool.Query(ctx, `SELECT `+invoiceCols+` FROM invoices ORDER BY created_at DESC`)
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
	ctx, cancel := opCtx()
	defer cancel()
	rows, err := p.pool.Query(ctx, `SELECT `+invoiceCols+` FROM invoices WHERE status='pending' ORDER BY created_at`)
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

func (p *Postgres) PendingCounts(payTo string) (int, int, error) {
	ctx, cancel := opCtx()
	defer cancel()
	var total, forAddr int
	err := p.pool.QueryRow(ctx,
		`SELECT count(*), count(*) FILTER (WHERE pay_to=$1) FROM invoices WHERE status='pending'`,
		payTo).Scan(&total, &forAddr)
	if err != nil {
		return 0, 0, err
	}
	return total, forAddr, nil
}

func (p *Postgres) ClaimInvoiceForSettlement(id, txHash string, paidAt time.Time) (bool, error) {
	ctx, cancel := opCtx()
	defer cancel()
	tag, err := p.pool.Exec(ctx, `UPDATE invoices SET status='paid', tx_hash=$2, paid_at=$3 WHERE id=$1 AND status='pending'`,
		id, txHash, nt(paidAt))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (p *Postgres) ExpireInvoice(id string) (bool, error) {
	ctx, cancel := opCtx()
	defer cancel()
	tag, err := p.pool.Exec(ctx, `UPDATE invoices SET status='expired' WHERE id=$1 AND status='pending'`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
