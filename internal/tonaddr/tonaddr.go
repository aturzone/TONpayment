// Package tonaddr parses, validates, and normalizes TON addresses.
//
// A TON address identifies an account by (workchain, 32-byte hash). It has two
// textual forms:
//
//   - raw: "<workchain>:<64 hex>", e.g. "0:83dfd552e4...".
//   - user-friendly: a 36-byte structure (tag, workchain, hash, CRC16) encoded
//     as base64 — "EQ..." (bounceable) or "UQ..." (non-bounceable).
//
// User input is untrusted: a receiving address pasted in the wrong form, or with
// a corrupted checksum, must be rejected at the boundary rather than silently
// producing an address the chain explorer doesn't recognize (which would leave
// every invoice stuck pending — a fail-closed verifier never confirms a payment
// to an address it can't actually query). Normalize is the single choke point:
// it accepts any valid form and emits a canonical user-friendly url-safe string.
package tonaddr

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Address is a parsed, checksum-verified TON address.
type Address struct {
	Workchain  int32    // 0 (basechain) or -1 (masterchain)
	Hash       [32]byte // account id
	Bounceable bool     // EQ (true) vs UQ (false); informational for receiving
	Testnet    bool     // test-only flag in the user-friendly tag
}

// TON user-friendly tag bytes: bounceable/non-bounceable, OR'd with 0x80 on testnet.
const (
	tagBounceable    = 0x11
	tagNonBounceable = 0x51
	tagTestnet       = 0x80
)

// Parse decodes a raw or user-friendly TON address, verifying the checksum on the
// user-friendly form. It returns an error for anything it cannot prove is a valid
// address.
func Parse(s string) (Address, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Address{}, errors.New("empty address")
	}
	if i := strings.IndexByte(s, ':'); i >= 0 {
		return parseRaw(s[:i], s[i+1:])
	}
	return parseFriendly(s)
}

// Valid reports whether s is a well-formed TON address.
func Valid(s string) bool {
	_, err := Parse(s)
	return err == nil
}

// Normalize parses s and re-encodes it to the canonical user-friendly form
// (url-safe base64), preserving the bounceable flag. Use it at every boundary
// where an address enters the system so storage and toncenter queries are
// consistent.
func Normalize(s string) (string, error) {
	a, err := Parse(s)
	if err != nil {
		return "", err
	}
	return a.String(), nil
}

func parseRaw(wcStr, hexStr string) (Address, error) {
	wc, err := strconv.ParseInt(wcStr, 10, 32)
	if err != nil {
		return Address{}, fmt.Errorf("invalid workchain %q: %w", wcStr, err)
	}
	if wc != 0 && wc != -1 {
		return Address{}, fmt.Errorf("unsupported workchain %d (want 0 or -1)", wc)
	}
	if len(hexStr) != 64 {
		return Address{}, fmt.Errorf("raw hash must be 64 hex chars, got %d", len(hexStr))
	}
	h, err := hex.DecodeString(hexStr)
	if err != nil {
		return Address{}, fmt.Errorf("invalid hex hash: %w", err)
	}
	a := Address{Workchain: int32(wc), Bounceable: true} // raw carries no flag
	copy(a.Hash[:], h)
	return a, nil
}

func parseFriendly(s string) (Address, error) {
	raw, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		if raw, err = base64.StdEncoding.DecodeString(s); err != nil {
			return Address{}, errors.New("not a valid raw or base64 TON address")
		}
	}
	if len(raw) != 36 {
		return Address{}, fmt.Errorf("user-friendly address must be 36 bytes, got %d", len(raw))
	}
	var a Address
	switch raw[0] & ^byte(tagTestnet) {
	case tagBounceable:
		a.Bounceable = true
	case tagNonBounceable:
		a.Bounceable = false
	default:
		return Address{}, fmt.Errorf("invalid address tag 0x%02x", raw[0])
	}
	a.Testnet = raw[0]&tagTestnet != 0
	switch raw[1] {
	case 0x00:
		a.Workchain = 0
	case 0xff:
		a.Workchain = -1
	default:
		return Address{}, fmt.Errorf("unsupported workchain byte 0x%02x", raw[1])
	}
	want := uint16(raw[34])<<8 | uint16(raw[35])
	if got := crc16(raw[:34]); got != want {
		return Address{}, errors.New("address checksum mismatch")
	}
	copy(a.Hash[:], raw[2:34])
	return a, nil
}

// Friendly encodes the address in user-friendly url-safe base64 with the given
// bounceable flag.
func (a Address) Friendly(bounceable bool) string {
	buf := make([]byte, 36)
	tag := byte(tagBounceable)
	if !bounceable {
		tag = tagNonBounceable
	}
	if a.Testnet {
		tag |= tagTestnet
	}
	buf[0] = tag
	if a.Workchain == -1 {
		buf[1] = 0xff
	}
	copy(buf[2:34], a.Hash[:])
	crc := crc16(buf[:34])
	buf[34] = byte(crc >> 8)
	buf[35] = byte(crc)
	return base64.URLEncoding.EncodeToString(buf)
}

// String is the canonical user-friendly form, preserving the parsed bounceable flag.
func (a Address) String() string { return a.Friendly(a.Bounceable) }

// Raw is the "<workchain>:<hex>" form.
func (a Address) Raw() string {
	return strconv.FormatInt(int64(a.Workchain), 10) + ":" + hex.EncodeToString(a.Hash[:])
}

// crc16 is CRC-16/XMODEM (poly 0x1021, init 0x0000), the checksum TON uses in the
// user-friendly address form.
func crc16(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		crc ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
