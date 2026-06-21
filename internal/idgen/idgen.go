// Package idgen creates short, URL-safe, prefixed random IDs.
package idgen

import (
	"crypto/rand"
	"encoding/base64"
)

// New returns a random ID like "inv_3Fa9...". The 9 random bytes give 72 bits of
// entropy, ample to avoid collisions for invoice IDs.
func New(prefix string) string {
	b := make([]byte, 9)
	// IDs must be unguessable; a crypto/rand failure means broken entropy — fail
	// loud rather than emitting a predictable/zero id.
	if _, err := rand.Read(b); err != nil {
		panic("idgen: crypto/rand failed: " + err.Error())
	}
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(b)
}
