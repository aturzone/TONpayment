package telegrambot

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/aturzone/TONpayment/internal/money"
	"github.com/aturzone/TONpayment/internal/store"
)

// Alert categories, each routed to its own forum topic (created by /setup).
// A category with no topic falls back to the group's General topic.
const (
	catPayments = "payments"
	catLinks    = "links"
	catHealth   = "health"
	catCritical = "critical"
)

// topicNames maps category → the forum-topic display name /setup creates.
var topicNames = map[string]string{
	catPayments: "💸 Payments",
	catLinks:    "🔗 Links",
	catHealth:   "📈 Health",
	catCritical: "🚨 Critical",
}

// notifier fans platform events into the admin group's forum topics. Events are
// queued on a buffered channel consumed by one worker goroutine, so a slow or
// down Telegram can NEVER block the caller (the settle path, the control
// plane). When the queue is full the event is dropped with a log line — alerts
// are best-effort telemetry, never a correctness dependency.
type notifier struct {
	client *client
	store  botStateStore

	mu     sync.RWMutex
	chatID int64            // 0 until the group is bound
	topics map[string]int64 // category → message_thread_id

	queue chan event
	done  chan struct{}
}

type event struct {
	category string
	html     string
}

// botStateStore is the state persistence the notifier + bot loop share.
type botStateStore interface {
	GetBotState(ctx context.Context, key string) (string, error)
	SetBotState(ctx context.Context, key, value string) error
}

func newNotifier(c *client, st botStateStore) *notifier {
	return &notifier{
		client: c,
		store:  st,
		topics: map[string]int64{},
		queue:  make(chan event, 256),
		done:   make(chan struct{}),
	}
}

// loadState restores the bound chat + topic map persisted by a previous run.
func (n *notifier) loadState(ctx context.Context) {
	if raw, err := n.store.GetBotState(ctx, "chat_id"); err == nil && raw != "" {
		var id int64
		if err := json.Unmarshal([]byte(raw), &id); err == nil {
			n.mu.Lock()
			n.chatID = id
			n.mu.Unlock()
		}
	}
	if raw, err := n.store.GetBotState(ctx, "topics"); err == nil && raw != "" {
		topics := map[string]int64{}
		if err := json.Unmarshal([]byte(raw), &topics); err == nil {
			n.mu.Lock()
			n.topics = topics
			n.mu.Unlock()
		}
	}
}

// bindChat pins the admin group and persists it.
func (n *notifier) bindChat(ctx context.Context, chatID int64) error {
	n.mu.Lock()
	n.chatID = chatID
	n.mu.Unlock()
	b, _ := json.Marshal(chatID)
	return n.store.SetBotState(ctx, "chat_id", string(b))
}

func (n *notifier) boundChat() int64 {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.chatID
}

// setTopics replaces the topic map and persists it.
func (n *notifier) setTopics(ctx context.Context, topics map[string]int64) error {
	n.mu.Lock()
	n.topics = topics
	n.mu.Unlock()
	b, err := json.Marshal(topics)
	if err != nil {
		return err
	}
	return n.store.SetBotState(ctx, "topics", string(b))
}

func (n *notifier) topicFor(category string) int64 {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.topics[category]
}

// Notify queues one event; drop-on-full so callers never block.
func (n *notifier) Notify(category, html string) {
	select {
	case n.queue <- event{category: category, html: html}:
	default:
		log.Printf("telegrambot: notify queue full — dropped a %s event", category)
	}
}

// run consumes the queue until ctx is cancelled, then drains what's left with a
// short grace period so shutdown never hangs on Telegram.
func (n *notifier) run(ctx context.Context) {
	defer close(n.done)
	for {
		select {
		case <-ctx.Done():
			n.drain()
			return
		case ev := <-n.queue:
			n.deliver(context.Background(), ev)
		}
	}
}

func (n *notifier) drain() {
	deadline := time.Now().Add(3 * time.Second)
	for {
		select {
		case ev := <-n.queue:
			if time.Now().After(deadline) {
				return
			}
			ctx, cancel := context.WithDeadline(context.Background(), deadline)
			n.deliver(ctx, ev)
			cancel()
		default:
			return
		}
	}
}

func (n *notifier) deliver(ctx context.Context, ev event) {
	chat := n.boundChat()
	if chat == 0 {
		return // no group bound yet — nothing to deliver to
	}
	sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := n.client.send(sendCtx, chat, n.topicFor(ev.category), ev.html); err != nil {
		log.Printf("telegrambot: %s alert delivery failed: %v", ev.category, err)
	}
}

// Wait blocks until the worker goroutine has exited (for graceful shutdown).
func (n *notifier) Wait() { <-n.done }

// ----- event sources -----

// PaymentSink adapts the notifier to the service.Webhook interface so settled
// invoices flow into the Payments topic. It wraps (and always forwards to) the
// real webhook sink; notification is fire-and-forget on top.
type PaymentSink struct {
	Next     interface{ Fire(inv store.Invoice) }
	Notifier *notifier
	Lookup   func(ctx context.Context, gatewayID string) (store.Gateway, bool, error)
}

// Fire forwards settlement to the real sink first, then builds + queues the
// alert OFF the settle path (its slug lookup is a DB read; the settle caller
// must never wait on it — fire-and-forget as promised).
func (p *PaymentSink) Fire(inv store.Invoice) {
	if p.Next != nil {
		p.Next.Fire(inv)
	}
	go func() {
		slug := ""
		kind := "payment"
		if p.Lookup != nil && inv.GatewayID != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if g, ok, err := p.Lookup(ctx, inv.GatewayID); err == nil && ok {
				slug, kind = g.Slug, g.Kind
			}
			cancel()
		}
		icon := "💳"
		if kind == "donation" {
			icon = "☕"
		}
		msg := icon + " <b>" + escapeHTML(money.NanoToTONString(inv.AmountNano)) + " GRAM</b> received"
		if slug != "" {
			msg += " on /" + escapeHTML(slug)
		}
		msg += "\ninvoice <code>" + escapeHTML(inv.ID) + "</code>"
		if inv.TxHash != "" {
			msg += " · tx <code>" + escapeHTML(shorten(inv.TxHash, 12)) + "</code>"
		}
		p.Notifier.Notify(catPayments, msg)
	}()
}

// LinkEvent posts a link-lifecycle alert (created/updated/deleted).
func (n *notifier) LinkEvent(action string, g store.Gateway) {
	icon := "🔗"
	if action == "deleted" {
		icon = "🗑"
	}
	kind := g.Kind
	if kind == "" {
		kind = "payment"
	}
	msg := icon + " Link <b>" + escapeHTML(action) + "</b>: /" + escapeHTML(g.Slug) +
		" (" + escapeHTML(kind) + ")" +
		"\nwallet <code>" + escapeHTML(shorten(g.ReceivingAddress, 12)) + "</code>"
	if g.DisplayName != "" {
		msg += " · " + escapeHTML(g.DisplayName)
	}
	n.Notify(catLinks, msg)
}

func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	half := n / 2
	return s[:half] + "…" + s[len(s)-half:]
}
