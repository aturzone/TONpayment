// Package money handles TON amounts as integer nanoTON to avoid float errors.
package money

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Nano is the number of nanoTON in 1 TON.
const Nano int64 = 1_000_000_000

// TONToNano converts a decimal TON amount to integer nanoTON, rounding to the
// nearest nanoTON. Use only at the boundary (e.g. parsing human input).
func TONToNano(t float64) int64 {
	return int64(math.Round(t * float64(Nano)))
}

// NanoToTONString renders nanoTON as a trimmed decimal TON string ("2.5", "3").
func NanoToTONString(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	whole := n / Nano
	frac := n % Nano
	var s string
	if frac == 0 {
		s = strconv.FormatInt(whole, 10)
	} else {
		s = fmt.Sprintf("%d.%09d", whole, frac)
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	if neg {
		return "-" + s
	}
	return s
}
