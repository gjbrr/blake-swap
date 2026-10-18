package swap

import (
	"context"
	"strings"
	"testing"
)

// engineWithNode builds an engine whose only part that matters here is the node.
func engineWithNode(t *testing.T) *Engine {
	t.Helper()

	return &Engine{BTC: &chainNode{}}
}

// TestBTCTradeMinimumFloorsBelowTheSettlementCost: the minimum is the cost of
// settling, plus one satoshi, so a settlement can always pay.
func TestBTCTradeMinimumFloorsBelowTheSettlementCost(t *testing.T) {
	// The fake node's estimator says 10 sat/vB and its floors are 1 sat/vB, so the
	// rate is 10 and 200 vB costs 2000 sats.
	minimum, err := engineWithNode(t).BTCTradeMinimumSats(context.Background())

	if err != nil {
		t.Fatal(err)
	}

	if minimum != 2001 {
		t.Fatalf("minimum = %d, want 2001 (200 vB at 10 sat/vB, plus one)", minimum)
	}
}

// TestBTCTradeMinimumUsesTheVsizeEstimate: the size of a settlement spend is an
// assumption, so it is configurable, and an unusable value is refused.
func TestBTCTradeMinimumUsesTheVsizeEstimate(t *testing.T) {
	t.Setenv("BLAKE_SWAP_BTC_SETTLEMENT_VSIZE_ESTIMATE", "400")

	minimum, err := engineWithNode(t).BTCTradeMinimumSats(context.Background())

	if err != nil {
		t.Fatal(err)
	}

	if minimum != 4001 {
		t.Fatalf("minimum = %d, want 4001", minimum)
	}

	t.Setenv("BLAKE_SWAP_BTC_SETTLEMENT_VSIZE_ESTIMATE", "0")

	if _, err := engineWithNode(t).BTCTradeMinimumSats(context.Background()); err == nil {
		t.Fatal("a zero vsize estimate was accepted")
	}
}

// TestBTCTradeMinimumHonoursTheRateOverride: a client with its own view of the fee
// market sets the rate, and the minimum follows it.
func TestBTCTradeMinimumHonoursTheRateOverride(t *testing.T) {
	t.Setenv("BLAKE_SWAP_BTC_SPEND_FEE_RATE", "5")

	minimum, err := engineWithNode(t).BTCTradeMinimumSats(context.Background())

	if err != nil {
		t.Fatal(err)
	}

	if minimum != 1001 {
		t.Fatalf("minimum = %d, want 1001 (200 vB at 5 sat/vB, plus one)", minimum)
	}
}

// TestBTCTradeMinimumHonoursTheExactFeeOverride: an exact fee is the settlement cost
// itself, so the minimum is one satoshi above it.
func TestBTCTradeMinimumHonoursTheExactFeeOverride(t *testing.T) {
	t.Setenv("BLAKE_SWAP_BTC_SPEND_FEE_SATS", "1234")

	minimum, err := engineWithNode(t).BTCTradeMinimumSats(context.Background())

	if err != nil {
		t.Fatal(err)
	}

	if minimum != 1235 {
		t.Fatalf("minimum = %d, want 1235", minimum)
	}
}

// TestBTCTradeMinimumAppliesTheMultiplier: the margin over the estimator is a knob,
// and a multiplier below 1.0 is refused rather than quietly paying less.
func TestBTCTradeMinimumAppliesTheMultiplier(t *testing.T) {
	t.Setenv("BLAKE_SWAP_FEE_RATE_MULTIPLIER", "2.0")

	minimum, err := engineWithNode(t).BTCTradeMinimumSats(context.Background())

	if err != nil {
		t.Fatal(err)
	}

	if minimum != 4001 {
		t.Fatalf("minimum = %d, want 4001 (10 sat/vB doubled)", minimum)
	}

	t.Setenv("BLAKE_SWAP_FEE_RATE_MULTIPLIER", "0.5")

	_, err = engineWithNode(t).BTCTradeMinimumSats(context.Background())

	if err == nil || !strings.Contains(err.Error(), "BLAKE_SWAP_FEE_RATE_MULTIPLIER") {
		t.Fatalf("err = %v, want a refusal naming the multiplier", err)
	}
}

// TestBTCTradeMinimumNeedsANode: with no node and no override there is nothing to
// derive a floor from, and inventing one would advertise an unpayable trade.
func TestBTCTradeMinimumNeedsANode(t *testing.T) {
	_, err := (&Engine{}).BTCTradeMinimumSats(context.Background())

	if err == nil {
		t.Fatal("a minimum was derived without a node")
	}
}
