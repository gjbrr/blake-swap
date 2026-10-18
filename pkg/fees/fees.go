// Package fees is the settlement feerate policy: what to pay, and what may be
// paid.
//
// It is the port of fee_policy.py. Two properties are load-bearing and are
// preserved exactly:
//
//   - a malformed node value is ignored rather than fatal, so one bad field
//     cannot abort a settlement that a policy floor could price; and
//   - the cap refuses with the arithmetic an operator needs (the rate the cap
//     permits, and the input size the current rate requires), because "fee
//     exceeds cap" without those numbers is a dead end.
package fees

import (
	"fmt"

	"github.com/gjbrr/blake-swap/pkg/decimal"
)

// The configuration knobs this policy is driven by. They are named here
// because a refusal has to tell an operator which one to change.
const (
	// PercentCapEnv caps the settlement fee as a share of the HTLC input.
	PercentCapEnv = "BLAKE_SWAP_MAX_SETTLEMENT_FEE_PERCENT"
	// AbsoluteCapEnv caps the settlement fee in sat-units.
	AbsoluteCapEnv = "BLAKE_SWAP_MAX_SETTLEMENT_FEE_SATS"
	// RateMultiplierEnv scales the estimator's answer.
	RateMultiplierEnv = "BLAKE_SWAP_FEE_RATE_MULTIPLIER"
)

// DefaultMaxPercent is the default cap, as a percentage of the HTLC input.
var DefaultMaxPercent = decimal.MustParse("10")

// One is the neutral multiplier.
var One = decimal.MustParse("1.0")

// FeeData is the node data the rate is chosen from.
//
// Each field is `any` on purpose: the node reports these as JSON numbers or
// strings, and either may be malformed. A nil field means "the node did not
// report it". Malformed values are ignored exactly as the Python policy
// ignores them.
type FeeData struct {
	// Feerate is estimatesmartfee's feerate, in coin/kvB.
	Feerate any
	// MempoolMinFee is getmempoolinfo's mempoolminfee, in coin/kvB.
	MempoolMinFee any
	// MinRelayTxFee is getmempoolinfo's minrelaytxfee, in coin/kvB.
	MinRelayTxFee any
}

// CoinPerKvbToSatVb converts node RPC feerate units (coin/kvB) to sat/vB.
func CoinPerKvbToSatVb(v any) (decimal.Dec, error) {
	value, err := decimalFrom(v)

	if err != nil {
		return decimal.Dec{}, fmt.Errorf("feerate: %w", err)
	}

	if value.Sign() <= 0 {
		return decimal.Dec{}, fmt.Errorf("feerate must be positive")
	}

	// coin/kvB x 1e8 sat/coin / 1000 vbyte/kvB = sat/vB
	perKvb := value.Shift(8)
	rate, err := perKvb.Quo(decimal.FromInt64(1000), 18, decimal.HalfEven)

	if err != nil {
		return decimal.Dec{}, err
	}

	return rate, nil
}

// decimalFrom accepts the scalar shapes a decoded JSON node reply can take.
func decimalFrom(v any) (decimal.Dec, error) {
	switch value := v.(type) {
	case nil:
		return decimal.Dec{}, fmt.Errorf("value is absent")
	case decimal.Dec:
		return value, nil
	case string:
		return decimal.Parse(value)
	case int:
		return decimal.FromInt64(int64(value)), nil
	case int64:
		return decimal.FromInt64(value), nil
	case uint32:
		return decimal.FromInt64(int64(value)), nil
	case float32:
		return decimal.FromFloat64(float64(value))
	case float64:
		return decimal.FromFloat64(value)
	default:
		return decimal.Dec{}, fmt.Errorf("value %v is not a decimal number", v)
	}
}

// ChooseRate selects a settlement feerate from live node data.
//
// It uses the maximum of the estimator's feerate (when available) and the
// two policy floors, scaled by the multiplier. dustrelayfee is deliberately
// not used as a transaction-fee floor.
//
// A nil multiplier means 1.0. A multiplier below 1.0 is refused: paying less
// than the node's own estimate is a decision that belongs to an explicit
// feerate, not to a scale factor.
func ChooseRate(data FeeData, multiplier *decimal.Dec) (decimal.Dec, error) {
	best := decimal.Dec{}
	found := false

	consider := func(v any) {
		rate, err := CoinPerKvbToSatVb(v)

		if err != nil {
			// Ignore it: a malformed field must not abort the settlement.
			return
		}

		if !found || rate.Cmp(best) > 0 {
			best = rate
			found = true
		}
	}

	consider(data.Feerate)
	consider(data.MempoolMinFee)
	consider(data.MinRelayTxFee)

	if !found {
		return decimal.Dec{}, fmt.Errorf(
			"node returned no usable fee estimate or policy floor",
		)
	}

	scale := One

	if multiplier != nil {
		if multiplier.Cmp(One) < 0 {
			return decimal.Dec{}, fmt.Errorf(
				"fee-rate multiplier must be >= 1.0",
			)
		}

		scale = *multiplier
	}

	return best.Mul(scale), nil
}

// FeeForVsize returns the minimum integer satoshi fee needed for vsize.
func FeeForVsize(vsize int, rate decimal.Dec) (int64, error) {
	if vsize < 1 {
		return 0, fmt.Errorf("vsize must be >= 1")
	}

	if rate.Sign() <= 0 {
		return 0, fmt.Errorf("feerate must be positive")
	}

	required := decimal.FromInt64(int64(vsize)).Mul(rate)
	ceil := decimal.FromInt(required.CeilInt())

	return ceil.Int64()
}

// MinimumInputForRate is the smallest HTLC input that can pay rate for vsize
// under a percentage cap.
//
// The percentage cap is what usually binds, not the estimator, so this is the
// number that decides whether a swap is settleable at all in a given fee
// environment:
//
//	required_fee  = ceil(vsize x rate)
//	cap           = ceil(input x percent / 100)
//	input_minimum = the smallest input whose cap reaches required_fee
//
// The cap rounds up, so the boundary is not exactly rate x vsize x 100 /
// percent: for a 140 vB claim at 2 sat/vB and a 10% cap the smallest viable
// input is 2,791 satoshis, where the rough formula says 2,800.
func MinimumInputForRate(rate decimal.Dec, vsize int, maxPercent decimal.Dec) (int64, error) {
	if vsize < 1 {
		return 0, fmt.Errorf("vsize must be >= 1")
	}

	if rate.Sign() <= 0 {
		return 0, fmt.Errorf("feerate must be positive")
	}

	if maxPercent.Sign() <= 0 || maxPercent.Cmp(decimal.FromInt64(100)) >= 0 {
		return 0, fmt.Errorf(
			"maximum settlement fee percent must be > 0 and < 100",
		)
	}

	required, err := FeeForVsize(vsize, rate)

	if err != nil {
		return 0, err
	}

	// floor((required-1) x 100 / percent) + 1, exactly.
	numerator := decimal.FromInt64(required - 1).Mul(decimal.FromInt64(100))
	floored, err := numerator.Quo(maxPercent, 0, decimal.Floor)

	if err != nil {
		return 0, err
	}

	n, err := floored.Int64()

	if err != nil {
		return 0, err
	}

	return n + 1, nil
}

// Cap bounds what may be paid for a settlement.
type Cap struct {
	// MaxPercent is the cap as a share of the HTLC input, in percent.
	// Zero means DefaultMaxPercent.
	MaxPercent decimal.Dec
	// AbsoluteSats, when non-nil, is an additional flat ceiling.
	AbsoluteSats *int64
	// Vsize, when known, only makes a refusal actionable by naming the rate
	// the cap permits and the input size the current rate would need.
	Vsize int
}

func (c Cap) percent() decimal.Dec {
	if c.MaxPercent.IsZero() {
		return DefaultMaxPercent
	}

	return c.MaxPercent
}

// EnforceCap fails closed rather than silently consuming an excessive share of
// the HTLC input.
//
// The percentage cap is applied first; an optional absolute cap may only make
// the limit stricter. The returned value is the fee that was accepted.
func EnforceCap(feeSats, inputSats int64, c Cap) (int64, error) {
	if feeSats < 1 {
		return 0, fmt.Errorf("fee must be >= 1 sat-unit")
	}

	if inputSats < 1 {
		return 0, fmt.Errorf("input amount must be >= 1 sat-unit")
	}

	percent := c.percent()

	if percent.Sign() <= 0 || percent.Cmp(decimal.FromInt64(100)) >= 0 {
		return 0, fmt.Errorf(
			"maximum settlement fee percent must be > 0 and < 100",
		)
	}

	percentCap, err := decimal.FromInt64(inputSats).
		Mul(percent).
		Quo(decimal.FromInt64(100), 0, decimal.Ceil)

	if err != nil {
		return 0, err
	}

	capSats, err := percentCap.Int64()

	if err != nil {
		return 0, err
	}

	if c.AbsoluteSats != nil {
		if *c.AbsoluteSats < 1 {
			return 0, fmt.Errorf("absolute fee cap must be >= 1 sat-unit")
		}

		if *c.AbsoluteSats < capSats {
			capSats = *c.AbsoluteSats
		}
	}

	if feeSats > capSats {
		detail := fmt.Sprintf(
			"Settlement fee exceeds configured safety cap: fee=%d, cap=%d, input=%d",
			feeSats, capSats, inputSats,
		)

		if c.Vsize > 0 {
			affordable, err := decimal.FromInt64(capSats).
				Quo(decimal.FromInt64(int64(c.Vsize)), 3, decimal.HalfEven)

			if err != nil {
				return 0, err
			}

			affordableText, err := affordable.Fixed(3)

			if err != nil {
				return 0, err
			}

			detail += fmt.Sprintf(
				", vsize=%d vB (the cap permits %s sat/vB)",
				c.Vsize, affordableText,
			)

			implied, err := decimal.FromInt64(feeSats).
				Quo(decimal.FromInt64(int64(c.Vsize)), 18, decimal.HalfEven)

			if err != nil {
				return 0, err
			}

			needed, err := MinimumInputForRate(implied, c.Vsize, percent)

			if err != nil {
				return 0, err
			}

			impliedText, err := implied.Fixed(3)

			if err != nil {
				return 0, err
			}

			detail += fmt.Sprintf(
				"; this %d satoshi HTLC is below the %d satoshi minimum a %d vB spend needs at %s sat/vB",
				inputSats, needed, c.Vsize, impliedText,
			)
		}

		detail += fmt.Sprintf(
			". Raise %s (currently %s), set %s to bound the absolute fee, or wait for a cheaper mempool.",
			PercentCapEnv, percent.String(), AbsoluteCapEnv,
		)

		return 0, fmt.Errorf("%s", detail)
	}

	if feeSats >= inputSats {
		return 0, fmt.Errorf(
			"settlement fee would consume the entire HTLC input",
		)
	}

	return feeSats, nil
}
