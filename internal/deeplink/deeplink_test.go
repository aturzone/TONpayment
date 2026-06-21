package deeplink

import (
	"strings"
	"testing"
)

func TestBuild(t *testing.T) {
	dl := Build("UQabc", 2_500_000_000, "TON-xyz")
	if !strings.HasPrefix(dl, "ton://transfer/UQabc?") {
		t.Fatalf("unexpected prefix: %s", dl)
	}
	if !strings.Contains(dl, "amount=2500000000") {
		t.Fatalf("amount missing/wrong: %s", dl)
	}
	if !strings.Contains(dl, "text=TON-xyz") {
		t.Fatalf("memo missing/wrong: %s", dl)
	}
}

func TestBuildEscapesMemo(t *testing.T) {
	dl := Build("UQabc", 1, "a b&c")
	if strings.Contains(dl, "a b&c") {
		t.Fatalf("memo must be url-encoded, got raw in: %s", dl)
	}
	if !strings.Contains(dl, "text=a+b%26c") {
		t.Fatalf("memo not encoded as expected: %s", dl)
	}
}

func TestPNGClampsSizeAndRendersPNG(t *testing.T) {
	png, err := PNG("ton://transfer/UQabc?amount=1", 10) // below 64 -> clamps to 256
	if err != nil {
		t.Fatal(err)
	}
	// PNG magic number
	if len(png) < 8 || png[0] != 0x89 || png[1] != 'P' || png[2] != 'N' || png[3] != 'G' {
		t.Fatalf("output is not a PNG (len=%d)", len(png))
	}
}
