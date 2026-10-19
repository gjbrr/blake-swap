package swap

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/chain"
)

// This file mirrors the reference's `tests/swap_dynamic_fee_integration_tests.py`: the numbers
// behind "minimum settlement-feasible amount". A market offer below that floor can be claimed
// but not profitably, so the floor is what a maker's advertisement is checked against — and it
// is derived from the node's own conditions rather than from a constant.

// feeNode is a node whose fee surface is fixed at the reference's values: a 1.19 sat/vB
// estimator answer and a 1 sat/vB policy floor.
type feeNode struct{ Node }

func (feeNode) EstimateSmartFee(context.Context, int64) (*chain.FeeEstimate, error) {
	return &chain.FeeEstimate{Feerate: json.Number("0.00001190"), Blocks: 2}, nil
}

func (feeNode) MempoolInfo(context.Context) (*chain.MempoolInfo, error) {
	return &chain.MempoolInfo{
		MempoolMinFee: json.Number("0.00000100"),
		MinRelayTxFee: json.Number("0.00000100"),
	}, nil
}

// enginePricedAt builds an engine whose BTC node prices settlement at the reference's values.
func enginePricedAt(t *testing.T) *Engine {
	t.Helper()

	engine := NewEngine(nil, nil, nil, feeNode{})

	return engine
}

// clearFeeEnvironment removes every fee knob, so each case starts from the defaults.
func clearFeeEnvironment(t *testing.T) {
	t.Helper()

	for _, name := range []string{
		"BLAKE_SWAP_BTC_SPEND_FEE_SATS",
		"BLAKE_SWAP_BTC_SPEND_FEE_RATE",
		"BLAKE_SWAP_FEE_TARGET_BLOCKS",
		"BLAKE_SWAP_BTC_FUNDING_MIN_FEE_RATE",
		"BLAKE_SWAP_FEE_RATE_MULTIPLIER",
		"BLAKE_SWAP_BTC_SETTLEMENT_VSIZE_ESTIMATE",
	} {
		t.Setenv(name, "")
	}
}

// TestSwapDynamicFeeIntegrationTradeMinimum: 200 vB at 1.19 sat/vB is 238 sats, so the smallest
// trade worth advertising is 239 — one satoshi above the cost of settling it.
func TestSwapDynamicFeeIntegrationTradeMinimum(t *testing.T) {
	clearFeeEnvironment(t)

	engine := enginePricedAt(t)

	minimum, err := engine.BTCTradeMinimumSats(context.Background())

	if err != nil {
		t.Fatal(err)
	}

	if minimum != 239 {
		t.Fatalf("minimum = %d sats, want 239 (200 vB at 1.19 sat/vB plus one)", minimum)
	}
}

// TestSwapDynamicFeeIntegrationExactOverride: an exact fee is a statement about the settlement,
// and the minimum follows it.
func TestSwapDynamicFeeIntegrationExactOverride(t *testing.T) {
	clearFeeEnvironment(t)
	t.Setenv("BLAKE_SWAP_BTC_SPEND_FEE_SATS", "1000")

	minimum, err := enginePricedAt(t).BTCTradeMinimumSats(context.Background())

	if err != nil {
		t.Fatal(err)
	}

	if minimum != 1001 {
		t.Fatalf("minimum = %d sats with a 1000 sat override, want 1001", minimum)
	}
}

// TestSwapDynamicFeeIntegrationMultiplier: a margin over the estimator scales the floor, and a
// client's own rate is its answer rather than a number to scale again.
func TestSwapDynamicFeeIntegrationMultiplier(t *testing.T) {
	clearFeeEnvironment(t)
	t.Setenv("BLAKE_SWAP_FEE_RATE_MULTIPLIER", "1.10")

	engine := enginePricedAt(t)

	// ceil(200 × 1.19 × 1.10) = 262; the minimum is one above it.
	minimum, err := engine.BTCTradeMinimumSats(context.Background())

	if err != nil {
		t.Fatal(err)
	}

	if minimum != 263 {
		t.Fatalf("minimum = %d sats with a 1.10 multiplier, want 263", minimum)
	}

	// The rate override drives the same derivation, and the multiplier above is
	// deliberately not applied to it: an explicit rate is the client's answer, and scaling
	// it would be this engine answering the same question twice.
	t.Setenv("BLAKE_SWAP_BTC_SPEND_FEE_RATE", "10")

	minimum, err = engine.BTCTradeMinimumSats(context.Background())

	if err != nil {
		t.Fatal(err)
	}

	if minimum != 2001 {
		t.Fatalf("minimum = %d sats with a 10 sat/vB rate, want 2001", minimum)
	}
}
