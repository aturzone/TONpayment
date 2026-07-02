// Package telegrambot is the ADMIN Telegram bot for the TONpayment platform:
// a Verdex-style operations bot that (a) streams platform events — payments,
// link changes, health, security — into the forum topics of ONE admin group,
// and (b) answers admin commands there.
//
// SECURITY MODEL (all fail-closed):
//   - Commands are answered ONLY in the bound admin group, ONLY for a sender
//     who completed DM enrollment: sharing their OWN Telegram-verified phone
//     number (which must be on the TON_TELEGRAM_ADMIN_PHONES allow-list) and
//     confirming a TOTP secret. Everyone else is silently ignored.
//   - Sensitive commands (/disable, /enable, /setup rebind) additionally need
//     a FRESH one-time 6-digit TOTP code on every use.
//   - The bot uses getUpdates long-polling — NEVER a webhook (they are
//     mutually exclusive; Run deletes any stale webhook first).
package telegrambot

import (
	"context"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Bot wires the transport, auth gate, notifier and command handlers together.
type Bot struct {
	client    *client
	store     commandStore
	auth      *authService
	notifier  *notifier
	envChatID int64 // optional TON_TELEGRAM_CHAT_ID pin; 0 = bind via /setup
	username  string
	startedAt time.Time
	dmLimiter dmLimiter
}

// dmLimiter is a tiny fixed-window rate limit on DM handling per sender, so a
// stranger cannot use the public DM surface to hammer the database or make the
// bot spam replies. Enrollment needs only a handful of messages, so the cap is
// generous for the owner and useless for a flooder.
type dmLimiter struct {
	mu      sync.Mutex
	windows map[int64]*dmWindow
}

type dmWindow struct {
	start time.Time
	count int
}

const (
	dmWindowLen = time.Minute
	dmWindowCap = 8 // replies per sender per window
	dmMapCap    = 10_000
)

// allow reports whether this sender may be handled right now.
func (l *dmLimiter) allow(fromID int64, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.windows == nil {
		l.windows = map[int64]*dmWindow{}
	}
	// Bound the map: under a distributed flood, reset rather than grow.
	if len(l.windows) > dmMapCap {
		l.windows = map[int64]*dmWindow{}
	}
	w := l.windows[fromID]
	if w == nil || now.Sub(w.start) > dmWindowLen {
		l.windows[fromID] = &dmWindow{start: now, count: 1}
		return true
	}
	w.count++
	return w.count <= dmWindowCap
}

// Config carries the bot's runtime settings.
type Config struct {
	Token        string   // bot token (required)
	AdminPhones  []string // allow-listed enrollment phone numbers (required)
	ChatID       int64    // optional explicit admin group id (else /setup binds)
	MasterSecret string   // at-rest encryption key source (TON_SESSION_SECRET)
}

// New builds the bot. store must be the Postgres store (the bot requires
// durable state); returns an error only for unusable configuration.
func New(cfg Config, st commandStore) (*Bot, error) {
	cipher, err := newSecretCipher(cfg.MasterSecret)
	if err != nil {
		return nil, err
	}
	c := newClient(cfg.Token)
	n := newNotifier(c, st)
	b := &Bot{
		client:    c,
		store:     st,
		notifier:  n,
		envChatID: cfg.ChatID,
		startedAt: time.Now(),
	}
	b.auth = newAuthService(st, cipher, cfg.AdminPhones, n.Notify)
	return b, nil
}

// Notifier exposes the event sink for the settlement/link hooks in main.
func (b *Bot) Notifier() *notifier { return b.notifier }

// snapshotTopics returns a copy of the current topic map (for /setup merging).
func (n *notifier) snapshotTopics() map[string]int64 {
	n.mu.RLock()
	defer n.mu.RUnlock()
	out := make(map[string]int64, len(n.topics))
	for k, v := range n.topics {
		out[k] = v
	}
	return out
}

// Run is the bot main loop; it blocks until ctx is cancelled. Everything
// inside is defensive: a Telegram outage, a DB blip or one malformed update
// can only ever delay processing, never crash the engine.
func (b *Bot) Run(ctx context.Context) {
	// Restore persisted state, apply the env chat pin (which wins over any
	// previously persisted binding), and clear any stale webhook.
	b.notifier.loadState(ctx)
	if b.envChatID != 0 {
		if err := b.notifier.bindChat(ctx, b.envChatID); err != nil {
			log.Printf("telegrambot: pinning chat from env failed: %v", err)
		}
	}
	initCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	if err := b.client.deleteWebhook(initCtx); err != nil {
		log.Printf("telegrambot: deleteWebhook failed (getUpdates may 409 until resolved): %v", err)
	}
	if name, err := b.client.getMe(initCtx); err == nil {
		b.username = name
		log.Printf("telegrambot: running as @%s (long-poll)", name)
	} else {
		log.Printf("telegrambot: getMe failed (check TON_TELEGRAM_BOT_TOKEN): %v", err)
	}
	cancel()

	go b.notifier.run(ctx)
	go b.healthTicker(ctx)

	offset := b.loadOffset(ctx)
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			b.notifier.Wait()
			return
		}
		// Recover our identity if boot-time getMe failed: @-suffixed commands
		// are refused (fail closed) until the username is known.
		if b.username == "" {
			mctx, mcancel := context.WithTimeout(ctx, 10*time.Second)
			if name, err := b.client.getMe(mctx); err == nil && name != "" {
				b.username = name
				log.Printf("telegrambot: identity recovered: @%s", name)
			}
			mcancel()
		}
		pollCtx, cancel := context.WithTimeout(ctx, (longPollSeconds+10)*time.Second)
		updates, err := b.client.getUpdates(pollCtx, offset)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				b.notifier.Wait()
				return
			}
			// 409 = a webhook re-appeared (or another poller); try clearing it.
			if strings.Contains(err.Error(), "409") {
				dctx, dcancel := context.WithTimeout(ctx, 10*time.Second)
				_ = b.client.deleteWebhook(dctx)
				dcancel()
			}
			log.Printf("telegrambot: getUpdates failed (retrying in %s): %v", backoff, err)
			select {
			case <-ctx.Done():
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second

		for _, u := range updates {
			// Per-update isolation: one bad update must never stall the loop.
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("telegrambot: update %d panicked (skipped): %v", u.UpdateID, r)
					}
				}()
				b.handleUpdate(ctx, u)
			}()
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
		}
		if len(updates) > 0 {
			b.saveOffset(ctx, offset)
		}
	}
}

// handleUpdate routes ONE update: DMs drive enrollment; group messages are
// commands, gated on the bound chat + the enrolled-admin check.
func (b *Bot) handleUpdate(ctx context.Context, u Update) {
	msg := u.Message
	if msg == nil || msg.From == nil || msg.From.IsBot {
		return
	}

	// ---- DM: the enrollment/auth state machine. ----
	if msg.Chat.Type == "private" {
		// Rate-limit the public DM surface (silently: a flooder learns nothing).
		if !b.dmLimiter.allow(msg.From.ID, time.Now()) {
			return
		}
		var phone string
		var contactUID int64
		if msg.Contact != nil {
			phone, contactUID = msg.Contact.PhoneNumber, msg.Contact.UserID
		}
		reply := b.auth.HandleDM(ctx, msg.From.ID, msg.Text, phone, contactUID)
		if reply.text != "" {
			sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if err := b.client.sendDM(sctx, msg.Chat.ID, reply.text, reply.requestContact); err != nil {
				log.Printf("telegrambot: DM reply failed: %v", err)
			}
			cancel()
		}
		return
	}

	// ---- Group: known commands only, and only forum-capable groups. ----
	if msg.Chat.Type != "supergroup" && msg.Chat.Type != "group" {
		return
	}
	cmd, args, ok := matchCommand(msg.Text, b.username)
	if !ok {
		return
	}
	// GATE 1 (fail closed): only an ENROLLED admin is ever answered — the
	// command surface stays invisible to everyone else.
	if !b.auth.IsEnrolledAdmin(ctx, msg.From.ID) {
		return
	}
	// GATE 2: only the bound admin group is answered. /setup is the one
	// exception — it IS the binding command (rebind needs TOTP inside).
	bound := b.notifier.boundChat()
	if cmd != "setup" && msg.Chat.ID != bound {
		return
	}

	reply := b.runCommand(ctx, cmd, args, msg.From.ID, msg.Chat.ID)
	if reply == "" {
		return
	}
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	// Reply into the SAME forum topic the command came from.
	if err := b.client.send(sctx, msg.Chat.ID, msg.MessageThreadID, reply); err != nil {
		log.Printf("telegrambot: command reply failed: %v", err)
	}
	cancel()
}

// healthTicker posts a periodic health summary into the Health topic.
func (b *Bot) healthTicker(ctx context.Context) {
	const every = 12 * time.Hour
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if b.notifier.boundChat() == 0 {
				continue
			}
			b.notifier.Notify(catHealth, b.cmdHealth(ctx))
		}
	}
}

const offsetStateKey = "updates_offset"

func (b *Bot) loadOffset(ctx context.Context) int64 {
	raw, err := b.store.GetBotState(ctx, offsetStateKey)
	if err != nil || raw == "" {
		return 0
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func (b *Bot) saveOffset(ctx context.Context, offset int64) {
	if err := b.store.SetBotState(ctx, offsetStateKey, strconv.FormatInt(offset, 10)); err != nil {
		log.Printf("telegrambot: persisting offset failed (updates may replay once): %v", err)
	}
}
