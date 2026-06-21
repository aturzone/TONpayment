package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// Claims is the session payload bound into an opaque token after a successful
// ton_proof sign-in. It is the merchant's identity for control-plane requests.
type Claims struct {
	MerchantID string `json:"sub"`
	Wallet     string `json:"wallet"`
	Admin      bool   `json:"admin,omitempty"`
	IssuedAt   int64  `json:"iat"`
	ExpiresAt  int64  `json:"exp"`
}

// SignSession returns base64url(claims) + "." + base64url(HMAC-SHA256(secret,
// payload)). This is a self-contained, stateless token verified by recomputing the
// MAC — no new dependency (the same HMAC primitive the webhook signer uses) and no
// server-side session store.
func SignSession(secret []byte, c Claims) string {
	payload, _ := json.Marshal(c)
	p := b64(payload)
	return p + "." + b64(macSum(secret, []byte(p)))
}

// ParseSession verifies the signature (constant-time) and the expiry, returning the
// claims. now is injectable so callers/tests control the clock.
func ParseSession(secret []byte, token string, now time.Time) (Claims, bool) {
	dot := strings.IndexByte(token, '.')
	if dot <= 0 || dot == len(token)-1 {
		return Claims{}, false
	}
	payloadB64, sigB64 := token[:dot], token[dot+1:]
	want := macSum(secret, []byte(payloadB64))
	got, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil || subtle.ConstantTimeCompare(want, got) != 1 {
		return Claims{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return Claims{}, false
	}
	var c Claims
	if err := json.Unmarshal(raw, &c); err != nil {
		return Claims{}, false
	}
	if c.ExpiresAt != 0 && now.Unix() >= c.ExpiresAt {
		return Claims{}, false
	}
	return c, true
}

func macSum(secret, msg []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write(msg)
	return m.Sum(nil)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
