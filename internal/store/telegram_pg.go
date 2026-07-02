package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// TelegramAdmin is one enrolled (or enrolling) bot admin. Enrollment is a
// two-step DM flow: a verified phone share stages an encrypted TOTP secret
// (enrolled=false), and a correct 6-digit code flips enrolled=true. The failed
// attempt counter + lock timestamp implement the brute-force lockout; both are
// persisted so a restart cannot reset an in-progress lockout.
type TelegramAdmin struct {
	TelegramUserID     int64
	Phone              string
	TOTPSecretEnc      string
	Enrolled           bool
	FailedTOTPAttempts int
	TOTPLockedUntil    time.Time // zero = not locked
	UpdatedAt          time.Time
}

// telegramSchemaSQL is applied by MigrateTelegram only when the Telegram admin
// bot is configured, so deployments without the bot never grow these tables.
// Every statement is idempotent.
const telegramSchemaSQL = `
CREATE TABLE IF NOT EXISTS telegram_admins (
  telegram_user_id bigint PRIMARY KEY,
  phone            text NOT NULL,
  totp_secret_enc  text NOT NULL DEFAULT '',
  enrolled         boolean NOT NULL DEFAULT false,
  failed_totp_attempts int NOT NULL DEFAULT 0,
  totp_locked_until timestamptz,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now()
);

-- Small keyed state the bot persists across restarts: the getUpdates offset,
-- the bound group chat id, and the forum-topic thread map (JSON).
CREATE TABLE IF NOT EXISTS telegram_bot_state (
  key        text PRIMARY KEY,
  value      text NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);

-- One-time-use claims of verified TOTP codes (the money-command replay guard).
-- A row means "this admin already used this code"; rows expire shortly after
-- the code itself stops verifying and are purged opportunistically.
CREATE TABLE IF NOT EXISTS telegram_totp_used (
  telegram_user_id bigint NOT NULL,
  code             text NOT NULL,
  expires_at       timestamptz NOT NULL,
  PRIMARY KEY (telegram_user_id, code)
);
`

// MigrateTelegram applies the Telegram bot schema. Idempotent; called from
// main() only when the bot is configured.
func (p *Postgres) MigrateTelegram(ctx context.Context) error {
	_, err := p.pool.Exec(ctx, telegramSchemaSQL)
	return err
}

// GetTelegramAdmin loads one admin row. (found=false, err=nil) when absent.
func (p *Postgres) GetTelegramAdmin(ctx context.Context, telegramUserID int64) (TelegramAdmin, bool, error) {
	var a TelegramAdmin
	var locked *time.Time
	err := p.pool.QueryRow(ctx, `
SELECT telegram_user_id, phone, totp_secret_enc, enrolled, failed_totp_attempts, totp_locked_until, updated_at
FROM telegram_admins WHERE telegram_user_id=$1`, telegramUserID).
		Scan(&a.TelegramUserID, &a.Phone, &a.TOTPSecretEnc, &a.Enrolled, &a.FailedTOTPAttempts, &locked, &a.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TelegramAdmin{}, false, nil
		}
		return TelegramAdmin{}, false, err
	}
	if locked != nil {
		a.TOTPLockedUntil = *locked
	}
	return a, true, nil
}

// UpsertStagedTelegramAdmin stages (or re-stages) an enrollment: a fresh
// encrypted TOTP secret with enrolled=false. Never called for an already
// enrolled row (the auth layer checks first).
func (p *Postgres) UpsertStagedTelegramAdmin(ctx context.Context, telegramUserID int64, phone, totpSecretEnc string) error {
	_, err := p.pool.Exec(ctx, `
INSERT INTO telegram_admins (telegram_user_id, phone, totp_secret_enc, enrolled, updated_at)
VALUES ($1,$2,$3,false,now())
ON CONFLICT (telegram_user_id)
DO UPDATE SET phone=$2, totp_secret_enc=$3, enrolled=false, updated_at=now()`,
		telegramUserID, phone, totpSecretEnc)
	return err
}

// MarkTelegramAdminEnrolled completes enrollment after a verified 6-digit code.
func (p *Postgres) MarkTelegramAdminEnrolled(ctx context.Context, telegramUserID int64) error {
	_, err := p.pool.Exec(ctx,
		`UPDATE telegram_admins SET enrolled=true, updated_at=now() WHERE telegram_user_id=$1`, telegramUserID)
	return err
}

// RecordTelegramTOTPFailure increments the bad-code counter and, when lock is
// non-zero, arms the lockout until that time.
func (p *Postgres) RecordTelegramTOTPFailure(ctx context.Context, telegramUserID int64, attempts int, lock time.Time) error {
	_, err := p.pool.Exec(ctx, `
UPDATE telegram_admins SET failed_totp_attempts=$2, totp_locked_until=$3, updated_at=now()
WHERE telegram_user_id=$1`, telegramUserID, attempts, nt(lock))
	return err
}

// ResetTelegramTOTPFailures clears the counter + lock after a good code.
func (p *Postgres) ResetTelegramTOTPFailures(ctx context.Context, telegramUserID int64) error {
	_, err := p.pool.Exec(ctx, `
UPDATE telegram_admins SET failed_totp_attempts=0, totp_locked_until=NULL, updated_at=now()
WHERE telegram_user_id=$1`, telegramUserID)
	return err
}

// ClaimTelegramTOTPCode atomically claims (admin, code) for one-time use.
// Returns true when THIS call won the claim (first use); false when the same
// code was already consumed (replay). Expired claims are purged first so the
// table stays tiny.
func (p *Postgres) ClaimTelegramTOTPCode(ctx context.Context, telegramUserID int64, code string, ttl time.Duration) (bool, error) {
	_, _ = p.pool.Exec(ctx, `DELETE FROM telegram_totp_used WHERE expires_at < now()`)
	tag, err := p.pool.Exec(ctx, `
INSERT INTO telegram_totp_used (telegram_user_id, code, expires_at)
VALUES ($1,$2,now() + make_interval(secs => $3))
ON CONFLICT DO NOTHING`, telegramUserID, code, ttl.Seconds())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// GetBotState reads one bot state value ("" when unset).
func (p *Postgres) GetBotState(ctx context.Context, key string) (string, error) {
	var v string
	err := p.pool.QueryRow(ctx, `SELECT value FROM telegram_bot_state WHERE key=$1`, key).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetBotState upserts one bot state value.
func (p *Postgres) SetBotState(ctx context.Context, key, value string) error {
	_, err := p.pool.Exec(ctx, `
INSERT INTO telegram_bot_state (key, value, updated_at) VALUES ($1,$2,now())
ON CONFLICT (key) DO UPDATE SET value=$2, updated_at=now()`, key, value)
	return err
}

// BotStats is the platform-wide snapshot behind the bot's /stats and /health.
type BotStats struct {
	Merchants       int
	Gateways        int
	Donations       int
	Payments        int
	ActiveGateways  int
	PendingInvoices int
	PaidInvoices    int
	PaidVolumeNano  int64
	Paid24h         int
	Paid24hNano     int64
	WebhookFails24h int
}

// GetBotStats aggregates the platform snapshot in one round-trip.
func (p *Postgres) GetBotStats(ctx context.Context) (BotStats, error) {
	var s BotStats
	err := p.pool.QueryRow(ctx, `
SELECT
 (SELECT count(*) FROM merchants),
 (SELECT count(*) FROM gateways),
 (SELECT count(*) FROM gateways WHERE kind='donation'),
 (SELECT count(*) FROM gateways WHERE kind='payment'),
 (SELECT count(*) FROM gateways WHERE active),
 (SELECT count(*) FROM invoices WHERE status='pending'),
 (SELECT count(*) FROM invoices WHERE status='paid'),
 (SELECT coalesce(sum(amount_nano),0) FROM invoices WHERE status='paid'),
 (SELECT count(*) FROM invoices WHERE status='paid' AND paid_at > now() - interval '24 hours'),
 (SELECT coalesce(sum(amount_nano),0) FROM invoices WHERE status='paid' AND paid_at > now() - interval '24 hours'),
 (SELECT count(*) FROM webhook_deliveries WHERE NOT ok AND created_at > now() - interval '24 hours')`).
		Scan(&s.Merchants, &s.Gateways, &s.Donations, &s.Payments, &s.ActiveGateways,
			&s.PendingInvoices, &s.PaidInvoices, &s.PaidVolumeNano, &s.Paid24h, &s.Paid24hNano, &s.WebhookFails24h)
	return s, err
}

// BotGatewayRow is one line of the bot's /links listing.
type BotGatewayRow struct {
	Slug        string
	Kind        string
	Active      bool
	DisplayName string
	Wallet      string
	CreatedAt   time.Time
}

// ListGatewaysForBot lists the most recent links with their owner wallet.
func (p *Postgres) ListGatewaysForBot(ctx context.Context, limit int) ([]BotGatewayRow, error) {
	rows, err := p.pool.Query(ctx, `
SELECT g.slug, g.kind, g.active, g.display_name, m.wallet, g.created_at
FROM gateways g JOIN merchants m ON m.id = g.merchant_id
ORDER BY g.created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BotGatewayRow{}
	for rows.Next() {
		var r BotGatewayRow
		if err := rows.Scan(&r.Slug, &r.Kind, &r.Active, &r.DisplayName, &r.Wallet, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// BotGatewayDetail is the /link <slug> drill-down: the link plus its invoice
// aggregates and the owning merchant's wallet.
type BotGatewayDetail struct {
	Gateway     Gateway
	Wallet      string
	PaidCount   int
	PaidNano    int64
	PendingOpen int
}

// GetGatewayDetailForBot loads one link by slug with aggregates.
func (p *Postgres) GetGatewayDetailForBot(ctx context.Context, slug string) (BotGatewayDetail, bool, error) {
	g, ok, err := p.GetGatewayBySlug(ctx, slug)
	if err != nil || !ok {
		return BotGatewayDetail{}, ok, err
	}
	d := BotGatewayDetail{Gateway: g}
	if m, ok, err := p.GetMerchant(ctx, g.MerchantID); err == nil && ok {
		d.Wallet = m.Wallet
	}
	err = p.pool.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE status='paid'),
       coalesce(sum(amount_nano) FILTER (WHERE status='paid'),0),
       count(*) FILTER (WHERE status='pending')
FROM invoices WHERE gateway_id=$1`, g.ID).Scan(&d.PaidCount, &d.PaidNano, &d.PendingOpen)
	if err != nil {
		return BotGatewayDetail{}, false, err
	}
	return d, true, nil
}

// BotInvoiceRow is one line of the bot's /invoices listing.
type BotInvoiceRow struct {
	ID         string
	AmountNano int64
	Status     string
	Slug       string
	CreatedAt  time.Time
}

// ListRecentInvoicesForBot lists the newest invoices with their link slug.
func (p *Postgres) ListRecentInvoicesForBot(ctx context.Context, limit int) ([]BotInvoiceRow, error) {
	rows, err := p.pool.Query(ctx, `
SELECT i.id, i.amount_nano, i.status, coalesce(g.slug,''), i.created_at
FROM invoices i LEFT JOIN gateways g ON g.id = i.gateway_id
ORDER BY i.created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BotInvoiceRow{}
	for rows.Next() {
		var r BotInvoiceRow
		if err := rows.Scan(&r.ID, &r.AmountNano, &r.Status, &r.Slug, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PendingSummaryForBot returns the pending backlog size and the age of its
// oldest invoice (zero when the backlog is empty).
func (p *Postgres) PendingSummaryForBot(ctx context.Context) (int, time.Time, error) {
	var n int
	var oldest *time.Time
	err := p.pool.QueryRow(ctx,
		`SELECT count(*), min(created_at) FROM invoices WHERE status='pending'`).Scan(&n, &oldest)
	if err != nil {
		return 0, time.Time{}, err
	}
	if oldest == nil {
		return n, time.Time{}, nil
	}
	return n, *oldest, nil
}
