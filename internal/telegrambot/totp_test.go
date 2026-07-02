package telegrambot

import (
	"testing"
	"time"
)

// RFC 6238 Appendix B test vectors use an ASCII "12345678901234567890" key
// (base32 GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ) with 8 digits; we verify our HOTP
// core against the low-order 6 digits of those vectors.
func TestCheckTOTP_RFCVectors(t *testing.T) {
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	cases := []struct {
		unix int64
		code string // last 6 digits of the RFC 8-digit value
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1234567890, "005924"},
		{2000000000, "279037"},
	}
	for _, c := range cases {
		if !CheckTOTP(c.code, secret, time.Unix(c.unix, 0)) {
			t.Errorf("code %s at t=%d should verify", c.code, c.unix)
		}
	}
	if CheckTOTP("000000", secret, time.Unix(59, 0)) {
		t.Error("wrong code must not verify")
	}
}

func TestCheckTOTP_WindowAndFailModes(t *testing.T) {
	secret, err := NewTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	// A code from the PREVIOUS step still verifies (clock skew tolerance)...
	key, _ := b32.DecodeString(secret)
	prev := hotp(key, uint64(now.Add(-totpStep).Unix())/30)
	if !CheckTOTP(prev, secret, now) {
		t.Error("previous-step code should verify (±1 window)")
	}
	// ...but one from two steps back must not.
	old := hotp(key, uint64(now.Add(-2*totpStep).Unix())/30)
	cur := hotp(key, uint64(now.Unix())/30)
	next := hotp(key, uint64(now.Add(totpStep).Unix())/30)
	if old != cur && old != next && old != prev && CheckTOTP(old, secret, now) {
		t.Error("two-steps-old code should NOT verify")
	}
	// Garbage secrets fail closed.
	if CheckTOTP("123456", "not-base32!!!", now) {
		t.Error("invalid secret must fail closed")
	}
	if CheckTOTP("123456", "", now) {
		t.Error("empty secret must fail closed")
	}
}

func TestExtractTOTPCode(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"123456", "123456", true},
		{" 123 456 ", "123456", true},
		{"12345", "", false},
		{"1234567", "", false},
		{"/start", "", false},
		{"abc123", "", false},
		{"", "", false},
		{"123456 extra", "", false},
	}
	for _, c := range cases {
		got, ok := ExtractTOTPCode(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("ExtractTOTPCode(%q) = %q,%v; want %q,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestSecretCipherRoundTrip(t *testing.T) {
	c, err := newSecretCipher("test-master-secret")
	if err != nil {
		t.Fatal(err)
	}
	enc, err := c.encrypt("JBSWY3DPEHPK3PXP")
	if err != nil {
		t.Fatal(err)
	}
	if enc == "JBSWY3DPEHPK3PXP" {
		t.Fatal("ciphertext must differ from plaintext")
	}
	dec, err := c.decrypt(enc)
	if err != nil || dec != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("round trip failed: %q %v", dec, err)
	}
	// Tamper detection.
	if _, err := c.decrypt(enc[:len(enc)-4] + "AAAA"); err == nil {
		t.Error("tampered ciphertext must fail")
	}
	// A different master secret cannot decrypt.
	c2, _ := newSecretCipher("other-secret")
	if _, err := c2.decrypt(enc); err == nil {
		t.Error("wrong key must fail")
	}
	// Empty master secret is refused.
	if _, err := newSecretCipher(""); err == nil {
		t.Error("empty master secret must be refused")
	}
}

func TestMatchCommand(t *testing.T) {
	cases := []struct {
		text, bot string
		cmd       string
		args      []string
		ok        bool
	}{
		{"/stats", "MyBot", "stats", nil, true},
		{"/stats@MyBot", "MyBot", "stats", nil, true},
		{"/stats@mybot", "MyBot", "stats", nil, true},
		{"/stats@OtherBot", "MyBot", "", nil, false},
		{"/link github", "MyBot", "link", []string{"github"}, true},
		{"/disable github 123456", "MyBot", "disable", []string{"github", "123456"}, true},
		{"hello", "MyBot", "", nil, false},
		{"", "MyBot", "", nil, false},
		{"/STATS", "MyBot", "stats", nil, true},
	}
	for _, c := range cases {
		cmd, args, ok := matchCommand(c.text, c.bot)
		if ok != c.ok || cmd != c.cmd || len(args) != len(c.args) {
			t.Errorf("matchCommand(%q) = %q,%v,%v; want %q,%v,%v", c.text, cmd, args, ok, c.cmd, c.args, c.ok)
		}
	}
}

func TestNormalizePhone(t *testing.T) {
	for _, in := range []string{"+989154869715", "989154869715", "09154869715", "0098 915 486 9715", "+98 915-486-9715"} {
		if got := normalizePhone(in); got != "989154869715" {
			t.Errorf("normalizePhone(%q) = %q; want 989154869715", in, got)
		}
	}
	if got := normalizePhone("+15551234567"); got != "15551234567" {
		t.Errorf("US number mangled: %q", got)
	}
	if normalizePhone("") != "" {
		t.Error("empty stays empty")
	}
}
