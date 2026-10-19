package settle

import (
	"context"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/decimal"
)

// This file mirrors the reference's `tests/dynamic_settlement_builder_tests.py`: the fee
// builder's precedence and its convergence.
//
// The order is the whole point, and it is not the order anyone guesses: a parameter outranks
// the environment, and within the environment the exact-satoshis knob outranks the rate. A
// build that picks the wrong one pays the wrong fee, and the operator who set the other knob
// as an emergency never learns it was ignored — which is why the plan carries both the source
// it used and a note about the override it passed over.

// builderNode is a node whose transactions are all `vsize` bytes and whose estimator is
// deliberately terrible, so a test can prove the estimator was not consulted.
func builderNode(vsize int64) *fakeNode {
	node := newFakeNode(20_000)
	node.vsizeFor = func(int64) int64 { return vsize }
	node.feeRate = "0.00001000" // 1 sat/vB: wrong on purpose

	return node
}

// TestDynamicSettlementBuilderRateOverrideConverges: 10 sat/vB on a 100 vB spend is 1000
// sats, and the estimator is not asked: the operator already said what to pay.
func TestDynamicSettlementBuilderRateOverrideConverges(t *testing.T) {
	node := builderNode(100)

	rate := decimal.MustParse("10")

	built := make([]int64, 0, 2)

	plan, err := Build(context.Background(), node, chain.Blake, 20_000, func(fee int64) (string, error) {
		built = append(built, fee)

		return "fin:0", nil
	}, Options{FeeRate: &rate})

	if err != nil {
		t.Fatalf("the spend did not build: %v", err)
	}

	if plan.FeeSats != 1000 {
		t.Fatalf("fee = %d sats, want 1000 for 100 vB at 10 sat/vB", plan.FeeSats)
	}

	if plan.Vsize != 100 {
		t.Fatalf("vsize = %d", plan.Vsize)
	}

	if plan.Source != "--fee-rate" {
		t.Fatalf("source = %q, want --fee-rate", plan.Source)
	}

	// The source is what proves the estimator was passed over: an answer that came from
	// the node would say "dynamic", and 100 vB at 1 sat/vB would have been 100 sats.
	// The builder starts from a placeholder and then uses the measured answer, which is
	// why the first entry is not the fee.
	if len(built) < 2 || built[len(built)-1] != 1000 {
		t.Fatalf("builds = %v, want the last one to be the measured 1000", built)
	}
}

// TestDynamicSettlementBuilderFractionalRate: a rate is a decimal, and 2.5 sat/vB on 100 vB is
// 250 sats — an amount no integer-only knob could express.
func TestDynamicSettlementBuilderFractionalRate(t *testing.T) {
	node := builderNode(100)

	rate := decimal.MustParse("2.5")

	plan, err := Build(context.Background(), node, chain.Blake, 20_000, func(int64) (string, error) {
		return "fin:0", nil
	}, Options{FeeRate: &rate})

	if err != nil {
		t.Fatal(err)
	}

	if plan.FeeSats != 250 {
		t.Fatalf("fee = %d sats, want 250 for 100 vB at 2.5 sat/vB", plan.FeeSats)
	}

	if plan.Source != "--fee-rate" {
		t.Fatalf("source = %q", plan.Source)
	}
}

// TestDynamicSettlementBuilderExactSatsIsTheOverride: the exact-satoshis knob is used as given
// when no parameter outranks it, and the plan says so.
func TestDynamicSettlementBuilderExactSatsIsTheOverride(t *testing.T) {
	node := builderNode(100)

	fee := int64(200)

	plan, err := Build(context.Background(), node, chain.Blake, 20_000, func(int64) (string, error) {
		return "fin:0", nil
	}, Options{FeeSats: &fee})

	if err != nil {
		t.Fatal(err)
	}

	if plan.FeeSats != 200 || plan.Source != "override" {
		t.Fatalf("plan = %+v, want 200 sats from the override", plan)
	}
}

// TestDynamicSettlementBuilderParameterPassesOverTheOverride: when both are present the rate
// wins, and the note says the override was passed over.
//
// A silently ignored emergency override is worse than no override at all.
func TestDynamicSettlementBuilderParameterPassesOverTheOverride(t *testing.T) {
	node := builderNode(100)

	fee := int64(200)
	rate := decimal.MustParse("10")

	plan, err := Build(context.Background(), node, chain.Blake, 20_000, func(int64) (string, error) {
		return "fin:0", nil
	}, Options{FeeRate: &rate, FeeSats: &fee})

	if err != nil {
		t.Fatal(err)
	}

	if plan.FeeSats != 1000 {
		t.Fatalf("fee = %d sats, want the rate's 1000", plan.FeeSats)
	}

	if plan.Note == "" || !strings.Contains(plan.Note, "overridden by --fee-rate") {
		t.Fatalf("the note did not report the passed-over override: %q", plan.Note)
	}
}

// TestDynamicSettlementBuilderWithoutAKnobIsDynamic: with nothing configured, the estimator is
// the source.
func TestDynamicSettlementBuilderWithoutAKnobIsDynamic(t *testing.T) {
	node := builderNode(100)

	plan, err := Build(context.Background(), node, chain.Blake, 20_000, func(int64) (string, error) {
		return "fin:0", nil
	}, Options{})

	if err != nil {
		t.Fatal(err)
	}

	// 100 vB at the node's 1 sat/vB.
	if plan.FeeSats != 100 || plan.Source != "dynamic" {
		t.Fatalf("plan = %+v, want 100 sats from the estimator", plan)
	}
}

// planOf is the plan a build produced, or a zero plan when it refused.
func planOf(t *testing.T, node *fakeNode, rate decimal.Dec) Plan {
	t.Helper()

	plan, err := Build(context.Background(), node, chain.Blake, 20_000, func(int64) (string, error) {
		return "fin:0", nil
	}, Options{FeeRate: &rate})

	if err != nil {
		return Plan{}
	}

	return *plan
}

// TestDynamicSettlementBuilderRefusesAnImpossibleRate: a rate that cannot describe a spend is
// refused by name, and nothing is built from it.
func TestDynamicSettlementBuilderRefusesAnImpossibleRate(t *testing.T) {
	for _, bad := range []string{"0", "-1"} {
		node := builderNode(100)

		rate := decimal.MustParse(bad)

		_, err := Build(context.Background(), node, chain.Blake, 20_000, func(int64) (string, error) {
			return "fin:0", nil
		}, Options{FeeRate: &rate})

		if err == nil {
			t.Errorf("a rate of %s was accepted", bad)
		}

		// Nothing was built from it: the plan's zero value is what a caller sees.
		if plan := planOf(t, node, rate); plan.FeeSats != 0 {
			t.Errorf("a rate of %s produced a %d sat plan", bad, plan.FeeSats)
		}
	}
}
