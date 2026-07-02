package telegrambot

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP (RFC 6238, the flavor every authenticator app implements): 30-second
// steps, 6 digits, HMAC-SHA1, with a ±1-step verify window for clock skew.
// Implemented on the stdlib so the engine gains no new dependency for it.
const (
	totpStep   = 30 * time.Second
	totpDigits = 6
	// totpWindow accepts the previous/current/next step, so a code stays valid
	// for up to ~90s. The replay-guard TTL must outlive this span.
	totpWindow = 1
	// TOTPReplayTTL is how long a consumed code is remembered. It must cover
	// the full verify window (3 steps = 90s) so a code can never be replayed
	// while it would still verify.
	TOTPReplayTTL = 2 * time.Minute
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a fresh 160-bit base32 secret (the RFC 4226
// recommendation, and what authenticator apps expect).
func NewTOTPSecret() (string, error) {
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return b32.EncodeToString(buf), nil
}

// TOTPKeyURI renders the otpauth:// enrollment URI for authenticator apps.
func TOTPKeyURI(label, issuer, secret string) string {
	return fmt.Sprintf("otpauth://totp/%s?secret=%s&issuer=%s",
		url.PathEscape(issuer+":"+label), secret, url.QueryEscape(issuer))
}

// CheckTOTP reports whether code matches secret at time now, accepting ±1 step
// of clock skew. Constant-time code comparison; any decode error is "no match"
// (fail closed).
func CheckTOTP(code, secret string, now time.Time) bool {
	key, err := b32.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil || len(key) == 0 {
		return false
	}
	counter := uint64(now.Unix()) / uint64(totpStep.Seconds())
	for skew := -totpWindow; skew <= totpWindow; skew++ {
		if subtle.ConstantTimeCompare([]byte(hotp(key, counter+uint64(skew))), []byte(code)) == 1 {
			return true
		}
	}
	return false
}

// hotp computes one RFC 4226 HOTP value (6 digits, zero-padded).
func hotp(key []byte, counter uint64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, v%1_000_000)
}

// ExtractTOTPCode pulls a 6-digit code out of a text message, tolerating inner
// spaces ("123 456"); anything that is not exactly 6 digits is rejected so
// ordinary text is never mistaken for a code.
func ExtractTOTPCode(text string) (string, bool) {
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		if r == ' ' {
			return -1
		}
		return 'x' // any other rune poisons the string below
	}, strings.TrimSpace(text))
	if len(digits) != totpDigits || strings.ContainsRune(digits, 'x') {
		return "", false
	}
	return digits, true
}
