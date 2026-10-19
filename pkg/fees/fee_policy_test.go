package fees

import (
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/decimal"
)

// This file mirrors the reference's `tests/fee_policy_tests.py`: the refusal and fallback
// branches. These are the paths that matter when a node answers with something unexpected —
// each one either falls back to the policy floor or fails closed with a named error, and never
// silently picks a fee.

// rate is the policy's sat/vB as a decimal, for comparison.
func rate(t *testing.T, data FeeData, multiplier string) decimal.Dec {
	t.Helper()

	factor := decimal.MustParse(multiplier)

	chosen, err := ChooseRate(data, &factor)

	if err != nil {
		t.Fatalf("ChooseRate: %v", err)
	}

	return chosen
}

// TestFeePolicyRefusesNonPositiveRates: a zero or negative feerate is not a cheap fee, it is
// an absent one, and converting it must refuse rather than produce zero.
func TestFeePolicyRefusesNonPositiveRates(t *testing.T) {
	for _, bad := range []any{0, "0", -1, "-0.00001"} {
		if _, err := CoinPerKvbToSatVb(bad); err == nil {
			t.Errorf("%v was accepted as a feerate", bad)
		}
	}
}

// TestFeePolicyFallsBackWhenTheEstimatorIsUnusable: a node that cannot estimate still has a
// policy floor, and settlement falls back to it rather than aborting.
func TestFeePolicyFallsBackWhenTheEstimatorIsUnusable(t *testing.T) {
	// A malformed estimator value is ignored, not fatal.
	got := rate(t, FeeData{
		Feerate:       "unavailable",
		MempoolMinFee: 0.00001000,
	}, "1.0")

	if got.String() != decimal.MustParse("1").String() {
		t.Fatalf("a malformed estimator produced %s, want the 1 sat/vB floor", got)
	}

	// No usable estimator but a usable floor: the floor.
	got = rate(t, FeeData{Feerate: nil, MempoolMinFee: 0.00001000, MinRelayTxFee: 0.00000500}, "1.0")

	if got.String() != decimal.MustParse("1").String() {
		t.Fatalf("no estimator produced %s, want 1 sat/vB", got)
	}

	// A zero estimate is not a usable rate either.
	got = rate(t, FeeData{Feerate: 0, MempoolMinFee: 0.00001000}, "1.0")

	if got.String() != decimal.MustParse("1").String() {
		t.Fatalf("a zero estimate produced %s, want 1 sat/vB", got)
	}

	// Only minrelaytxfee: it is the floor.
	got = rate(t, FeeData{MinRelayTxFee: 0.00003000}, "1.0")

	if got.String() != decimal.MustParse("3").String() {
		t.Fatalf("minrelaytxfee alone produced %s, want 3 sat/vB", got)
	}
}

// TestFeePolicyFailsClosedWithoutAFloor: with nothing usable at all, the answer is an error
// naming the reason, never a guessed fee.
func TestFeePolicyFailsClosedWithoutAFloor(t *testing.T) {
	for name, data := range map[string]FeeData{
		"empty":             {},
		"only an estimate":  {Feerate: 0},
		"nothing but noise": {MempoolMinFee: nil, MinRelayTxFee: nil},
	} {
		_, err := ChooseRate(data, nil)

		if err == nil {
			t.Errorf("%s: accepted without a usable fee floor", name)

			continue
		}

		if !strings.Contains(err.Error(), "no usable fee estimate") {
			t.Errorf("%s: refusal = %v", name, err)
		}
	}
}

// TestFeePolicyRefusesAMultiplierBelowOne: the multiplier is a margin, so scaling a fee down
// is not what it can express.
func TestFeePolicyRefusesAMultiplierBelowOne(t *testing.T) {
	for _, bad := range []string{"0.99", "0", "-2"} {
		factor := decimal.MustParse(bad)

		if _, err := ChooseRate(FeeData{Feerate: 0.00001000}, &factor); err == nil {
			t.Errorf("a multiplier of %s was accepted", bad)
		}
	}
}

// TestFeePolicyValidatesSizeAndRate: a fee is a product of two positive quantities, and both
// are checked by name.
func TestFeePolicyValidatesSizeAndRate(t *testing.T) {
	for _, vsize := range []int64{0, -1} {
		if _, err := FeeForVsize(int(vsize), decimal.MustParse("1")); err == nil {
			t.Errorf("a vsize of %d was accepted", vsize)
		}
	}

	for _, bad := range []string{"0", "-1"} {
		if _, err := FeeForVsize(131, decimal.MustParse(bad)); err == nil {
			t.Errorf("a feerate of %s was accepted", bad)
		}
	}
}
