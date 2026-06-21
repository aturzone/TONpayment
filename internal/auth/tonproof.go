package auth

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"

	"github.com/aturzone/TONpayment/internal/tonaddr"
)

// Proof is the ton_proof a TON Connect wallet returns when signing in. Signature is
// the raw 64-byte ed25519 signature; Payload is our challenge nonce echoed back.
type Proof struct {
	Timestamp int64
	Domain    string // domain.value, e.g. "tonpayment.net"
	Payload   string
	Signature []byte
}

// proofMessage assembles the bytes the wallet signs, per the TON Connect ton_proof
// specification:
//
//	"ton-proof-item-v2/"
//	  ++ workchain   (int32, big-endian)
//	  ++ address     (32-byte account hash)
//	  ++ domainLen   (uint32, little-endian)
//	  ++ domain      (utf-8 bytes)
//	  ++ timestamp   (uint64, little-endian)
//	  ++ payload     (utf-8 bytes)
func proofMessage(addr tonaddr.Address, p Proof) []byte {
	out := make([]byte, 0, 64+len(p.Domain)+len(p.Payload))
	out = append(out, "ton-proof-item-v2/"...)

	var wc [4]byte
	binary.BigEndian.PutUint32(wc[:], uint32(addr.Workchain))
	out = append(out, wc[:]...)

	out = append(out, addr.Hash[:]...)

	var dl [4]byte
	binary.LittleEndian.PutUint32(dl[:], uint32(len(p.Domain)))
	out = append(out, dl[:]...)
	out = append(out, p.Domain...)

	var ts [8]byte
	binary.LittleEndian.PutUint64(ts[:], uint64(p.Timestamp))
	out = append(out, ts[:]...)

	out = append(out, p.Payload...)
	return out
}

// VerifyProof reports whether p was signed by the private key matching pubkey for
// the given address. The signed digest follows the spec's outer wrapping:
//
//	ed25519.Verify(pubkey, sha256(0xffff ++ "ton-connect" ++ sha256(message)), sig)
func VerifyProof(pubkey ed25519.PublicKey, addr tonaddr.Address, p Proof) bool {
	if len(pubkey) != ed25519.PublicKeySize || len(p.Signature) != ed25519.SignatureSize {
		return false
	}
	inner := sha256.Sum256(proofMessage(addr, p))

	full := make([]byte, 0, 2+len("ton-connect")+sha256.Size)
	full = append(full, 0xff, 0xff)
	full = append(full, "ton-connect"...)
	full = append(full, inner[:]...)
	digest := sha256.Sum256(full)

	return ed25519.Verify(pubkey, digest[:], p.Signature)
}
