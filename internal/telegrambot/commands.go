package telegrambot

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/aturzone/TONpayment/internal/money"
	"github.com/aturzone/TONpayment/internal/store"
)

// commandStore is the full data access the bot needs (satisfied by
// *store.Postgres): the auth flow's persistence, the shared bot state, and the
// read/kill-switch queries behind the commands.
type commandStore interface {
	authStore
	botStateStore
	GetBotStats(ctx context.Context) (store.BotStats, error)
	ListGatewaysForBot(ctx context.Context, limit int) ([]store.BotGatewayRow, error)
	GetGatewayDetailForBot(ctx context.Context, slug string) (store.BotGatewayDetail, bool, error)
	ListRecentInvoicesForBot(ctx context.Context, limit int) ([]store.BotInvoiceRow, error)
	PendingSummaryForBot(ctx context.Context) (int, time.Time, error)
	GetGatewayBySlug(ctx context.Context, slug string) (store.Gateway, bool, error)
	UpdateGateway(ctx context.Context, g store.Gateway) error
	GetPlatformConfig(ctx context.Context) (store.PlatformConfig, error)
	Ping(ctx context.Context) error
}

// runCommand executes one already-authorized admin command and returns the
// HTML reply ("" = stay silent). Sensitive commands re-verify a FRESH TOTP
// inside their handler — enrollment alone is not enough for them.
func (b *Bot) runCommand(ctx context.Context, cmd string, args []string, fromID, chatID int64) string {
	switch cmd {
	case "help", "start":
		return replyAdminHelp
	case "stats":
		return b.cmdStats(ctx)
	case "links":
		return b.cmdLinks(ctx)
	case "link":
		return b.cmdLink(ctx, args)
	case "invoices":
		return b.cmdInvoices(ctx)
	case "pending":
		return b.cmdPending(ctx)
	case "health":
		return b.cmdHealth(ctx)
	case "fee":
		return b.cmdFee(ctx)
	case "setup":
		return b.cmdSetup(ctx, args, fromID, chatID)
	case "disable":
		return b.cmdSetActive(ctx, args, fromID, false)
	case "enable":
		return b.cmdSetActive(ctx, args, fromID, true)
	default:
		return "" // unknown command: stay silent (it may belong to another bot)
	}
}

func (b *Bot) cmdStats(ctx context.Context) string {
	s, err := b.store.GetBotStats(ctx)
	if err != nil {
		log.Printf("telegrambot: /stats failed: %v", err)
		return "⚠️ stats temporarily unavailable — try again shortly."
	}
	return fmt.Sprintf(
		"📊 <b>Platform stats</b>\n"+
			"Merchants: <b>%d</b>\n"+
			"Links: <b>%d</b> (%d donation · %d payment · %d active)\n"+
			"Invoices: <b>%d</b> paid · %d pending\n"+
			"Volume: <b>%s GRAM</b> all-time · %s GRAM last 24h (%d payments)",
		s.Merchants, s.Gateways, s.Donations, s.Payments, s.ActiveGateways,
		s.PaidInvoices, s.PendingInvoices,
		escapeHTML(money.NanoToTONString(s.PaidVolumeNano)),
		escapeHTML(money.NanoToTONString(s.Paid24hNano)), s.Paid24h)
}

func (b *Bot) cmdLinks(ctx context.Context) string {
	rows, err := b.store.ListGatewaysForBot(ctx, 15)
	if err != nil {
		log.Printf("telegrambot: /links failed: %v", err)
		return "⚠️ links temporarily unavailable — try again shortly."
	}
	if len(rows) == 0 {
		return "No links yet."
	}
	var sb strings.Builder
	sb.WriteString("🔗 <b>Recent links</b>\n")
	for _, r := range rows {
		state := "✅"
		if !r.Active {
			state = "⛔"
		}
		icon := "💳"
		if r.Kind == "donation" {
			icon = "☕"
		}
		fmt.Fprintf(&sb, "%s %s /%s → <code>%s</code>\n", state, icon, escapeHTML(r.Slug), escapeHTML(shorten(r.Wallet, 12)))
	}
	sb.WriteString("\n/link &lt;slug&gt; for detail")
	return sb.String()
}

func (b *Bot) cmdLink(ctx context.Context, args []string) string {
	if len(args) == 0 {
		return "Usage: <code>/link &lt;slug&gt;</code>"
	}
	slug := strings.TrimPrefix(strings.ToLower(args[0]), "/")
	d, ok, err := b.store.GetGatewayDetailForBot(ctx, slug)
	if err != nil {
		log.Printf("telegrambot: /link failed: %v", err)
		return "⚠️ link lookup failed — try again shortly."
	}
	if !ok {
		return "No link with slug <code>" + escapeHTML(slug) + "</code>."
	}
	g := d.Gateway
	state := "active ✅"
	if !g.Active {
		state = "disabled ⛔"
	}
	return fmt.Sprintf(
		"🔗 <b>/%s</b> (%s, %s)\n"+
			"Name: %s\n"+
			"Wallet: <code>%s</code>\n"+
			"Paid: <b>%d</b> invoices · <b>%s GRAM</b>\n"+
			"Open pending: %d\n"+
			"Created: %s",
		escapeHTML(g.Slug), escapeHTML(g.Kind), state,
		escapeHTML(orDash(g.DisplayName)),
		escapeHTML(g.ReceivingAddress),
		d.PaidCount, escapeHTML(money.NanoToTONString(d.PaidNano)),
		d.PendingOpen,
		g.CreatedAt.UTC().Format("2006-01-02"))
}

func (b *Bot) cmdInvoices(ctx context.Context) string {
	rows, err := b.store.ListRecentInvoicesForBot(ctx, 10)
	if err != nil {
		log.Printf("telegrambot: /invoices failed: %v", err)
		return "⚠️ invoices temporarily unavailable — try again shortly."
	}
	if len(rows) == 0 {
		return "No invoices yet."
	}
	var sb strings.Builder
	sb.WriteString("🧾 <b>Recent invoices</b>\n")
	for _, r := range rows {
		icon := map[string]string{"paid": "✅", "pending": "⏳", "expired": "⌛"}[r.Status]
		if icon == "" {
			icon = "•"
		}
		slug := r.Slug
		if slug == "" {
			slug = "-"
		}
		fmt.Fprintf(&sb, "%s %s GRAM · /%s · <code>%s</code>\n",
			icon, escapeHTML(money.NanoToTONString(r.AmountNano)), escapeHTML(slug), escapeHTML(shorten(r.ID, 14)))
	}
	return sb.String()
}

func (b *Bot) cmdPending(ctx context.Context) string {
	n, oldest, err := b.store.PendingSummaryForBot(ctx)
	if err != nil {
		log.Printf("telegrambot: /pending failed: %v", err)
		return "⚠️ pending summary unavailable — try again shortly."
	}
	if n == 0 {
		return "⏳ Pending backlog: <b>0</b> — all clear."
	}
	return fmt.Sprintf("⏳ Pending backlog: <b>%d</b>\nOldest: %s ago", n, escapeHTML(humanSince(oldest)))
}

func (b *Bot) cmdHealth(ctx context.Context) string {
	dbOK := "✅"
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if err := b.store.Ping(pingCtx); err != nil {
		dbOK = "❌ " + escapeHTML(err.Error())
	}
	cancel()
	s, err := b.store.GetBotStats(ctx)
	if err != nil {
		return "🩺 DB: " + dbOK + "\n⚠️ stats unavailable: read failed."
	}
	return fmt.Sprintf(
		"🩺 <b>Engine health</b>\n"+
			"DB: %s\n"+
			"Uptime: %s\n"+
			"Pending invoices: %d\n"+
			"Paid last 24h: %d (%s GRAM)\n"+
			"Webhook failures 24h: %d",
		dbOK, escapeHTML(humanSince(b.startedAt)),
		s.PendingInvoices, s.Paid24h, escapeHTML(money.NanoToTONString(s.Paid24hNano)), s.WebhookFails24h)
}

func (b *Bot) cmdFee(ctx context.Context) string {
	cfg, err := b.store.GetPlatformConfig(ctx)
	if err != nil {
		log.Printf("telegrambot: /fee failed: %v", err)
		return "⚠️ fee config unavailable — try again shortly."
	}
	wallet := cfg.FeeWallet
	if wallet == "" {
		wallet = "-"
	}
	return fmt.Sprintf("💰 Platform fee: <b>%.2f%%</b> (%d bps)\nFee wallet: <code>%s</code>",
		float64(cfg.FeeBps)/100, cfg.FeeBps, escapeHTML(wallet))
}

// cmdSetup binds THIS group as the admin group (first run) and creates the
// alert topics. Rebinding to a DIFFERENT group is sensitive → fresh TOTP.
func (b *Bot) cmdSetup(ctx context.Context, args []string, fromID, chatID int64) string {
	bound := b.notifier.boundChat()
	switch {
	case bound == 0:
		// First bind: the sender is already gated as an enrolled admin.
		if err := b.notifier.bindChat(ctx, chatID); err != nil {
			log.Printf("telegrambot: bind failed: %v", err)
			return "⚠️ could not persist the group binding — try again."
		}
	case bound != chatID:
		// Rebind = moving the alert firehose → require a fresh code.
		if len(args) == 0 || !b.auth.VerifyFreshTOTP(ctx, fromID, args[0]) {
			return "🚫 This bot is bound to another group. To rebind here run <code>/setup &lt;2fa-code&gt;</code>."
		}
		if err := b.notifier.bindChat(ctx, chatID); err != nil {
			log.Printf("telegrambot: rebind failed: %v", err)
			return "⚠️ could not persist the group binding — try again."
		}
	}

	// Create any missing topics (idempotent per category: existing ids are kept).
	topics := map[string]int64{}
	for cat, id := range b.notifier.snapshotTopics() {
		topics[cat] = id
	}
	created, failed := 0, 0
	for cat, name := range topicNames {
		if topics[cat] != 0 {
			continue
		}
		tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		id, err := b.client.createForumTopic(tctx, chatID, name)
		cancel()
		if err != nil {
			failed++
			log.Printf("telegrambot: createForumTopic %q failed: %v", name, err)
			continue
		}
		topics[cat] = id
		created++
	}
	if err := b.notifier.setTopics(ctx, topics); err != nil {
		log.Printf("telegrambot: persisting topics failed: %v", err)
	}
	if failed > 0 {
		return fmt.Sprintf("⚠️ Group bound, %d topic(s) created, %d failed — make sure I'm an admin with the <b>Manage topics</b> right, then run /setup again.", created, failed)
	}
	return fmt.Sprintf("✅ Group bound. %d topic(s) created (%d total): Payments, Links, Health, Critical alerts now flow to their topics.", created, len(topics))
}

// cmdSetActive is the sensitive link kill switch: /disable <slug> <code> and
// /enable <slug> <code>. Both require a FRESH one-time TOTP on top of the
// enrolled-admin gate — fail closed on anything less.
func (b *Bot) cmdSetActive(ctx context.Context, args []string, fromID int64, active bool) string {
	verb := "disable"
	if active {
		verb = "enable"
	}
	if len(args) < 2 {
		return "Usage: <code>/" + verb + " &lt;slug&gt; &lt;2fa-code&gt;</code>"
	}
	slug := strings.TrimPrefix(strings.ToLower(args[0]), "/")
	if !b.auth.VerifyFreshTOTP(ctx, fromID, args[1]) {
		return "🚫 Invalid or reused 2FA code. The link was NOT changed."
	}
	g, ok, err := b.store.GetGatewayBySlug(ctx, slug)
	if err != nil {
		log.Printf("telegrambot: /%s lookup failed: %v", verb, err)
		return "⚠️ lookup failed — the link was NOT changed."
	}
	if !ok {
		return "No link with slug <code>" + escapeHTML(slug) + "</code>."
	}
	if g.Active == active {
		return "Link /" + escapeHTML(slug) + " is already " + verb + "d."
	}
	g.Active = active
	if err := b.store.UpdateGateway(ctx, g); err != nil {
		log.Printf("telegrambot: /%s update failed: %v", verb, err)
		return "⚠️ update failed — the link was NOT changed."
	}
	b.notifier.LinkEvent(verb+"d (admin)", g)
	if active {
		return "✅ Link /" + escapeHTML(slug) + " is back ON."
	}
	return "⛔ Link /" + escapeHTML(slug) + " is now OFF — its page and new invoices are blocked."
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func humanSince(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
