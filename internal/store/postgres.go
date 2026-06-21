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

// MigrateTenant applies the multi-tenant control-plane schema (tenantSchemaSQL).
// It is idempotent and is called from main() only when multi-tenant mode is on, so
// a single-tenant database never grows these tables. Splitting it from the base
// schema (applied in NewPostgres) keeps OSS single-tenant deployments minimal.
func (p *Postgres) MigrateTenant(ctx context.Context) error {
	_, err := p.pool.Exec(ctx, tenantSchemaSQL)
	return err
}

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

// ns returns nil for an empty string so it stores as SQL NULL — keeping
// single-tenant rows out of the partial tenant indexes (WHERE merchant_id IS NOT
// NULL) and round-tripping back to "" via scanInvoice.
func ns(s string) any {
	if s == "" {
		return nil
	}
	return s
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
-- Tenancy columns (nullable; NULL in single-tenant/OSS mode). Added idempotently
-- so a database created before multi-tenancy gains them on the next boot. The
-- shared invoiceCols selects them, so they must exist in EVERY mode; their indexes
-- and the tenant tables, however, are created only under multi-tenancy (see
-- tenantSchemaSQL / MigrateTenant) since only that mode populates them.
ALTER TABLE invoices ADD COLUMN IF NOT EXISTS merchant_id text;
ALTER TABLE invoices ADD COLUMN IF NOT EXISTS gateway_id text;
ALTER TABLE invoices ADD COLUMN IF NOT EXISTS idem_key text;
CREATE INDEX IF NOT EXISTS invoices_status ON invoices(status);
-- A memo must be unique per receiving address: it is the per-invoice key the
-- verifier matches within an address, so a collision could let one on-chain
-- payment settle two invoices. The unique index makes that impossible.
CREATE UNIQUE INDEX IF NOT EXISTS invoices_payto_memo ON invoices(pay_to, memo);
-- PendingCounts filters pending invoices by pay_to (the per-address cap); a
-- partial index keeps that from scanning every pending row as volume grows.
CREATE INDEX IF NOT EXISTS invoices_pending_payto ON invoices(pay_to) WHERE status='pending';
`

// tenantSchemaSQL adds the multi-tenant control plane. It is applied by
// MigrateTenant only when TON_MULTITENANT=1, so a single-tenant (OSS) database
// never grows these tables. Every statement is idempotent. The cross-product rule
// — a wallet may run a donation link OR a payment gateway, never both — is enforced
// by wallet_ownership.address being a PRIMARY KEY: a wallet appears at most once
// across both products.
const tenantSchemaSQL = `
CREATE TABLE IF NOT EXISTS merchants (
  id         text PRIMARY KEY,
  wallet     text NOT NULL,
  status     text NOT NULL DEFAULT 'active',
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS merchants_wallet ON merchants(wallet);

CREATE TABLE IF NOT EXISTS gateways (
  id                text PRIMARY KEY,
  merchant_id       text NOT NULL REFERENCES merchants(id) ON DELETE CASCADE,
  kind              text NOT NULL DEFAULT 'payment',  -- 'donation' | 'payment'
  slug              text NOT NULL,
  display_name      text NOT NULL DEFAULT '',
  branding          jsonb NOT NULL DEFAULT '{}',
  contact           jsonb NOT NULL DEFAULT '{}',      -- PII for payment links; never public
  receiving_address text NOT NULL,
  active            boolean NOT NULL DEFAULT true,
  created_at        timestamptz NOT NULL DEFAULT now()
);
-- Evolve gateways created before the donation/payment unification.
ALTER TABLE gateways ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'payment';
ALTER TABLE gateways ADD COLUMN IF NOT EXISTS contact jsonb NOT NULL DEFAULT '{}';
CREATE UNIQUE INDEX IF NOT EXISTS gateways_slug ON gateways(slug);
CREATE INDEX IF NOT EXISTS gateways_merchant ON gateways(merchant_id);

CREATE TABLE IF NOT EXISTS api_keys (
  id          text PRIMARY KEY,
  merchant_id text NOT NULL REFERENCES merchants(id) ON DELETE CASCADE,
  key_hash    bytea NOT NULL,
  key_prefix  text NOT NULL,
  mode        text NOT NULL DEFAULT 'live',
  label       text NOT NULL DEFAULT '',
  created_at  timestamptz NOT NULL DEFAULT now(),
  revoked_at  timestamptz
);
CREATE UNIQUE INDEX IF NOT EXISTS api_keys_key_hash ON api_keys(key_hash);
CREATE INDEX IF NOT EXISTS api_keys_merchant ON api_keys(merchant_id);

CREATE TABLE IF NOT EXISTS webhook_endpoints (
  id         text PRIMARY KEY,
  gateway_id text NOT NULL REFERENCES gateways(id) ON DELETE CASCADE,
  url        text NOT NULL,
  secret     text NOT NULL,
  active     boolean NOT NULL DEFAULT true,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS webhook_endpoints_gateway ON webhook_endpoints(gateway_id) WHERE active;

CREATE TABLE IF NOT EXISTS webhook_deliveries (
  id          bigserial PRIMARY KEY,
  invoice_id  text NOT NULL,
  endpoint_id text NOT NULL,
  attempt     int  NOT NULL,
  status_code int  NOT NULL DEFAULT 0,
  ok          boolean NOT NULL DEFAULT false,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS webhook_deliveries_endpoint ON webhook_deliveries(endpoint_id, created_at DESC);
CREATE INDEX IF NOT EXISTS webhook_deliveries_invoice ON webhook_deliveries(invoice_id);

-- The cross-product uniqueness enforcer. address is the PRIMARY KEY, so a wallet
-- can be claimed by EITHER a donation link OR a payment gateway, never both.
CREATE TABLE IF NOT EXISTS wallet_ownership (
  address    text PRIMARY KEY,
  product    text NOT NULL,
  owner_id   text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT wallet_ownership_product_chk CHECK (product IN ('donation','payment'))
);

-- Single-row platform config. fee_bps/fee_wallet default to 0/'' so a self-hoster
-- gets a zero-fee gateway with nothing baked in; tonpayment.net sets them via admin.
CREATE TABLE IF NOT EXISTS platform_config (
  id         int PRIMARY KEY DEFAULT 1 CHECK (id = 1),
  fee_bps    int  NOT NULL DEFAULT 0,
  fee_wallet text NOT NULL DEFAULT '',
  updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO platform_config (id) VALUES (1) ON CONFLICT (id) DO NOTHING;

-- Uploaded link images (logos), kept on our own server. Bounded in size by the
-- upload handler; served public at /v1/asset/{id}.
CREATE TABLE IF NOT EXISTS assets (
  id           text PRIMARY KEY,
  merchant_id  text NOT NULL,
  content_type text NOT NULL,
  bytes        bytea NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS admin_audit (
  id         bigserial PRIMARY KEY,
  actor      text NOT NULL,
  action     text NOT NULL,
  target     text NOT NULL DEFAULT '',
  meta       jsonb NOT NULL DEFAULT '{}',
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS admin_audit_created ON admin_audit(created_at DESC);

-- ton_proof nonces: single-use (used_at) with a short TTL (expires_at).
CREATE TABLE IF NOT EXISTS auth_challenges (
  nonce      text PRIMARY KEY,
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  used_at    timestamptz
);
CREATE INDEX IF NOT EXISTS auth_challenges_expires ON auth_challenges(expires_at);

-- Tenant-scoped invoice indexes. The columns live in the base schema (invoiceCols
-- selects them in every mode); these partial indexes are created only here because
-- only multi-tenant mode populates merchant_id/gateway_id/idem_key.
CREATE INDEX IF NOT EXISTS invoices_merchant_status  ON invoices(merchant_id, status)          WHERE merchant_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS invoices_merchant_created ON invoices(merchant_id, created_at DESC) WHERE merchant_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS invoices_gateway_created  ON invoices(gateway_id, created_at DESC)  WHERE gateway_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS invoices_merchant_idem ON invoices(merchant_id, idem_key)    WHERE idem_key IS NOT NULL;
`

const invoiceCols = `id,pay_to,memo,amount_nano,currency,status,tx_hash,metadata,created_at,paid_at,expires_at,merchant_id,gateway_id`

func scanInvoice(s scanner) (Invoice, error) {
	var inv Invoice
	var meta []byte
	var paid, expires *time.Time
	var merchantID, gatewayID *string // nullable: NULL in single-tenant mode
	if err := s.Scan(&inv.ID, &inv.PayTo, &inv.Memo, &inv.AmountNano, &inv.Currency, &inv.Status, &inv.TxHash, &meta, &inv.CreatedAt, &paid, &expires, &merchantID, &gatewayID); err != nil {
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
	if merchantID != nil {
		inv.MerchantID = *merchantID
	}
	if gatewayID != nil {
		inv.GatewayID = *gatewayID
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
INSERT INTO invoices (id,pay_to,memo,amount_nano,currency,status,tx_hash,metadata,created_at,paid_at,expires_at,merchant_id,gateway_id)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		inv.ID, inv.PayTo, inv.Memo, inv.AmountNano, inv.Currency, inv.Status, inv.TxHash, metaJSON(inv.Metadata), inv.CreatedAt, nt(inv.PaidAt), nt(inv.ExpiresAt), ns(inv.MerchantID), ns(inv.GatewayID))
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
