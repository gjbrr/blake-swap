package amount

import (
	"testing"

	"github.com/gjbrr/blake-swap/pkg/decimal"
)

func TestParseBlakeExact(t *testing.T) {
	cases := map[string]int64{
		"0.00000001":  1,
		"0.0000001":   10,
		"0.1":         10_000_000,
		"1":           100_000_000,
		"1.23456789":  123_456_789,
		"0.5":         50_000_000,
		"100":         10_000_000_000,
		"1e-8":        1,
		"0.00000100":  100,
		"12.00000000": 1_200_000_000,
	}

	for in, want := range cases {
		got, err := ParseBlake(in)

		if err != nil {
			t.Fatalf("ParseBlake(%q): %v", in, err)
		}

		if int64(got) != want {
			t.Errorf("ParseBlake(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseBlakeRefusesWhatItCannotRepresent(t *testing.T) {
	cases := []any{
		"0", "-1", "0.000000001", "0.000000015", // finer than a base unit
		"abc", "", "NaN", "Inf", true, false, // not a number
		// A float that has no exact base-unit form; Python refuses this too.
		0.30000000000000004,
	}

	for _, in := range cases {
		if got, err := ParseBlake(in); err == nil {
			t.Errorf("ParseBlake(%v) accepted it as %d", in, got)
		}
	}
}

func TestParseBlakeAcceptsFloatsThatAreExact(t *testing.T) {
	// 0.1 as a float must mean exactly 0.1, not 0.1000000000000000055.
	b, err := ParseBlake(0.1)

	if err != nil {
		t.Fatal(err)
	}

	if int64(b) != 10_000_000 {
		t.Fatalf("ParseBlake(0.1) = %d", b)
	}

	if b2, err := ParseBlake(int64(2)); err != nil || b2 != 200_000_000 {
		t.Fatalf("ParseBlake(2) = %d, %v", b2, err)
	}

	if b3, err := ParseBlake(decimal.MustParse("3.5")); err != nil || b3 != 350_000_000 {
		t.Fatalf("ParseBlake(3.5 dec) = %d, %v", b3, err)
	}
}

func TestLegacyMigration(t *testing.T) {
	// An exact legacy value migrates unchanged and reports no change.
	b, changed, err := ParseBlakeLegacy(0.3)

	if err != nil {
		t.Fatal(err)
	}

	if changed || int64(b) != 30_000_000 {
		t.Fatalf("0.3 -> %d changed=%v", b, changed)
	}

	// The float artefact rounds to the nearest base unit and says so.
	b, changed, err = ParseBlakeLegacy(0.30000000000000004)

	if err != nil {
		t.Fatal(err)
	}

	if !changed || int64(b) != 30_000_000 {
		t.Fatalf("0.30000000000000004 -> %d changed=%v", b, changed)
	}

	// Half-even at the boundary: 1e-9 is a tie between 0 and 1 base units,
	// and 0 is even.
	b, changed, err = ParseBlakeLegacy("0.000000001")

	if err != nil {
		t.Fatal(err)
	}

	if b != 0 {
		t.Fatalf("legacy tie rounded to %d, want 0 (half-even)", b)
	}

	_ = changed
}

func TestFormatBlake(t *testing.T) {
	cases := map[int64]string{
		0:              "0",
		1:              "0.00000001",
		10:             "0.0000001",
		10_000_000:     "0.1",
		100_000_000:    "1",
		123_456_789:    "1.23456789",
		10_000_000_000: "100",
	}

	for units, want := range cases {
		if got := Blake(units).Format(); got != want {
			t.Errorf("Format(%d) = %q, want %q", units, got, want)
		}
	}
}

func TestParseSats(t *testing.T) {
	cases := map[string]int64{
		"0":          0,
		"0.00000001": 1,
		"0.00025000": 25_000,
		"0.1":        10_000_000,
		"1":          100_000_000,
		"1.00000000": 100_000_000,
		"25000":      2_500_000_000_000,
	}

	for in, want := range cases {
		got, err := ParseSats(in)

		if err != nil {
			t.Fatalf("ParseSats(%q): %v", in, err)
		}

		if int64(got) != want {
			t.Errorf("ParseSats(%q) = %d, want %d", in, got, want)
		}
	}

	// Sub-satoshi precision and negatives are refused, not rounded.
	for _, in := range []string{"0.000000001", "-1", "-0.00000001"} {
		if got, err := ParseSats(in); err == nil {
			t.Errorf("ParseSats(%q) accepted it as %d", in, got)
		}
	}
}

func TestCoinRendering(t *testing.T) {
	if got := Sats(25_000).String(); got != "0.00025" {
		t.Errorf("25000 sats = %q", got)
	}

	if got := Sats(100_000_000).String(); got != "1" {
		t.Errorf("1 coin = %q", got)
	}

	if got := Sats(1).String(); got != "0.00000001" {
		t.Errorf("1 sat = %q", got)
	}
}

func TestRoundTrip(t *testing.T) {
	// Every representable amount must render and parse back identically.
	for _, units := range []int64{1, 9, 10, 99, 100_000_000, 123_456_789, 1 << 40} {
		text := Blake(units).Format()

		back, err := ParseBlake(text)

		if err != nil {
			t.Fatalf("reparse %q: %v", text, err)
		}

		if back != Blake(units) {
			t.Fatalf("%d rendered as %q reparsed as %d", units, text, back)
		}
	}
}
