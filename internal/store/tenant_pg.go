package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Postgres implements the multi-tenant control plane. These methods are only
// reachable in multi-tenant mode (the wiring in main.go gates it on
// TON_MULTITENANT=1 + a database), so the in-memory store never needs them.
var _ TenantStore = (*Postgres)(nil)

// bctx bounds a caller's context with the store timeout, so a stalled database
// cannot pin a request handler open while still honoring caller cancellation.
func (p *Postgres) bctx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, opTimeout)
}

// orNow defaults a zero time to the current UTC time, matching how the service
// stamps created_at before handing rows to the store.
func orNow(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().UTC()
	}
	return t
}

// jsonbObj marshals an opaque object to a JSON string for a jsonb column, falling
// back to an empty object so the column is never NULL.
func jsonbObj(m map[string]any) string {
	if m == nil {
		return "{}"
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// clampLimit keeps list queries bounded regardless of caller input.
func clampLimit(limit int) int {
	switch {
	case limit <= 0:
		return 50
	case limit > 200:
		return 200
	default:
		return limit
	}
}

// --- merchants ---

func scanMerchant(s scanner) (Merchant, error) {
	var m Merchant
	if err := s.Scan(&m.ID, &m.Wallet, &m.Status, &m.CreatedAt); err != nil {
		return Merchant{}, err
	}
	return m, nil
}

func (p *Postgres) CreateMerchant(ctx context.Context, m Merchant) error {
	ctx, cancel := p.bctx(ctx)
	defer cancel()
	if m.Status == "" {
		m.Status = MerchantActive
	}
	_, err := p.pool.Exec(ctx,
		`INSERT INTO merchants (id,wallet,status,created_at) VALUES ($1,$2,$3,$4)`,
		m.ID, m.Wallet, m.Status, orNow(m.CreatedAt))
	return err
}

func (p *Postgres) GetMerchant(ctx context.Context, id string) (Merchant, bool, error) {
	ctx, cancel := p.bctx(ctx)
	defer cancel()
	m, err := scanMerchant(p.pool.QueryRow(ctx, `SELECT id,wallet,status,created_at FROM merchants WHERE id=$1`, id))
	return foundMerchant(m, err)
}

func (p *Postgres) GetMerchantByWallet(ctx context.Context, wallet string) (Merchant, bool, error) {
	ctx, cancel := p.bctx(ctx)
	defer cancel()
	m, err := scanMerchant(p.pool.QueryRow(ctx, `SELECT id,wallet,status,created_at FROM merchants WHERE wallet=$1`, wallet))
	return foundMerchant(m, err)
}

func foundMerchant(m Merchant, err error) (Merchant, bool, error) {
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Merchant{}, false, nil
		}
		return Merchant{}, false, err
	}
	return m, true, nil
}

func (p *Postgres) ListMerchants(ctx context.Context, limit, offset int) ([]Merchant, error) {
	ctx, cancel := p.bctx(ctx)
	defer cancel()
	if offset < 0 {
		offset = 0
	}
	rows, err := p.pool.Query(ctx,
		`SELECT id,wallet,status,created_at FROM merchants ORDER BY created_at DESC LIMIT $1 OFFSET $2`,
		clampLimit(limit), offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Merchant{}
	for rows.Next() {
		m, err := scanMerchant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (p *Postgres) SetMerchantStatus(ctx context.Context, id, status string) error {
	ctx, cancel := p.bctx(ctx)
	defer cancel()
	_, err := p.pool.Exec(ctx, `UPDATE merchants SET status=$2 WHERE id=$1`, id, status)
	return err
}

// --- gateways ---

func scanGateway(s scanner) (Gateway, error) {
	var g Gateway
	var branding, contact []byte
	if err := s.Scan(&g.ID, &g.MerchantID, &g.Kind, &g.Slug, &g.DisplayName, &branding, &contact, &g.ReceivingAddress, &g.Active, &g.CreatedAt); err != nil {
		return Gateway{}, err
	}
	if len(branding) > 0 {
		_ = json.Unmarshal(branding, &g.Branding)
	}
	if len(contact) > 0 {
		_ = json.Unmarshal(contact, &g.Contact)
	}
	return g, nil
}

const gatewayCols = `id,merchant_id,kind,slug,display_name,branding,contact,receiving_address,active,created_at`

func (p *Postgres) CreateGateway(ctx context.Context, g Gateway) error {
	ctx, cancel := p.bctx(ctx)
	defer cancel()
	if g.Kind == "" {
		g.Kind = ProductPayment
	}
	_, err := p.pool.Exec(ctx,
		`INSERT INTO gateways (id,merchant_id,kind,slug,display_name,branding,contact,receiving_address,active,created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		g.ID, g.MerchantID, g.Kind, g.Slug, g.DisplayName, jsonbObj(g.Branding), jsonbObj(g.Contact), g.ReceivingAddress, g.Active, orNow(g.CreatedAt))
	return err
}

func (p *Postgres) GetGateway(ctx context.Context, id string) (Gateway, bool, error) {
	ctx, cancel := p.bctx(ctx)
	defer cancel()
	g, err := scanGateway(p.pool.QueryRow(ctx, `SELECT `+gatewayCols+` FROM gateways WHERE id=$1`, id))
	return foundGateway(g, err)
}

func (p *Postgres) GetGatewayBySlug(ctx context.Context, slug string) (Gateway, bool, error) {
	ctx, cancel := p.bctx(ctx)
	defer cancel()
	g, err := scanGateway(p.pool.QueryRow(ctx, `SELECT `+gatewayCols+` FROM gateways WHERE slug=$1`, slug))
	return foundGateway(g, err)
}

func foundGateway(g Gateway, err error) (Gateway, bool, error) {
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Gateway{}, false, nil
		}
		return Gateway{}, false, err
	}
	return g, true, nil
}

func (p *Postgres) ListGatewaysByMerchant(ctx context.Context, merchantID string) ([]Gateway, error) {
	ctx, cancel := p.bctx(ctx)
	defer cancel()
	rows, err := p.pool.Query(ctx, `SELECT `+gatewayCols+` FROM gateways WHERE merchant_id=$1 ORDER BY created_at DESC`, merchantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Gateway{}
	for rows.Next() {
		g, err := scanGateway(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (p *Postgres) UpdateGateway(ctx context.Context, g Gateway) error {
	ctx, cancel := p.bctx(ctx)
	defer cancel()
	_, err := p.pool.Exec(ctx,
		`UPDATE gateways SET display_name=$2, branding=$3, active=$4, contact=$5 WHERE id=$1`,
		g.ID, g.DisplayName, jsonbObj(g.Branding), g.Active, jsonbObj(g.Contact))
	return err
}

// --- api keys ---

func scanAPIKey(s scanner) (APIKey, error) {
	var k APIKey
	var revoked *time.Time
	if err := s.Scan(&k.ID, &k.MerchantID, &k.KeyHash, &k.KeyPrefix, &k.Mode, &k.Label, &k.CreatedAt, &revoked); err != nil {
		return APIKey{}, err
	}
	k.RevokedAt = revoked
	return k, nil
}

const apiKeyCols = `id,merchant_id,key_hash,key_prefix,mode,label,created_at,revoked_at`

func (p *Postgres) CreateAPIKey(ctx context.Context, k APIKey) error {
	ctx, cancel := p.bctx(ctx)
	defer cancel()
	if k.Mode == "" {
		k.Mode = KeyModeLive
	}
	_, err := p.pool.Exec(ctx,
		`INSERT INTO api_keys (id,merchant_id,key_hash,key_prefix,mode,label,created_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		k.ID, k.MerchantID, k.KeyHash, k.KeyPrefix, k.Mode, k.Label, orNow(k.CreatedAt))
	return err
}

func (p *Postgres) GetAPIKeyByHash(ctx context.Context, hash []byte) (APIKey, bool, error) {
	ctx, cancel := p.bctx(ctx)
	defer cancel()
	k, err := scanAPIKey(p.pool.QueryRow(ctx, `SELECT `+apiKeyCols+` FROM api_keys WHERE key_hash=$1`, hash))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return APIKey{}, false, nil
		}
		return APIKey{}, false, err
	}
	return k, true, nil
}

func (p *Postgres) ListAPIKeysByMerchant(ctx context.Context, merchantID string) ([]APIKey, error) {
	ctx, cancel := p.bctx(ctx)
	defer cancel()
	rows, err := p.pool.Query(ctx, `SELECT `+apiKeyCols+` FROM api_keys WHERE merchant_id=$1 ORDER BY created_at DESC`, merchantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIKey{}
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (p *Postgres) RevokeAPIKey(ctx context.Context, id string, at time.Time) error {
	ctx, cancel := p.bctx(ctx)
	defer cancel()
	_, err := p.pool.Exec(ctx, `UPDATE api_keys SET revoked_at=$2 WHERE id=$1 AND revoked_at IS NULL`, id, orNow(at))
	return err
}

// --- webhooks ---

func scanWebhookEndpoint(s scanner) (WebhookEndpoint, error) {
	var e WebhookEndpoint
	if err := s.Scan(&e.ID, &e.GatewayID, &e.URL, &e.Secret, &e.Active, &e.CreatedAt); err != nil {
		return WebhookEndpoint{}, err
	}
	return e, nil
}

const webhookCols = `id,gateway_id,url,secret,active,created_at`

func (p *Postgres) CreateWebhookEndpoint(ctx context.Context, e WebhookEndpoint) error {
	ctx, cancel := p.bctx(ctx)
	defer cancel()
	_, err := p.pool.Exec(ctx,
		`INSERT INTO webhook_endpoints (id,gateway_id,url,secret,active,created_at) VALUES ($1,$2,$3,$4,$5,$6)`,
		e.ID, e.GatewayID, e.URL, e.Secret, e.Active, orNow(e.CreatedAt))
	return err
}

func (p *Postgres) listWebhookEndpoints(ctx context.Context, gatewayID string, activeOnly bool) ([]WebhookEndpoint, error) {
	ctx, cancel := p.bctx(ctx)
	defer cancel()
	q := `SELECT ` + webhookCols + ` FROM webhook_endpoints WHERE gateway_id=$1`
	if activeOnly {
		q += ` AND active`
	}
	q += ` ORDER BY created_at DESC`
	rows, err := p.pool.Query(ctx, q, gatewayID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WebhookEndpoint{}
	for rows.Next() {
		e, err := scanWebhookEndpoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (p *Postgres) ListWebhookEndpoints(ctx context.Context, gatewayID string) ([]WebhookEndpoint, error) {
	return p.listWebhookEndpoints(ctx, gatewayID, false)
}

func (p *Postgres) ListActiveWebhookEndpoints(ctx context.Context, gatewayID string) ([]WebhookEndpoint, error) {
	return p.listWebhookEndpoints(ctx, gatewayID, true)
}

func (p *Postgres) RecordWebhookDelivery(ctx context.Context, invoiceID, endpointID string, attempt, statusCode int, ok bool) error {
	ctx, cancel := p.bctx(ctx)
	defer cancel()
	_, err := p.pool.Exec(ctx,
		`INSERT INTO webhook_deliveries (invoice_id,endpoint_id,attempt,status_code,ok) VALUES ($1,$2,$3,$4,$5)`,
		invoiceID, endpointID, attempt, statusCode, ok)
	return err
}

// --- tenant-scoped invoice reads ---

func (p *Postgres) GetInvoiceForMerchant(ctx context.Context, id, merchantID string) (Invoice, bool, error) {
	ctx, cancel := p.bctx(ctx)
	defer cancel()
	inv, err := scanInvoice(p.pool.QueryRow(ctx, `SELECT `+invoiceCols+` FROM invoices WHERE id=$1 AND merchant_id=$2`, id, merchantID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Invoice{}, false, nil // cross-tenant or unknown id: indistinguishable, by design
		}
		return Invoice{}, false, err
	}
	return inv, true, nil
}

func (p *Postgres) ListInvoicesByMerchant(ctx context.Context, merchantID string, limit, offset int) ([]Invoice, error) {
	ctx, cancel := p.bctx(ctx)
	defer cancel()
	if offset < 0 {
		offset = 0
	}
	rows, err := p.pool.Query(ctx,
		`SELECT `+invoiceCols+` FROM invoices WHERE merchant_id=$1 ORDER BY created_at DESC LIMIT $2 OFFSET $3`,
		merchantID, clampLimit(limit), offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Invoice{}
	for rows.Next() {
		inv, err := scanInvoice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// --- wallet ownership ---

func (p *Postgres) GetWalletOwner(ctx context.Context, address string) (WalletOwner, bool, error) {
	ctx, cancel := p.bctx(ctx)
	defer cancel()
	var w WalletOwner
	err := p.pool.QueryRow(ctx, `SELECT address,product,owner_id,created_at FROM wallet_ownership WHERE address=$1`, address).
		Scan(&w.Address, &w.Product, &w.OwnerID, &w.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return WalletOwner{}, false, nil
		}
		return WalletOwner{}, false, err
	}
	return w, true, nil
}

func (p *Postgres) ClaimWallet(ctx context.Context, w WalletOwner) error {
	ctx, cancel := p.bctx(ctx)
	defer cancel()
	_, err := p.pool.Exec(ctx,
		`INSERT INTO wallet_ownership (address,product,owner_id,created_at) VALUES ($1,$2,$3,$4)`,
		w.Address, w.Product, w.OwnerID, orNow(w.CreatedAt))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // primary-key conflict on address
		return ErrWalletTaken
	}
	return err
}

func (p *Postgres) ReleaseWallet(ctx context.Context, address, ownerID string) error {
	ctx, cancel := p.bctx(ctx)
	defer cancel()
	_, err := p.pool.Exec(ctx, `DELETE FROM wallet_ownership WHERE address=$1 AND owner_id=$2`, address, ownerID)
	return err
}

// --- platform config ---

func (p *Postgres) GetPlatformConfig(ctx context.Context) (PlatformConfig, error) {
	ctx, cancel := p.bctx(ctx)
	defer cancel()
	var c PlatformConfig
	err := p.pool.QueryRow(ctx, `SELECT fee_bps,fee_wallet,updated_at FROM platform_config WHERE id=1`).
		Scan(&c.FeeBps, &c.FeeWallet, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PlatformConfig{}, nil // not seeded yet: zero fee
	}
	return c, err
}

func (p *Postgres) SetPlatformConfig(ctx context.Context, feeBps int, feeWallet string) error {
	ctx, cancel := p.bctx(ctx)
	defer cancel()
	_, err := p.pool.Exec(ctx,
		`INSERT INTO platform_config (id,fee_bps,fee_wallet,updated_at) VALUES (1,$1,$2,now())
		 ON CONFLICT (id) DO UPDATE SET fee_bps=excluded.fee_bps, fee_wallet=excluded.fee_wallet, updated_at=now()`,
		feeBps, feeWallet)
	return err
}

// --- assets ---

func (p *Postgres) CreateAsset(ctx context.Context, a Asset) error {
	ctx, cancel := p.bctx(ctx)
	defer cancel()
	_, err := p.pool.Exec(ctx,
		`INSERT INTO assets (id,merchant_id,content_type,bytes,created_at) VALUES ($1,$2,$3,$4,$5)`,
		a.ID, a.MerchantID, a.ContentType, a.Bytes, orNow(a.CreatedAt))
	return err
}

func (p *Postgres) CountAssetsByMerchant(ctx context.Context, merchantID string) (int, error) {
	ctx, cancel := p.bctx(ctx)
	defer cancel()
	var n int
	err := p.pool.QueryRow(ctx, `SELECT count(*) FROM assets WHERE merchant_id=$1`, merchantID).Scan(&n)
	return n, err
}

func (p *Postgres) GetAsset(ctx context.Context, id string) (Asset, bool, error) {
	ctx, cancel := p.bctx(ctx)
	defer cancel()
	var a Asset
	err := p.pool.QueryRow(ctx, `SELECT id,merchant_id,content_type,bytes,created_at FROM assets WHERE id=$1`, id).
		Scan(&a.ID, &a.MerchantID, &a.ContentType, &a.Bytes, &a.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Asset{}, false, nil
		}
		return Asset{}, false, err
	}
	return a, true, nil
}

// --- audit + challenges ---

func (p *Postgres) AppendAudit(ctx context.Context, actor, action, target string, meta map[string]any) error {
	ctx, cancel := p.bctx(ctx)
	defer cancel()
	_, err := p.pool.Exec(ctx,
		`INSERT INTO admin_audit (actor,action,target,meta) VALUES ($1,$2,$3,$4)`,
		actor, action, target, jsonbObj(meta))
	return err
}

func (p *Postgres) PutChallenge(ctx context.Context, nonce string, ttl time.Duration) error {
	ctx, cancel := p.bctx(ctx)
	defer cancel()
	_, err := p.pool.Exec(ctx,
		`INSERT INTO auth_challenges (nonce,expires_at) VALUES ($1, now() + make_interval(secs => $2))`,
		nonce, ttl.Seconds())
	return err
}

func (p *Postgres) ConsumeChallenge(ctx context.Context, nonce string) (bool, error) {
	ctx, cancel := p.bctx(ctx)
	defer cancel()
	// Single-use + unexpired, atomically: the UPDATE claims the row only if it has
	// not been used and has not expired. RowsAffected==1 means THIS call won.
	tag, err := p.pool.Exec(ctx,
		`UPDATE auth_challenges SET used_at=now() WHERE nonce=$1 AND used_at IS NULL AND expires_at > now()`,
		nonce)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
