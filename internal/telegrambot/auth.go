package telegrambot

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/aturzone/TONpayment/internal/store"
)

// authStore is the persistence the auth flow needs (satisfied by
// *store.Postgres, and by a tiny fake in tests).
type authStore interface {
	GetTelegramAdmin(ctx context.Context, id int64) (store.TelegramAdmin, bool, error)
	UpsertStagedTelegramAdmin(ctx context.Context, id int64, phone, secretEnc string) error
	MarkTelegramAdminEnrolled(ctx context.Context, id int64) error
	RecordTelegramTOTPFailure(ctx context.Context, id int64, attempts int, lock time.Time) error
	ResetTelegramTOTPFailures(ctx context.Context, id int64) error
	ClaimTelegramTOTPCode(ctx context.Context, id int64, code string, ttl time.Duration) (bool, error)
}

// authService is the admin allow-list + 2FA gate. SECURITY-CRITICAL: a defect
// here would hand platform admin commands to a stranger. Every path FAILS
// CLOSED — any error reads as "not authorized" and never as a thrown panic.
//
// The enrollment state machine (all in the bot DM):
//  1. Any text from an unknown user → ask them to SHARE THEIR PHONE.
//  2. A shared contact binds ONLY if BOTH hold: contact.user_id == sender (their
//     OWN number — a forwarded contact card never binds) AND the normalized
//     number is on the allow-list. On bind: stage an encrypted TOTP secret and
//     reply with the secret + otpauth URI exactly once.
//  3. A 6-digit code from a bound-but-unenrolled user completes enrollment.
//  4. Everything else gets a NEUTRAL reply that leaks nothing.
type authService struct {
	store     authStore
	cipher    *secretCipher
	allowlist []string // normalized digit strings
	notify    func(category, html string)

	// Brute-force lockout: after lockoutThreshold consecutive bad codes the
	// admin row is locked for lockoutWindow (persisted; restart-proof).
	lockoutThreshold int
	lockoutWindow    time.Duration
	// reshareCooldown bounds how often a pre-enrollment contact re-share may
	// rotate the staged secret.
	reshareCooldown time.Duration
}

func newAuthService(st authStore, cipher *secretCipher, phones []string, notify func(category, html string)) *authService {
	a := &authService{
		store:            st,
		cipher:           cipher,
		notify:           notify,
		lockoutThreshold: 5,
		lockoutWindow:    15 * time.Minute,
		reshareCooldown:  30 * time.Second,
	}
	for _, p := range phones {
		if n := normalizePhone(p); n != "" {
			a.allowlist = append(a.allowlist, n)
		}
	}
	return a
}

// normalizePhone reduces a phone number to bare digits in international form:
// strip everything but digits, drop a leading 00, and map an Iranian national
// 0-prefixed number to its 98 form so +989…, 989… and 09… all agree.
func normalizePhone(p string) string {
	var b strings.Builder
	for _, r := range p {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	d := b.String()
	d = strings.TrimPrefix(d, "00")
	if len(d) == 11 && strings.HasPrefix(d, "0") {
		d = "98" + d[1:]
	}
	return d
}

func (a *authService) isAllowlisted(phone string) bool {
	n := normalizePhone(phone)
	if n == "" {
		return false
	}
	for _, p := range a.allowlist {
		if p == n {
			return true
		}
	}
	return false
}

// IsEnrolledAdmin is THE gate every group command passes first. FAILS CLOSED:
// any store error reads as not-authorized.
func (a *authService) IsEnrolledAdmin(ctx context.Context, telegramUserID int64) bool {
	if telegramUserID == 0 {
		return false
	}
	row, ok, err := a.store.GetTelegramAdmin(ctx, telegramUserID)
	if err != nil {
		log.Printf("telegrambot: admin lookup failed for %d — failing CLOSED: %v", telegramUserID, err)
		return false
	}
	return ok && row.Enrolled
}

// VerifyFreshTOTP is the sensitive-command 2FA gate: the code must verify for
// an ENROLLED admin AND be unused (one-time claim), with the lockout enforced
// before any cryptographic check. FAILS CLOSED on every error path.
func (a *authService) VerifyFreshTOTP(ctx context.Context, telegramUserID int64, code string) bool {
	normalized, ok := ExtractTOTPCode(code)
	if !ok {
		return false
	}
	row, found, err := a.store.GetTelegramAdmin(ctx, telegramUserID)
	if err != nil || !found || !row.Enrolled || row.TOTPSecretEnc == "" {
		return false
	}
	if time.Now().Before(row.TOTPLockedUntil) {
		return false // locked: reject WITHOUT giving the brute-forcer an oracle
	}
	secret, err := a.cipher.decrypt(row.TOTPSecretEnc)
	if err != nil {
		log.Printf("telegrambot: TOTP secret decrypt failed for %d — failing CLOSED: %v", telegramUserID, err)
		return false
	}
	if !CheckTOTP(normalized, secret, time.Now()) {
		a.registerFailure(ctx, telegramUserID, row, "command")
		return false
	}
	// Cryptographically valid — now CONSUME it so the same 6 digits can never
	// authorize twice within their validity window.
	claimed, err := a.store.ClaimTelegramTOTPCode(ctx, telegramUserID, normalized, TOTPReplayTTL)
	if err != nil || !claimed {
		if err != nil {
			log.Printf("telegrambot: TOTP replay claim failed for %d — failing CLOSED: %v", telegramUserID, err)
		} else {
			log.Printf("telegrambot: TOTP REPLAY rejected for %d", telegramUserID)
			a.alert("critical", "🚨 A replayed 2FA code was rejected for admin <code>"+escapeHTML(itoa(telegramUserID))+"</code>.")
		}
		return false
	}
	if err := a.store.ResetTelegramTOTPFailures(ctx, telegramUserID); err != nil {
		log.Printf("telegrambot: TOTP failure reset failed (self-heals): %v", err)
	}
	return true
}

// dmReply is what the DM handler wants sent back (empty text = stay silent).
type dmReply struct {
	text           string
	requestContact bool
}

// HandleDM advances the enrollment state machine for one private-chat message.
// Never returns an error — every failure resolves to a neutral reply.
func (a *authService) HandleDM(ctx context.Context, fromID int64, text string, contactPhone string, contactUserID int64) dmReply {
	if fromID == 0 {
		return dmReply{}
	}

	// A shared contact is the phone-verification step.
	if contactPhone != "" || contactUserID != 0 {
		return a.handleContact(ctx, fromID, contactPhone, contactUserID)
	}

	row, found, err := a.store.GetTelegramAdmin(ctx, fromID)
	if err != nil {
		log.Printf("telegrambot: DM admin lookup failed — neutral reply: %v", err)
		return dmReply{text: replyNotAuthorized}
	}

	// Enrolled admin: confirm status + command help. No secret, ever.
	if found && row.Enrolled {
		return dmReply{text: replyAdminHelp}
	}

	// Bound but not yet enrolled: a 6-digit code completes enrollment.
	if found {
		if code, ok := ExtractTOTPCode(text); ok {
			return a.confirmEnrollment(ctx, fromID, row, code)
		}
		return dmReply{text: replyAwaitingCode}
	}

	// A stranger (or the allow-listed owner before sharing): the ONLY path
	// forward is sharing their phone. No capability hints beyond that.
	return dmReply{text: replySharePhone, requestContact: true}
}

// handleContact verifies ownership + allow-list, then stages enrollment.
func (a *authService) handleContact(ctx context.Context, fromID int64, phone string, contactUserID int64) dmReply {
	ownsContact := contactUserID != 0 && contactUserID == fromID
	allowed := a.isAllowlisted(phone)
	if !ownsContact || !allowed {
		// Neutral rejection: never reveal WHICH check failed.
		log.Printf("telegrambot: bind REJECTED for %d (own=%v allowed=%v phone=%s)", fromID, ownsContact, allowed, maskPhone(phone))
		a.alert("critical", "🚨 A Telegram enrollment attempt was rejected (id <code>"+escapeHTML(itoa(fromID))+"</code>, phone "+escapeHTML(maskPhone(phone))+").")
		return dmReply{text: replyNotAuthorized}
	}

	row, found, err := a.store.GetTelegramAdmin(ctx, fromID)
	if err != nil {
		log.Printf("telegrambot: contact admin lookup failed — neutral reply: %v", err)
		return dmReply{text: replyNotAuthorized}
	}
	if found && row.Enrolled {
		return dmReply{text: replyAlreadyEnrolled}
	}

	// Throttled re-share: don't rotate the staged secret again within the
	// cooldown — RE-SHOW the existing one so the owner can still finish setup.
	if found && row.TOTPSecretEnc != "" && time.Since(row.UpdatedAt) < a.reshareCooldown {
		if secret, err := a.cipher.decrypt(row.TOTPSecretEnc); err == nil {
			return dmReply{text: enrollPrompt(fromID, secret)}
		}
		return dmReply{text: replyAwaitingCode}
	}

	secret, err := NewTOTPSecret()
	if err != nil {
		log.Printf("telegrambot: secret generation failed: %v", err)
		return dmReply{text: replyNotAuthorized}
	}
	enc, err := a.cipher.encrypt(secret)
	if err != nil {
		log.Printf("telegrambot: secret encrypt failed: %v", err)
		return dmReply{text: replyNotAuthorized}
	}
	if err := a.store.UpsertStagedTelegramAdmin(ctx, fromID, normalizePhone(phone), enc); err != nil {
		log.Printf("telegrambot: staging enrollment failed: %v", err)
		return dmReply{text: replyNotAuthorized}
	}
	log.Printf("telegrambot: phone VERIFIED + TOTP staged for %d (phone %s); awaiting 2FA confirm", fromID, maskPhone(phone))
	return dmReply{text: enrollPrompt(fromID, secret)}
}

// confirmEnrollment verifies the 6-digit code against the staged secret.
func (a *authService) confirmEnrollment(ctx context.Context, fromID int64, row store.TelegramAdmin, code string) dmReply {
	if row.TOTPSecretEnc == "" {
		return dmReply{text: replySharePhone, requestContact: true}
	}
	if time.Now().Before(row.TOTPLockedUntil) {
		return dmReply{text: replyLockedOut}
	}
	secret, err := a.cipher.decrypt(row.TOTPSecretEnc)
	if err != nil {
		log.Printf("telegrambot: enrollment decrypt failed for %d — failing CLOSED: %v", fromID, err)
		return dmReply{text: replyWrongCode}
	}
	if !CheckTOTP(code, secret, time.Now()) {
		locked := a.registerFailure(ctx, fromID, row, "enroll")
		if locked {
			return dmReply{text: replyLockedOut}
		}
		return dmReply{text: replyWrongCode}
	}
	// CONSUME the enrollment code too, so the exact code that finished setup
	// cannot immediately authorize a sensitive command within its window.
	if _, err := a.store.ClaimTelegramTOTPCode(ctx, fromID, code, TOTPReplayTTL); err != nil {
		log.Printf("telegrambot: enrollment code claim failed (continuing): %v", err)
	}
	if err := a.store.ResetTelegramTOTPFailures(ctx, fromID); err != nil {
		log.Printf("telegrambot: TOTP failure reset failed (self-heals): %v", err)
	}
	if err := a.store.MarkTelegramAdminEnrolled(ctx, fromID); err != nil {
		log.Printf("telegrambot: enroll write failed: %v", err)
		return dmReply{text: replyWrongCode}
	}
	log.Printf("telegrambot: admin %d 2FA ENROLLED (phone %s)", fromID, maskPhone(row.Phone))
	return dmReply{text: replyEnrolled}
}

// registerFailure counts one bad code and arms the lockout at the threshold.
// Returns true when the account is (now) locked. Best-effort: a write failure
// can't turn a rejected code into an accepted one.
func (a *authService) registerFailure(ctx context.Context, fromID int64, row store.TelegramAdmin, stage string) bool {
	next := row.FailedTOTPAttempts + 1
	var lock time.Time
	if next >= a.lockoutThreshold {
		lock = time.Now().Add(a.lockoutWindow)
	}
	if err := a.store.RecordTelegramTOTPFailure(ctx, fromID, next, lock); err != nil {
		log.Printf("telegrambot: TOTP failure write failed (%s) — code already rejected: %v", stage, err)
		return false
	}
	if !lock.IsZero() {
		log.Printf("telegrambot: admin %d TOTP LOCKED after %d bad attempts (%s) until %s", fromID, next, stage, lock.UTC().Format(time.RFC3339))
		a.alert("critical", "🚨 2FA lockout: too many bad codes for admin <code>"+escapeHTML(itoa(fromID))+"</code> ("+escapeHTML(stage)+").")
		return true
	}
	return false
}

func (a *authService) alert(category, html string) {
	if a.notify != nil {
		a.notify(category, html)
	}
}

func maskPhone(p string) string {
	d := normalizePhone(p)
	if len(d) <= 4 {
		return "****"
	}
	return "…" + d[len(d)-4:]
}

func itoa(n int64) string {
	// small helper to avoid strconv imports sprinkled through message builders
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// ----- DM reply copy (English, neutral on rejection) -----

const (
	// replyNotAuthorized is IDENTICAL for every rejection so nothing about why
	// is leaked and the allow-list cannot be probed.
	replyNotAuthorized = "Not authorized."

	replySharePhone = "To continue, please share your phone number using the button below."

	replyAwaitingCode = "First-time 2FA setup (one time). Add the secret I sent to your authenticator app, " +
		"then send me the 6-digit code it shows to finish setup."

	replyAlreadyEnrolled = "You're already set up as the authorized admin. Sensitive commands ask for a fresh 6-digit code."

	replyEnrolled = "2FA setup complete — you're the authorized admin. Run /help in the admin group to see commands. " +
		"Sensitive commands (disable/enable links, fees) need a fresh 6-digit code each time."

	replyWrongCode = "Wrong code. Send the current 6-digit code from your authenticator app."

	replyLockedOut = "Too many incorrect codes. Try again later."

	replyAdminHelp = "You're the authorized admin (2FA done). Run commands in the admin group:\n" +
		"/stats — platform snapshot\n" +
		"/links — recent links\n" +
		"/link <slug> — one link in detail\n" +
		"/invoices — recent invoices\n" +
		"/pending — pending backlog\n" +
		"/health — engine health\n" +
		"/fee — platform fee\n" +
		"/disable <slug> <code> — turn a link off (2FA)\n" +
		"/enable <slug> <code> — turn a link back on (2FA)\n" +
		"/setup — bind this group + create alert topics"
)

// enrollPrompt is sent exactly once per (re)stage, to the verified owner only.
// The secret + otpauth URI sit inside <code> so they tap-copy on mobile.
func enrollPrompt(fromID int64, secret string) string {
	uri := TOTPKeyURI("tg:"+itoa(fromID), "TONpayment Admin", secret)
	return "Phone verified. First-time 2FA setup (one time).\n\n" +
		"🔑 2FA secret (tap to copy):\n<code>" + escapeHTML(secret) + "</code>\n\n" +
		"or add via link:\n<code>" + escapeHTML(uri) + "</code>\n\n" +
		"Add it to your authenticator app, then send me the 6-digit code it shows to finish setup."
}
