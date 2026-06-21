package tonaddr

import (
	"strings"
	"testing"
)

// TestCRC16CheckValue pins the checksum to CRC-16/XMODEM via its standard check
// value (the CRC of "123456789"). If this ever changes, the algorithm no longer
// matches what TON uses and every user-friendly address would be misvalidated.
func TestCRC16CheckValue(t *testing.T) {
	if got := crc16([]byte("123456789")); got != 0x31C3 {
		t.Fatalf("crc16(\"123456789\") = 0x%04X, want 0x31C3 (CRC-16/XMODEM)", got)
	}
}

// TestNormalizeOnNetwork pins the mainnet/testnet guard: a user-friendly address
// must match the target network; raw "<wc>:<hex>" is network-agnostic.
func TestNormalizeOnNetwork(t *testing.T) {
	var a Address // workchain 0, zero hash
	mainnet := a.Friendly(false)
	a.Testnet = true
	testnet := a.Friendly(false)
	raw := "0:" + strings.Repeat("00", 32)

	mustOK := func(s string, wantTestnet bool) {
		if _, err := NormalizeOnNetwork(s, wantTestnet); err != nil {
			t.Fatalf("NormalizeOnNetwork(%q, %v) = %v, want ok", s, wantTestnet, err)
		}
	}
	mustErr := func(s string, wantTestnet bool) {
		if _, err := NormalizeOnNetwork(s, wantTestnet); err == nil {
			t.Fatalf("NormalizeOnNetwork(%q, %v) = nil, want network mismatch error", s, wantTestnet)
		}
	}
	mustOK(mainnet, false)
	mustOK(testnet, true)
	mustErr(testnet, false)
	mustErr(mainnet, true)
	mustOK(raw, false) // raw accepted on either network
	mustOK(raw, true)
}

func TestFriendlyRoundTrip(t *testing.T) {
	var a Address
	for i := range a.Hash {
		a.Hash[i] = byte(i * 7)
	}
	a.Workchain = 0
	a.Bounceable = true

	s := a.String()
	if !strings.HasPrefix(s, "EQ") || len(s) != 48 {
		t.Fatalf("bounceable friendly form = %q, want 48-char EQ...", s)
	}
	back, err := Parse(s)
	if err != nil || back != a {
		t.Fatalf("round-trip: back=%+v err=%v, want %+v", back, err, a)
	}

	nb := a.Friendly(false)
	if !strings.HasPrefix(nb, "UQ") {
		t.Fatalf("non-bounceable friendly form = %q, want UQ...", nb)
	}
	if back, _ := Parse(nb); back.Bounceable {
		t.Fatal("UQ form should parse as non-bounceable")
	}
}

func TestParseRejectsBadChecksum(t *testing.T) {
	var a Address
	a.Hash[0] = 1
	good := a.String()
	// Flip a character in the body so the checksum no longer matches.
	bad := []byte(good)
	if bad[5] == 'A' {
		bad[5] = 'B'
	} else {
		bad[5] = 'A'
	}
	if _, err := Parse(string(bad)); err == nil {
		t.Fatal("a corrupted address must be rejected")
	}
}

func TestParseRaw(t *testing.T) {
	raw := "0:0000000000000000000000000000000000000000000000000000000000000001"
	a, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if a.Workchain != 0 || a.Hash[31] != 1 {
		t.Fatalf("parsed raw = %+v", a)
	}
	if a.Raw() != raw {
		t.Fatalf("raw round-trip = %q, want %q", a.Raw(), raw)
	}
	// Masterchain.
	mc := "-1:" + strings.Repeat("33", 32)
	if a, err := Parse(mc); err != nil || a.Workchain != -1 {
		t.Fatalf("masterchain parse: a=%+v err=%v", a, err)
	}
}

func TestNormalizeAcceptsStdBase64AndCanonicalizesToURLSafe(t *testing.T) {
	var a Address
	for i := range a.Hash {
		a.Hash[i] = byte(255 - i) // bytes that base64-encode with + and / in std alphabet
	}
	canonical := a.String() // url-safe
	if strings.ContainsAny(canonical, "+/") {
		t.Fatalf("canonical form must be url-safe, got %q", canonical)
	}
	// Feed the std-base64 spelling of the same bytes; Normalize must accept it and
	// return the url-safe canonical form.
	std := strings.NewReplacer("-", "+", "_", "/").Replace(canonical)
	got, err := Normalize(std)
	if err != nil {
		t.Fatalf("Normalize(std base64) error: %v", err)
	}
	if got != canonical {
		t.Fatalf("Normalize(%q) = %q, want %q", std, got, canonical)
	}
}

func TestRejectsGarbage(t *testing.T) {
	for _, s := range []string{"", "   ", "nope", "UQpay", "0:xyz", "0:" + strings.Repeat("0", 63), "2:" + strings.Repeat("0", 64)} {
		if Valid(s) {
			t.Errorf("Valid(%q) = true, want false", s)
		}
	}
}
