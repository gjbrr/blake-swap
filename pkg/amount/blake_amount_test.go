package amount

import "testing"

// This file mirrors the reference's `tests/blake_amount_tests.py`: the canonical wire strings,
// the accepted input shapes, and the refusals. Amounts are the one value the protocol compares
// for equality on both sides of a swap, so a format that disagrees by one base unit is a
// settlement that one implementation refuses and the other signs.

// TestBlakeAmountCanonicalWireStrings: the rendering the reference writes onto the wire and
// into records.
func TestBlakeAmountCanonicalWireStrings(t *testing.T) {
	cases := map[Blake]string{
		1:           "0.00000001",
		10_000_000:  "0.1",
		20_000_000:  "0.2",
		100_000_000: "1",
		12_345_678:  "0.12345678",
	}

	for units, want := range cases {
		if got := units.Format(); got != want {
			t.Errorf("%d rendered as %q, want %q", int64(units), got, want)
		}
	}
}

// TestBlakeAmountAcceptsTheSharesTheReferenceAccepts: whole coins, an exact decimal string, and
// a float with an exact decimal form — the last is the legacy path and has to keep working.
func TestBlakeAmountAcceptsTheSharesTheReferenceAccepts(t *testing.T) {
	cases := map[any]Blake{
		1:      100_000_000,
		"0.25": 25_000_000,
		0.2:    20_000_000,
	}

	for input, want := range cases {
		got, err := ParseBlake(input)

		if err != nil {
			t.Errorf("%v: %v", input, err)

			continue
		}

		if got != want {
			t.Errorf("%v = %d base units, want %d", input, int64(got), int64(want))
		}
	}
}

// TestBlakeAmountRefusesUnrepresentableAmounts: a value finer than one base unit cannot be
// represented, and rounding it would change what both sides agreed to.
func TestBlakeAmountRefusesUnrepresentableAmounts(t *testing.T) {
	for _, input := range []any{
		"0.000000001",
		"0.000000005",
		1e-9,
		"-1",
		"-0.00000001",
	} {
		if _, err := ParseBlake(input); err == nil {
			t.Errorf("%v was accepted", input)
		}
	}
}

// TestBlakeAmountOneBaseUnitApartIsNotEqual: the comparison that matters is exact, and the
// smallest representable difference has to survive parsing rather than collapse.
func TestBlakeAmountOneBaseUnitApartIsNotEqual(t *testing.T) {
	lower, err := ParseBlake("0.12345677")

	if err != nil {
		t.Fatal(err)
	}

	upper, err := ParseBlake("0.12345678")

	if err != nil {
		t.Fatal(err)
	}

	if upper-lower != 1 {
		t.Fatalf("difference = %d base units, want 1", int64(upper-lower))
	}

	if upper == lower {
		t.Fatal("amounts one base unit apart compared equal")
	}
}

// TestBlakeAmountZeroFormatsAsZero: zero is a value the protocol refuses to trade, but it is a
// value it has to be able to print.
func TestBlakeAmountZeroFormatsAsZero(t *testing.T) {
	if got := Blake(0).Format(); got != "0" {
		t.Fatalf("zero rendered as %q", got)
	}
}

// TestBlakeAmountSatsAreExact: a node reports Bitcoin as a coin value, and converting it to
// satoshis is exact or refused — never rounded, because the round-off is money.
func TestBlakeAmountSatsAreExact(t *testing.T) {
	cases := map[any]Sats{
		"0.00025000": 25_000,
		"1":          100_000_000,
		"0.00000001": 1,
	}

	for coin, want := range cases {
		got, err := ParseSats(coin)

		if err != nil {
			t.Errorf("%v: %v", coin, err)

			continue
		}

		if got != want {
			t.Errorf("%v = %d sats, want %d", coin, int64(got), int64(want))
		}
	}

	for name, bad := range map[string]any{
		"a sub-satoshi amount": "0.000000001",
		"a non-finite amount":  "NaN",
		"a negative amount":    "-1",
		"a boolean":            true,
	} {
		if _, err := ParseSats(bad); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
