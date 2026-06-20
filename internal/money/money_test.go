package money

import "testing"

func TestNanoToTONString(t *testing.T) {
	cases := map[int64]string{
		0:              "0",
		1_000_000_000:  "1",
		3_000_000_000:  "3",
		2_500_000_000:  "2.5",
		1_500_000_000:  "1.5",
		1:              "0.000000001",
		-2_500_000_000: "-2.5",
	}
	for n, want := range cases {
		if got := NanoToTONString(n); got != want {
			t.Errorf("NanoToTONString(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestTONToNanoAndRoundTrip(t *testing.T) {
	if got := TONToNano(2.5); got != 2_500_000_000 {
		t.Errorf("TONToNano(2.5) = %d, want 2500000000", got)
	}
	if got := NanoToTONString(TONToNano(3.0)); got != "3" {
		t.Errorf("round-trip 3.0 = %q, want \"3\"", got)
	}
}
