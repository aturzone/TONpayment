package telegrambot

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
)

// secretCipher encrypts TOTP secrets at rest (AES-256-GCM) so a database dump
// alone cannot mint valid 2FA codes. The key is derived from the platform
// session secret with a domain-separation tag, so no extra secret needs to be
// provisioned or rotated separately.
type secretCipher struct {
	aead cipher.AEAD
}

func newSecretCipher(masterSecret string) (*secretCipher, error) {
	if masterSecret == "" {
		return nil, errors.New("telegram bot: an encryption secret is required (TON_SESSION_SECRET)")
	}
	key := sha256.Sum256([]byte("tonpayment-telegram-totp|" + masterSecret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &secretCipher{aead: aead}, nil
}

// encrypt seals plaintext with a fresh nonce; output is base64(nonce||ct).
func (c *secretCipher) encrypt(plaintext string) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := c.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ct), nil
}

// decrypt opens base64(nonce||ct); any tamper/format error fails closed.
func (c *secretCipher) decrypt(enc string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", err
	}
	ns := c.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("ciphertext too short")
	}
	pt, err := c.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}
