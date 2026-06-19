// Package deeplink builds ton://transfer payment URIs and renders them as QR
// codes, so a payer can tap (deeplink) or scan (QR) to pay an invoice from any
// TON wallet.
package deeplink

import (
	"fmt"
	"net/url"
	"strconv"

	qrcode "github.com/skip2/go-qrcode"
)

// Build returns a ton://transfer deeplink for paying an invoice:
//
//	ton://transfer/<payTo>?amount=<nanoTON>&text=<memo>
//
// The amount is the integer nanoTON value and text is the memo the payer must
// include so the verifier can match the transfer.
func Build(payTo string, amountNano int64, memo string) string {
	q := url.Values{}
	q.Set("amount", strconv.FormatInt(amountNano, 10))
	q.Set("text", memo)
	return fmt.Sprintf("ton://transfer/%s?%s", payTo, q.Encode())
}

// PNG renders data (typically a deeplink) as a QR-code PNG of the given pixel
// size. Size is clamped to a sane range.
func PNG(data string, size int) ([]byte, error) {
	switch {
	case size < 64:
		size = 256
	case size > 1024:
		size = 1024
	}
	return qrcode.Encode(data, qrcode.Medium, size)
}
