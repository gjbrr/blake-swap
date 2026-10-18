package amount

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Coin is a coin amount exactly as a node wrote it in JSON.
//
// Bitcoin Core is not consistent about the shape of the same quantity:
// decoderawtransaction and getrawtransaction write output values as bare JSON
// numbers ("value": 0.00010000), while listunspent and gettransaction write the
// same figure as a quoted string ("amount": "0.00010000"). One struct field has
// to accept both forms.
//
// Neither form may pass through a float on the way in. 0.1 has no exact binary
// representation, so a float would turn an exact amount into a nearby one and
// the engine would compare its own records against the wrong number. Coin
// therefore keeps the digits the node sent, and Sats parses them exactly.
type Coin string

// UnmarshalJSON accepts a quoted or a bare decimal amount.
//
// A bare JSON number is only accepted when it is a plain decimal: digits with
// at most one decimal point and an optional sign. Exponents and the JSON
// non-numbers are refused, because the node never writes them and a parser
// that guessed at them could silently read a different amount than the one on
// the wire.
func (c *Coin) UnmarshalJSON(data []byte) error {
	text := strings.TrimSpace(string(data))

	switch {
	case text == "null":
		*c = ""

		return nil
	case text == "":
		return fmt.Errorf("amount is empty")
	case text[0] == '"':
		var quoted string

		if err := json.Unmarshal(data, &quoted); err != nil {
			return fmt.Errorf("amount %s: %w", text, err)
		}

		// Quoting is not a way around the shape check: a node's string
		// amounts have the same digits as its numeric ones.
		if !isPlainDecimal(quoted) {
			return fmt.Errorf("amount %q is not a plain decimal number", quoted)
		}

		*c = Coin(quoted)

		return nil
	case !isPlainDecimal(text):
		return fmt.Errorf("amount %q is not a plain decimal number", text)
	}

	*c = Coin(text)

	return nil
}

// MarshalJSON writes an amount back as a quoted decimal string.
//
// A string round-trips through every node and client without the precision
// question that a JSON number raises for a reader whose numbers are floats.
func (c Coin) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(c))
}

// String returns the amount's digits.
func (c Coin) String() string { return string(c) }

// Sats converts the amount to integer satoshis, exactly.
func (c Coin) Sats() (Sats, error) { return ParseSats(string(c)) }

// isPlainDecimal reports whether text is a plain decimal number.
func isPlainDecimal(text string) bool {
	digits, point := 0, false

	for i, r := range text {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case (r == '-' || r == '+') && i == 0:
		case r == '.' && !point:
			point = true
		default:
			return false
		}
	}

	return digits > 0
}
