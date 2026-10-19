package amount

import (
	"encoding/json"
	"fmt"
	"strconv"
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
// A node writes an amount in one of two shapes: a plain decimal, or -- for a
// small enough value -- a JSON number in exponent notation. Core really does
// print `1.13e-06` for a 113-satoshi output, so refusing exponents would refuse a
// real answer, and reading one through a float would lose the exact amount.
//
// Exponent notation is therefore expanded into a plain decimal by moving the
// point, digit by digit, and then parsed by the same exact path as every other
// amount. Nothing is rounded and nothing goes through a float.
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

		expanded, err := expandExponent(quoted)

		if err != nil {
			return err
		}

		// Quoting is not a way around the shape check: a node's string
		// amounts have the same digits as its numeric ones.
		if !isPlainDecimal(expanded) {
			return fmt.Errorf("amount %q is not a plain decimal number", quoted)
		}

		*c = Coin(expanded)

		return nil
	}

	expanded, err := expandExponent(text)

	if err != nil {
		return err
	}

	if !isPlainDecimal(expanded) {
		return fmt.Errorf("amount %q is not a plain decimal number", text)
	}

	*c = Coin(expanded)

	return nil
}

// expandExponent rewrites exponent notation as a plain decimal.
//
// `1.13e-06` becomes `0.00000113`: the digits are moved, never rounded, so the
// value that reaches Sats is exactly the one the node sent.
func expandExponent(text string) (string, error) {
	if !strings.ContainsAny(text, "eE") {
		return text, nil
	}

	mantissa := text
	exponent := 0

	if index := strings.IndexAny(text, "eE"); index >= 0 {
		mantissa = text[:index]

		parsed, err := strconv.Atoi(strings.TrimSpace(text[index+1:]))

		if err != nil {
			return "", fmt.Errorf("amount %q has an unreadable exponent", text)
		}

		exponent = parsed
	}

	sign := ""

	switch {
	case strings.HasPrefix(mantissa, "-"):
		sign = "-"
		mantissa = mantissa[1:]
	case strings.HasPrefix(mantissa, "+"):
		mantissa = mantissa[1:]
	}

	integer := mantissa
	fraction := ""

	if index := strings.IndexByte(mantissa, '.'); index >= 0 {
		integer = mantissa[:index]
		fraction = mantissa[index+1:]
	}

	for _, part := range []string{integer, fraction} {
		for _, digit := range part {
			if digit < '0' || digit > '9' {
				return "", fmt.Errorf("amount %q is not a decimal number", text)
			}
		}
	}

	digits := integer + fraction
	point := len(integer) + exponent

	// A point at or before the first digit needs leading zeros; past the last
	// digit needs trailing zeros. Both are the same move of the point.
	switch {
	case point <= 0:
		digits = strings.Repeat("0", 1-point) + digits
		point = 1
	case point > len(digits):
		digits += strings.Repeat("0", point-len(digits))
	}

	whole := digits[:point]
	part := digits[point:]

	whole = strings.TrimLeft(whole, "0")

	if whole == "" {
		whole = "0"
	}

	part = strings.TrimRight(part, "0")

	expanded := whole

	if part != "" {
		expanded += "." + part
	}

	if sign == "-" && expanded != "0" {
		expanded = "-" + expanded
	}

	return expanded, nil
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
