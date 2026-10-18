// Package decimal is an exact decimal type.
//
// The Python engine uses decimal.Decimal for money and feerates, and the
// difference from float is not cosmetic: 0.1+0.2 must be 0.3, a feerate of
// 1.19 sat/vB times 140 vB must ceil to exactly 167, and an amount finer than
// a base unit must be refused rather than rounded into a different amount.
//
// The representation is an arbitrary-precision integer and a power of ten, so
// every operation is exact except division, which says how it rounds. This is
// deliberately a small implementation: only the operations the swap engine
// needs, each one auditable.
package decimal

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// Mode selects how an inexact division is rounded.
type Mode int

const (
	// Floor rounds toward negative infinity.
	Floor Mode = iota
	// Ceil rounds toward positive infinity.
	Ceil
	// HalfEven rounds to nearest, ties to even, as Python's default does.
	HalfEven
)

// Dec is an exact decimal number.
//
// The zero value is zero. A Dec is value-comparable only through Cmp; it holds
// a pointer, so do not compare with ==.
type Dec struct {
	// unscaled is the significand: the value is unscaled * 10^-scale.
	unscaled *big.Int
	// scale is the number of fractional digits; it may be negative, meaning
	// the value is unscaled * 10^-scale with a positive exponent.
	scale int32
}

var bigZero = big.NewInt(0)
var bigTen = big.NewInt(10)

// Zero returns zero.
func Zero() Dec { return Dec{} }

// FromInt64 returns n exactly.
func FromInt64(n int64) Dec {
	return Dec{unscaled: big.NewInt(n)}
}

// FromInt returns n exactly.
func FromInt(n *big.Int) Dec {
	if n == nil {
		return Dec{}
	}

	return Dec{unscaled: new(big.Int).Set(n)}
}

// FromFloat64 converts a float using its shortest round-trip representation,
// which is what Python's repr() produces and therefore what the Python engine
// parses when it receives a float.
func FromFloat64(f float64) (Dec, error) {
	return Parse(strconv.FormatFloat(f, 'g', -1, 64))
}

// MustParse is Parse for constants and tests.
func MustParse(s string) Dec {
	d, err := Parse(s)
	if err != nil {
		panic(err)
	}

	return d
}

// Parse parses a decimal string exactly.
//
// Accepted: an optional sign, digits with an optional decimal point, and an
// optional exponent ("1e-8", "1.19", "-0.5e3"). Rejected: everything else,
// including NaN and infinities, so callers get one class of failure.
func Parse(s string) (Dec, error) {
	text := strings.TrimSpace(s)

	if text == "" {
		return Dec{}, fmt.Errorf("decimal: empty string")
	}

	neg := false

	switch text[0] {
	case '+':
		text = text[1:]
	case '-':
		neg = true
		text = text[1:]
	}

	if text == "" {
		return Dec{}, fmt.Errorf("decimal: %q has no digits", s)
	}

	mantissa := text
	exp := int64(0)

	if i := strings.IndexAny(text, "eE"); i >= 0 {
		mantissa = text[:i]
		expText := text[i+1:]

		if expText == "" {
			return Dec{}, fmt.Errorf("decimal: %q has an empty exponent", s)
		}

		parsed, err := strconv.ParseInt(expText, 10, 32)

		if err != nil {
			return Dec{}, fmt.Errorf("decimal: %q has an invalid exponent", s)
		}

		exp = parsed
	}

	intPart := mantissa
	fracPart := ""

	if i := strings.IndexByte(mantissa, '.'); i >= 0 {
		intPart = mantissa[:i]
		fracPart = mantissa[i+1:]

		if strings.ContainsRune(fracPart, '.') {
			return Dec{}, fmt.Errorf("decimal: %q has two decimal points", s)
		}
	}

	if intPart == "" && fracPart == "" {
		return Dec{}, fmt.Errorf("decimal: %q has no digits", s)
	}

	for _, part := range []string{intPart, fracPart} {
		for _, r := range part {
			if r < '0' || r > '9' {
				return Dec{}, fmt.Errorf("decimal: %q is not a number", s)
			}
		}
	}

	digits := intPart + fracPart

	if digits == "" {
		return Dec{}, fmt.Errorf("decimal: %q has no digits", s)
	}

	unscaled := new(big.Int)

	if _, ok := unscaled.SetString(digits, 10); !ok {
		return Dec{}, fmt.Errorf("decimal: %q is not a number", s)
	}

	if neg {
		unscaled.Neg(unscaled)
	}

	// value = digits * 10^(-len(frac)) * 10^exp
	scale := int32(len(fracPart)) - int32(exp)

	return normalize(unscaled, scale), nil
}

// normalize strips trailing zeros so equal values have equal representations.
func normalize(unscaled *big.Int, scale int32) Dec {
	if unscaled == nil || unscaled.Sign() == 0 {
		return Dec{}
	}

	for scale > 0 {
		q, r := new(big.Int).QuoRem(unscaled, bigTen, new(big.Int))

		if r.Sign() != 0 {
			break
		}

		unscaled = q
		scale--
	}

	return Dec{unscaled: unscaled, scale: scale}
}

// copyInt returns a defensively copied significand.
func (d Dec) copyInt() *big.Int {
	if d.unscaled == nil {
		return big.NewInt(0)
	}

	return new(big.Int).Set(d.unscaled)
}

// Sign returns -1, 0 or +1.
func (d Dec) Sign() int { return d.copyInt().Sign() }

// IsZero reports whether d is zero.
func (d Dec) IsZero() bool { return d.Sign() == 0 }

// IsNegative reports whether d is below zero.
func (d Dec) IsNegative() bool { return d.Sign() < 0 }

// IsInteger reports whether d has no fractional part.
func (d Dec) IsInteger() bool {
	if d.scale <= 0 {
		return true
	}

	_, r := new(big.Int).QuoRem(d.copyInt(), pow10(d.scale), new(big.Int))

	return r.Sign() == 0
}

func pow10(n int32) *big.Int {
	return new(big.Int).Exp(bigTen, big.NewInt(int64(n)), nil)
}

// align returns both significands expressed at the same scale.
func align(a, b Dec) (*big.Int, *big.Int, int32) {
	ai, bi := a.copyInt(), b.copyInt()

	switch {
	case a.scale == b.scale:
		return ai, bi, a.scale
	case a.scale > b.scale:
		// The result needs a's precision, so b is scaled up.
		bi.Mul(bi, pow10(a.scale-b.scale))
		return ai, bi, a.scale
	default:
		ai.Mul(ai, pow10(b.scale-a.scale))
		return ai, bi, b.scale
	}
}

// Cmp compares a and b, returning -1, 0 or +1.
func (a Dec) Cmp(b Dec) int {
	ai, bi, _ := align(a, b)

	return ai.Cmp(bi)
}

// Add returns a+b exactly.
func (a Dec) Add(b Dec) Dec {
	ai, bi, scale := align(a, b)

	return normalize(new(big.Int).Add(ai, bi), scale)
}

// Sub returns a-b exactly.
func (a Dec) Sub(b Dec) Dec {
	ai, bi, scale := align(a, b)

	return normalize(new(big.Int).Sub(ai, bi), scale)
}

// Shift multiplies by 10^n exactly. It is the decimal-point move that
// converts between a coin amount and its integer base units.
func (d Dec) Shift(n int) Dec {
	return normalize(d.copyInt(), d.scale-int32(n))
}

// Neg returns -a.
func (a Dec) Neg() Dec {
	return normalize(new(big.Int).Neg(a.copyInt()), a.scale)
}

// Abs returns |a|.
func (a Dec) Abs() Dec {
	return normalize(new(big.Int).Abs(a.copyInt()), a.scale)
}

// Mul returns a*b exactly.
func (a Dec) Mul(b Dec) Dec {
	return normalize(
		new(big.Int).Mul(a.copyInt(), b.copyInt()),
		a.scale+b.scale,
	)
}

// Quo returns a/b rounded to the given number of fractional digits using mode.
//
// A scale of 0 therefore yields an integer, which is how the engine turns an
// exact rate into satoshis.
func (a Dec) Quo(b Dec, scale int32, mode Mode) (Dec, error) {
	if b.IsZero() {
		return Dec{}, fmt.Errorf("decimal: division by zero")
	}

	if scale < 0 {
		return Dec{}, fmt.Errorf("decimal: result scale must be >= 0")
	}

	//
	// a = ai x 10^-as, b = bi x 10^-bs, so
	//   a/b at scale S = ai x 10^(bs-as+S) / bi
	//
	// The exponent can be negative on either side, so the power of ten is
	// applied to whichever side needs it. Assuming otherwise is how an
	// exponent-notation value would silently round wrong.
	//
	num := a.copyInt()
	den := b.copyInt()

	exp := int64(b.scale) - int64(a.scale) + int64(scale)

	if exp > 0 {
		num.Mul(num, pow10(int32(exp)))
	} else if exp < 0 {
		den.Mul(den, pow10(int32(-exp)))
	}

	q, r := new(big.Int).QuoRem(num, den, new(big.Int))

	if r.Sign() == 0 {
		return normalize(q, scale), nil
	}

	// The quotient is inexact; apply the requested rounding.
	negative := num.Sign()*den.Sign() < 0

	roundAwayFromZero := func() {
		if negative {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}

	switch mode {
	case Floor:
		if negative {
			roundAwayFromZero()
		}
	case Ceil:
		if !negative {
			roundAwayFromZero()
		}
	case HalfEven:
		// Compare 2*|r| against |den|.
		twice := new(big.Int).Abs(r)
		twice.Lsh(twice, 1)
		absDen := new(big.Int).Abs(den)

		switch twice.Cmp(absDen) {
		case 1:
			roundAwayFromZero()
		case 0:
			// Tie: round to even.
			if q.Bit(0) == 1 {
				roundAwayFromZero()
			}
		}
	default:
		return Dec{}, fmt.Errorf("decimal: unknown rounding mode %d", mode)
	}

	return normalize(q, scale), nil
}

// pow10Dec returns 10^n as a Dec.
func pow10Dec(n int) Dec {
	if n <= 0 {
		return FromInt64(1)
	}

	return Dec{unscaled: pow10(int32(n))}
}

// CeilInt returns the smallest integer >= d.
func (d Dec) CeilInt() *big.Int {
	q, _ := d.Quo(FromInt64(1), 0, Ceil)
	return q.copyInt()
}

// FloorInt returns the largest integer <= d.
func (d Dec) FloorInt() *big.Int {
	q, _ := d.Quo(FromInt64(1), 0, Floor)
	return q.copyInt()
}

// Int64 returns the truncated integer value, reporting overflow.
func (d Dec) Int64() (int64, error) {
	n := d.copyInt()

	if d.scale > 0 {
		n.Quo(n, pow10(d.scale))
	} else if d.scale < 0 {
		n.Mul(n, pow10(-d.scale))
	}

	if !n.IsInt64() {
		return 0, fmt.Errorf("decimal: %s does not fit in int64", d.String())
	}

	return n.Int64(), nil
}

// Unscaled returns the significand and its scale, for hashing and equality.
func (d Dec) Unscaled() (*big.Int, int32) { return d.copyInt(), d.scale }

// String renders the exact value with no exponent and trailing zeros trimmed.
func (d Dec) String() string {
	if d.IsZero() {
		return "0"
	}

	unscaled := d.copyInt()
	neg := unscaled.Sign() < 0

	if neg {
		unscaled.Abs(unscaled)
	}

	digits := unscaled.String()

	var body string

	switch {
	case d.scale <= 0:
		body = digits + strings.Repeat("0", int(-d.scale))
	case int(d.scale) >= len(digits):
		body = "0." + strings.Repeat("0", int(d.scale)-len(digits)) + digits
	default:
		cut := len(digits) - int(d.scale)
		body = digits[:cut] + "." + digits[cut:]
	}

	if neg {
		return "-" + body
	}

	return body
}

// Fixed renders the value rounded to exactly n fractional digits, half-even,
// which is what Python's format(x, ".3f") does.
func (d Dec) Fixed(n int) (string, error) {
	if n < 0 {
		return "", fmt.Errorf("decimal: negative precision")
	}

	scaled := d.Mul(pow10Dec(n))

	rounded, err := scaled.Quo(FromInt64(1), 0, HalfEven)

	if err != nil {
		return "", err
	}

	unscaled := rounded.copyInt()
	neg := unscaled.Sign() < 0

	if neg {
		unscaled.Abs(unscaled)
	}

	digits := unscaled.String()

	if n == 0 {
		if neg {
			return "-" + digits, nil
		}

		return digits, nil
	}

	if len(digits) <= n {
		digits = strings.Repeat("0", n-len(digits)+1) + digits
	}

	cut := len(digits) - n
	body := digits[:cut] + "." + digits[cut:]

	if neg {
		return "-" + body, nil
	}

	return body, nil
}
