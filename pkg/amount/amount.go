// Package amount is exact money arithmetic for the swap engine.
//
// It is the port of the amount helpers in the Python engine, and it keeps the
// property that makes them useful: nothing is ever rounded into a different
// amount. A BLAKE amount that is finer than a base unit is refused, a node
// value that is finer than a satoshi is refused, and rendering is a plain
// decimal string with no exponent notation.
package amount

import (
	"fmt"

	"github.com/gjbrr/blake-swap/pkg/decimal"
)

// BLAKE amounts are integer base units, eight decimal places per coin. The
// wire and the durable record both carry base units, and the decimal string is
// only ever a rendering.
const (
	// Decimals is the number of decimal places in one BLAKE coin.
	Decimals = 8

	// BaseUnitsPerCoin is 10^Decimals.
	BaseUnitsPerCoin int64 = 100_000_000

	// SatsPerCoin is 10^8 for Bitcoin.
	SatsPerCoin int64 = 100_000_000
)

// Blake is an amount in BLAKE base units.
type Blake int64

// Sats is an amount in satoshis.
type Sats int64

// scalar turns the input types the engine accepts into an exact decimal.
//
// Accepting a float is deliberate: the wire carries decimal strings, but the
// Python engine also accepted floats and stored them before the base-unit
// migration, so legacy records arrive as floats. The conversion uses the
// shortest round-trip representation, which is what str() produced on the
// Python side, so a legacy value means the same thing on both sides.
func scalar(v any) (decimal.Dec, error) {
	switch value := v.(type) {
	case decimal.Dec:
		return value, nil
	case string:
		return decimal.Parse(value)
	case Coin:
		return decimal.Parse(string(value))
	case int:
		return decimal.FromInt64(int64(value)), nil
	case int32:
		return decimal.FromInt64(int64(value)), nil
	case int64:
		return decimal.FromInt64(value), nil
	case uint32:
		return decimal.FromInt64(int64(value)), nil
	case uint64:
		if value > 1<<62 {
			return decimal.Dec{}, fmt.Errorf("amount %d is too large", value)
		}

		return decimal.FromInt64(int64(value)), nil
	case float32:
		return decimal.FromFloat64(float64(value))
	case float64:
		return decimal.FromFloat64(value)
	case bool:
		return decimal.Dec{}, fmt.Errorf("amount must be a number, not a boolean")
	default:
		return decimal.Dec{}, fmt.Errorf("amount %v is not a decimal number", v)
	}
}

// ParseBlake converts an amount to integer base units, exactly.
//
// Anything finer than Decimals places is refused rather than rounded: the value
// would be a different amount on the chain than in the message that announced
// it.
func ParseBlake(v any) (Blake, error) {
	value, err := scalar(v)

	if err != nil {
		return 0, err
	}

	if value.Sign() <= 0 {
		return 0, fmt.Errorf("BLAKE amount must be positive")
	}

	units := value.Shift(Decimals)

	if !units.IsInteger() {
		return 0, fmt.Errorf(
			"BLAKE amount %s is finer than %d decimal places, which the protocol cannot represent",
			value, Decimals,
		)
	}

	n, err := units.Int64()

	if err != nil {
		return 0, fmt.Errorf("BLAKE amount %s is out of range", value)
	}

	return Blake(n), nil
}

// ParseBlakeLegacy converts a pre-migration stored amount, which was a float.
//
// Legacy state was written from a Python float, so it can hold a value like
// 0.30000000000000004 that has no exact base-unit form. Round half-even to the
// nearest base unit and report whether that was necessary, so a migration can
// say what it changed rather than silently discarding value.
func ParseBlakeLegacy(v any) (Blake, bool, error) {
	value, err := scalar(v)

	if err != nil {
		return 0, false, err
	}

	if value.Sign() <= 0 {
		return 0, false, fmt.Errorf(
			"legacy BLAKE amount %s is not a positive finite number", value,
		)
	}

	quantized, err := value.Quo(decimal.FromInt64(1), Decimals, decimal.HalfEven)

	if err != nil {
		return 0, false, err
	}

	units, err := quantized.Shift(Decimals).Int64()

	if err != nil {
		return 0, false, fmt.Errorf("legacy BLAKE amount %s is out of range", value)
	}

	return Blake(units), quantized.Cmp(value) != 0, nil
}

// ParseSats converts a node-reported coin amount to integer satoshis, exactly.
//
// The node reports output values as decimal strings already quantised to one
// satoshi, so this is lossless; anything finer is refused rather than rounded
// into a different amount.
func ParseSats(v any) (Sats, error) {
	value, err := scalar(v)

	if err != nil {
		return 0, err
	}

	if value.Sign() < 0 {
		return 0, fmt.Errorf("Bitcoin amount %s must be non-negative", value)
	}

	sats := value.Shift(8)

	if !sats.IsInteger() {
		return 0, fmt.Errorf("Bitcoin amount %s is finer than one satoshi", value)
	}

	n, err := sats.Int64()

	if err != nil {
		return 0, fmt.Errorf("Bitcoin amount %s is out of range", value)
	}

	return Sats(n), nil
}

// Decimal returns the exact coin value of a BLAKE base-unit amount.
func (b Blake) Decimal() decimal.Dec {
	return decimal.FromInt64(int64(b)).Shift(-Decimals)
}

// Coin returns the exact coin value of a satoshi amount.
func (s Sats) Coin() decimal.Dec {
	return decimal.FromInt64(int64(s)).Shift(-8)
}

// Format renders the canonical wire and human form: a plain decimal string,
// never exponent notation, with trailing zeros trimmed.
func (b Blake) Format() string {
	return b.Decimal().String()
}

// String implements fmt.Stringer with the canonical rendering.
func (b Blake) String() string { return b.Format() }

// String renders satoshis as a coin amount.
func (s Sats) String() string { return s.Coin().String() }

// ParseBlakeString is ParseBlake restricted to the wire type.
func ParseBlakeString(s string) (Blake, error) { return ParseBlake(s) }

// MustParseBlake is ParseBlake for constants and tests.
func MustParseBlake(s string) Blake {
	b, err := ParseBlake(s)
	if err != nil {
		panic(err)
	}

	return b
}

// MustParseSats is ParseSats for constants and tests.
func MustParseSats(v any) Sats {
	s, err := ParseSats(v)
	if err != nil {
		panic(err)
	}

	return s
}
