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
// float-shaped amount ever reached this type it would be read as a different
// number than the one on the wire.
func TestCoinRefusesWhatTheNodeNeverWrites(t *testing.T) {
	for _, input := range []string{
		`1e-05`,       // an exponent hides the digit positions
		`1E5`,         //
		`"1e-05"`,     // quoting is not an excuse to pass the check
		`NaN`,         // not a JSON number at all
		`Infinity`,    //
		`0x10`,        // hex is a different number
		`1.2.3`,       // two decimal points
		`.`,           // no digits
		`""`,          // a node never reports an empty amount
		`true`,        // a boolean is not an amount
		`{"value":1}`, // an object is not an amount
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
