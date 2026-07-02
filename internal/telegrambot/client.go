package telegrambot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// client is a minimal Telegram Bot API transport: long-poll getUpdates plus the
// handful of send/admin calls the bot needs. Every call is bounded by a
// context deadline and fails SAFE (an error is returned, never panics), so a
// hung or failing Telegram endpoint can never stall the engine.
//
// ⚠️ getUpdates and a webhook are MUTUALLY EXCLUSIVE on the Telegram side: a
// set webhook makes getUpdates return 409. Run() calls deleteWebhook first and
// this bot NEVER sets one.
type client struct {
	token string
	http  *http.Client
}

func newClient(token string) *client {
	// The long poll holds the request open for up to longPollSeconds; the client
	// timeout must comfortably exceed it.
	return &client{token: token, http: &http.Client{Timeout: (longPollSeconds + 15) * time.Second}}
}

const longPollSeconds = 25

// Update is the subset of a Telegram update the bot reads: group commands
// (text + chat + thread + sender) and the DM auth flow (chat type, contact).
type Update struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		MessageID       int64  `json:"message_id"`
		MessageThreadID int64  `json:"message_thread_id"`
		Text            string `json:"text"`
		From            *struct {
			ID    int64 `json:"id"`
			IsBot bool  `json:"is_bot"`
		} `json:"from"`
		Chat struct {
			ID   int64  `json:"id"`
			Type string `json:"type"` // "private" | "group" | "supergroup" | "channel"
		} `json:"chat"`
		// Present only on a shared-contact message. UserID is set when the
		// sender shared their OWN number — the property the auth gate keys on.
		Contact *struct {
			PhoneNumber string `json:"phone_number"`
			UserID      int64  `json:"user_id"`
		} `json:"contact"`
	} `json:"message"`
}

// call POSTs one Bot API method. The token appears only in the URL — it is
// never logged and never included in an error message.
func (c *client) call(ctx context.Context, method string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.telegram.org/bot"+c.token+"/"+method, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("telegram %s: transport: %w", method, err)
	}
	defer res.Body.Close()
	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
		ErrorCode   int             `json:"error_code"`
	}
	if err := json.NewDecoder(res.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("telegram %s: decode: %w", method, err)
	}
	if !envelope.OK {
		return fmt.Errorf("telegram %s: api error %d: %s", method, envelope.ErrorCode, envelope.Description)
	}
	if out != nil {
		return json.Unmarshal(envelope.Result, out)
	}
	return nil
}

// getUpdates long-polls for updates after offset. On error it returns an empty
// slice + the error; the caller backs off and retries — no update is lost
// (Telegram redelivers anything past the last confirmed offset).
func (c *client) getUpdates(ctx context.Context, offset int64) ([]Update, error) {
	var updates []Update
	err := c.call(ctx, "getUpdates", map[string]any{
		"offset":          offset,
		"limit":           100,
		"timeout":         longPollSeconds,
		"allowed_updates": []string{"message"},
	}, &updates)
	if err != nil {
		return nil, err
	}
	return updates, nil
}

// deleteWebhook clears any webhook so getUpdates works (idempotent).
func (c *client) deleteWebhook(ctx context.Context) error {
	return c.call(ctx, "deleteWebhook", map[string]any{"drop_pending_updates": false}, nil)
}

// getMe returns the bot's username (for /cmd@BotName matching in groups).
func (c *client) getMe(ctx context.Context) (string, error) {
	var me struct {
		Username string `json:"username"`
	}
	if err := c.call(ctx, "getMe", map[string]any{}, &me); err != nil {
		return "", err
	}
	return me.Username, nil
}

// send posts an HTML message to chatID, into threadID's forum topic when
// non-zero. Errors are returned for the caller to log (never fatal).
func (c *client) send(ctx context.Context, chatID int64, threadID int64, html string) error {
	body := map[string]any{
		"chat_id":                  chatID,
		"text":                     html,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}
	if threadID != 0 {
		body["message_thread_id"] = threadID
	}
	return c.call(ctx, "sendMessage", body, nil)
}

// sendDM posts an HTML message to a private chat, attaching the share-phone
// reply keyboard when requestContact is true (removing it otherwise so a stale
// button never lingers).
func (c *client) sendDM(ctx context.Context, chatID int64, html string, requestContact bool) error {
	var markup any
	if requestContact {
		markup = map[string]any{
			"keyboard":          [][]map[string]any{{{"text": "Share my phone number", "request_contact": true}}},
			"resize_keyboard":   true,
			"one_time_keyboard": true,
		}
	} else {
		markup = map[string]any{"remove_keyboard": true}
	}
	return c.call(ctx, "sendMessage", map[string]any{
		"chat_id":                  chatID,
		"text":                     html,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
		"reply_markup":             markup,
	}, nil)
}

// createForumTopic creates one forum topic and returns its thread id. Requires
// the bot to be a group admin with the "Manage topics" right.
func (c *client) createForumTopic(ctx context.Context, chatID int64, name string) (int64, error) {
	var topic struct {
		MessageThreadID int64 `json:"message_thread_id"`
	}
	err := c.call(ctx, "createForumTopic", map[string]any{"chat_id": chatID, "name": name}, &topic)
	return topic.MessageThreadID, err
}

// escapeHTML escapes the three characters Telegram's HTML parse mode treats
// specially, so dynamic values render (and tap-copy) verbatim inside <code>.
func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	return strings.ReplaceAll(s, ">", "&gt;")
}

// matchCommand identifies which known command text invokes, tolerating the
// group-suffixed form (/stats@BotName, case-insensitive on the suffix). It
// returns the bare command name without the slash, plus the remaining
// whitespace-separated args; ok=false for a non-command.
func matchCommand(text, botUsername string) (cmd string, args []string, ok bool) {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return "", nil, false
	}
	name := strings.TrimPrefix(fields[0], "/")
	if at := strings.IndexByte(name, '@'); at >= 0 {
		// A suffixed command is EXPLICITLY addressed. If we don't yet know our
		// own username (getMe failed at boot) we cannot verify it is ours, so
		// fail CLOSED — never execute a command that may target another bot.
		if botUsername == "" || !strings.EqualFold(name[at+1:], botUsername) {
			return "", nil, false
		}
		name = name[:at]
	}
	return strings.ToLower(name), fields[1:], true
}
