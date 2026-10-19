package amount

import (
	"encoding/json"
	"testing"
)

// TestCoinAcceptsBothNodeShapes is the reason Coin exists: the same quantity
// arrives as a bare number from getrawtransaction and as a quoted string from
// listunspent, and both have to land in the same field.
func TestCoinAcceptsBothNodeShapes(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  Sats
	}{
		{name: "bare number", input: `0.00010000`, want: 10000},
		{name: "quoted string", input: `"0.00010000"`, want: 10000},
		{name: "integer number", input: `1`, want: 100_000_000},
		{name: "quoted integer", input: `"2"`, want: 200_000_000},
		{name: "trailing zeros are exact", input: `0.10000000`, want: 10_000_000},
		{name: "one satoshi, bare", input: `0.00000001`, want: 1},
		{name: "one satoshi, quoted", input: `"0.00000001"`, want: 1},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var coin Coin

			if err := json.Unmarshal([]byte(c.input), &coin); err != nil {
				t.Fatalf("%s: %v", c.input, err)
			}

			sats, err := coin.Sats()

			if err != nil {
				t.Fatalf("%s: %v", c.input, err)
			}

			if sats != c.want {
				t.Fatalf("%s = %d sats, want %d", c.input, sats, c.want)
			}
		})
	}
}

// TestCoinRefusesNegativeAmounts: a node never reports one, and ParseSats
// refuses it rather than handing back a negative balance.
func TestCoinRefusesNegativeAmounts(t *testing.T) {
	var coin Coin

	if err := json.Unmarshal([]byte(`-0.00000001`), &coin); err != nil {
		t.Fatalf("the digits should parse even for a negative amount: %v", err)
	}

	if _, err := coin.Sats(); err == nil {
		t.Fatal("a negative amount was accepted")
	}
}

// TestCoinRefusesWhatTheNodeNeverWrites keeps the exactness claim honest: if a
// shape that is not a decimal reached this type it could be read as a different
// number than the one on the wire.
//
// Exponent notation is not in this list: Core writes it for small amounts, and
// TestCoinAcceptsExponentNotationFromTheNode pins that it is expanded exactly
// rather than silently refused or read as a float.
func TestCoinRefusesWhatTheNodeNeverWrites(t *testing.T) {
	for _, input := range []string{
		`NaN`,         // not a JSON number at all
		`Infinity`,    //
		`0x10`,        // hex is a different number
		`1.2.3`,       // two decimal points
		`.`,           // no digits
		`""`,          // a node never reports an empty amount
		`true`,        // a boolean is not an amount
		`{"value":1}`, // an object is not an amount
		`1e`,          // an exponent with no value
		`e-6`,         // no mantissa
		`1.2e-6.5`,    // a fractional exponent is not a number
	} {
		t.Run(input, func(t *testing.T) {
			var coin Coin

			if err := json.Unmarshal([]byte(input), &coin); err == nil {
				t.Fatalf("%s was accepted as %q", input, coin)
			}
		})
	}
}

// TestCoinFinerThanASatoshiIsRefused covers the boundary: the value is
// representable as JSON but not on the chain, and rounding it would silently
// change the amount.
func TestCoinFinerThanASatoshiIsRefused(t *testing.T) {
	for _, input := range []string{`0.000000001`, `"0.000000019"`} {
		var coin Coin

		if err := json.Unmarshal([]byte(input), &coin); err != nil {
			t.Fatalf("%s: %v", input, err)
		}

		if _, err := coin.Sats(); err == nil {
			t.Fatalf("%s was rounded to a satoshi", input)
		}
	}
}

// TestCoinNullIsEmpty covers gettxout-style nulls and the zero that must not
// be invented for them.
func TestCoinNullIsEmpty(t *testing.T) {
	var coin Coin

	if err := json.Unmarshal([]byte(`null`), &coin); err != nil {
		t.Fatal(err)
	}

	if coin != "" {
		t.Fatalf("null became %q", coin)
	}

	if _, err := coin.Sats(); err == nil {
		t.Fatal("an empty amount parsed as a value")
	}
}

// TestCoinRoundTripsAsString keeps the wire shape stable: an amount written
// back out is a quoted string, so a reader whose numbers are floats cannot
// lose it.
func TestCoinRoundTripsAsString(t *testing.T) {
	var coin Coin

	if err := json.Unmarshal([]byte(`0.00010000`), &coin); err != nil {
		t.Fatal(err)
	}

	out, err := json.Marshal(coin)

	if err != nil {
		t.Fatal(err)
	}

	if string(out) != `"0.00010000"` {
		t.Fatalf("marshalled as %s", out)
	}

	// A field carrying the type behaves the same way in a struct.
	var record struct {
		Value Coin `json:"value"`
	}

	if err := json.Unmarshal([]byte(`{"value":0.00010000}`), &record); err != nil {
		t.Fatal(err)
	}

	encoded, err := json.Marshal(record)

	if err != nil {
		t.Fatal(err)
	}

	if string(encoded) != `{"value":"0.00010000"}` {
		t.Fatalf("struct marshalled as %s", encoded)
	}

	sats, err := record.Value.Sats()

	if err != nil {
		t.Fatal(err)
	}

	if sats != 10000 {
		t.Fatalf("round trip lost value: %d sats", sats)
	}
}

// TestCoinAcceptsExponentNotationFromTheNode: Bitcoin Core writes a small enough
// amount as a JSON number in exponent notation. `1.13e-06` is 113 satoshis, and a
// parser that refused it would refuse a real answer; one that read it as a float
// would read it as a different amount.
func TestCoinAcceptsExponentNotationFromTheNode(t *testing.T) {
	cases := map[string]int64{
		"1.13e-06":       113,
		"1.13E-6":        113,
		"2.1e-05":        2100,
		"1e-08":          1,
		"5e-08":          5,
		"0.00000001":     1,
		"1.5":            150000000,
		"0e-8":           0,
		"0.0":            0,
		"2.1e+5":         21000000000000,
		`"1.13e-06"`:     113,
		"1.00000000e+00": 100000000,
	}

	for text, want := range cases {
		var coin Coin

		if err := json.Unmarshal([]byte(text), &coin); err != nil {
			t.Errorf("%s: %v", text, err)

			continue
		}

		sats, err := coin.Sats()

		if err != nil {
			t.Errorf("%s: %v", text, err)

			continue
		}

		if int64(sats) != want {
			t.Errorf("%s = %d sats, want %d", text, sats, want)
		}
	}
}

// TestCoinRefusesWhatIsNotADecimal keeps the parser strict about everything the
// node does not write.
func TestCoinRefusesWhatIsNotADecimal(t *testing.T) {
	for _, text := range []string{
		"NaN", "Infinity", "1.2.3", "0x10", "1e", "e-6", "1.2e-6.5", "1,5", "  ",
	} {
		var coin Coin

		if err := json.Unmarshal([]byte(text), &coin); err == nil {
			t.Errorf("%q was accepted as %q", text, coin)
		}
	}
}
