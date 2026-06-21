package auth

import (
	"bytes"
	"crypto/ed25519"

	"github.com/aturzone/TONpayment/internal/tonaddr"
	"github.com/xssnick/tonutils-go/ton/wallet"
)

// walletVariant is a wallet contract version + the subwallet number it derives an
// address with. Tonkeeper uses W5 (V5R1, subwallet 0) and V4R2 (default subwallet);
// the rest cover older wallets. A wallet address is the hash of its StateInit,
// derived deterministically from (public key, version, subwallet), so we can bind a
// key to an address by reconstruction — without the wallet being deployed on-chain.
type walletVariant struct {
	cfg wallet.VersionConfig
	sub uint32
}

// Mainnet variants (the platform targets mainnet; only V5's address depends on the
// network id, so older versions are network-agnostic).
var walletVariants = []walletVariant{
	{wallet.ConfigV5R1Final{NetworkGlobalID: wallet.MainnetGlobalID, Workchain: 0}, 0},
	{wallet.V4R2, wallet.DefaultSubwallet},
	{wallet.V4R1, wallet.DefaultSubwallet},
	{wallet.V3R2, wallet.DefaultSubwallet},
	{wallet.V3R1, wallet.DefaultSubwallet},
}

// PubKeyOwnsAddress reports whether pub is the public key behind addr, by deriving
// the wallet address from the key for each standard variant and comparing. This
// binds a TON Connect-supplied publicKey to the claimed address WITHOUT the wallet
// being deployed (works for brand-new wallets), which on-chain get_public_key
// cannot do. An attacker cannot claim a victim's address: that needs the victim's
// public key, and then the proof signature would need the victim's private key.
func PubKeyOwnsAddress(pub ed25519.PublicKey, addr tonaddr.Address) bool {
	if len(pub) != ed25519.PublicKeySize {
		return false
	}
	for _, v := range walletVariants {
		a, err := wallet.AddressFromPubKey(pub, v.cfg, v.sub)
		if err != nil {
			continue
		}
		if int32(a.Workchain()) == addr.Workchain && bytes.Equal(a.Data(), addr.Hash[:]) {
			return true
		}
	}
	return false
}
