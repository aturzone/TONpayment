package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/aturzone/TONpayment/internal/tonaddr"
	"github.com/xssnick/tonutils-go/ton/wallet"
)

// TestPubKeyOwnsAddress derives the address for a key under every supported wallet
// variant and confirms the key (and only that key) owns it.
func TestPubKeyOwnsAddress(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)

	for i, v := range walletVariants {
		a, err := wallet.AddressFromPubKey(pub, v.cfg, v.sub)
		if err != nil {
			t.Fatalf("variant %d AddressFromPubKey: %v", i, err)
		}
		var h [32]byte
		copy(h[:], a.Data())
		addr := tonaddr.Address{Workchain: int32(a.Workchain()), Hash: h}

		if !PubKeyOwnsAddress(pub, addr) {
			t.Fatalf("variant %d: the deriving key must own its address", i)
		}
		if PubKeyOwnsAddress(other, addr) {
			t.Fatalf("variant %d: a different key must NOT own the address (impersonation)", i)
		}
	}
}
