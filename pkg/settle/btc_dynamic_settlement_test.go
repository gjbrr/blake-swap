package settle

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/decimal"
)

// This file mirrors the reference's `tests/btc_dynamic_settlement_tests.py`: settlement sizes
// itself from what the node measures, not from a constant. The case it pins is a real one — an
// estimator answer of 1.19 sat/vB over a 131 vB HTLC spend — and then the two caps that bound
// what the answer may become.
//
// The reason to pin the numbers rather than the shape: a fee that is one satoshi off is a
// transaction that does not relay, and a fee that is one percent off is a warning nobody reads.

// dynamicNode is the reference's fake node: it estimates 1.19 sat/vB, floors at 1 sat/vB, and
// measures every transaction at 131 vB.
func dynamicNode() *fakeNode {
	node := newFakeNode(2500)
	node.feeRate = "0.00001190" // 1.19 sat/vB from the estimator
	node.vsizeFor = func(int64) int64 { return 131 }

	return node
}

// TestBTCDynamicSettlementConvergesToTheMeasuredFee: the builder measures, recomputes and
// rebuilds, and settles on the fee the measurement actually justifies.
// Parity: tests/btc_dynamic_settlement_tests.py
func TestBTCDynamicSettlementConvergesToTheMeasuredFee(t *testing.T) {
	node := dynamicNode()

	built := make([]int64, 0, 2)

	plan, err := Build(context.Background(), node, chain.BTC, node.inputSats, func(fee int64) (string, error) {
		built = append(built, fee)

		return fmt.Sprintf("fin:%d", fee), nil
	}, Options{})

	if err != nil {
		t.Fatalf("the settlement did not build: %v", err)
	}

	// 131 vB × 1.19 sat/vB = 155.89 -> 156 sats. The first attempt is a placeholder; the
	// second is the measured answer.
	if plan.FeeSats != 156 {
		t.Fatalf("fee = %d sats, want 156 for 131 vB at 1.19 sat/vB", plan.FeeSats)
	}

	if len(built) < 2 {
		t.Fatalf("the builder finalized once (%v): no measurement took place", built)
	}

	if built[len(built)-1] != 156 {
		t.Fatalf("the final build used %d sats, want the measured 156", built[len(built)-1])
	}

	// 156 of 2,500 is 6.24%, under the default 10% ceiling.
	if plan.FeeSats >= 250 {
		t.Fatalf("the dynamic answer breached the default ceiling: %d", plan.FeeSats)
	}
}

// TestBTCDynamicSettlementHonoursAnExplicitOverride: the exact-satoshis override is a
// statement about this settlement, so it is used as given — and it still cannot breach the
// caps, which is the difference between "what to pay" and "what may be paid".
func TestBTCDynamicSettlementHonoursAnExplicitOverride(t *testing.T) {
	node := dynamicNode()

	exact := int64(200)

	plan, err := Build(context.Background(), node, chain.BTC, node.inputSats, func(fee int64) (string, error) {
		return fmt.Sprintf("fin:%d", fee), nil
	}, Options{FeeSats: &exact})

	if err != nil {
		t.Fatalf("an explicit override was refused: %v", err)
	}

	if plan.FeeSats != 200 {
		t.Fatalf("fee = %d sats, want the override's 200", plan.FeeSats)
	}

	// An override that would pay more than the percentage cap allows is refused, and the
	// refusal says which cap: an operator with 300 sats on a 2,500 sat HTLC is paying 12%.
	tooMuch := int64(300)

	_, err = Build(context.Background(), node, chain.BTC, node.inputSats, func(fee int64) (string, error) {
		return "fin:200", nil
	}, Options{FeeSats: &tooMuch})

	if err == nil || !strings.Contains(err.Error(), "safety cap") {
		t.Fatalf("an excessive override was not refused by the cap: %v", err)
	}

	// The absolute cap is the control a percentage cannot express: 200 sats is under 10%
	// of 2,500, so only this knob can refuse it.
	permitted := int64(250)
	absolute := int64(100)

	_, err = Build(context.Background(), node, chain.BTC, node.inputSats, func(fee int64) (string, error) {
		return "fin:200", nil
	}, Options{FeeSats: &permitted, MaxSats: &absolute})

	if err == nil || !strings.Contains(err.Error(), "100") {
		t.Fatalf("the absolute cap did not bind: %v", err)
	}

	// And a cap that permits the dynamic answer changes nothing else: 131 vB at
	// 1.19 sat/vB is 156 sats either way.
	_, err = Build(context.Background(), node, chain.BTC, node.inputSats, func(fee int64) (string, error) {
		return "fin:200", nil
	}, Options{MaxSats: &permitted})

	if err != nil {
		t.Fatalf("a permitted cap was refused: %v", err)
	}
}

// TestBTCDynamicSettlementRateOverrideOutranksTheEstimate: the client's feerate is what it
// wants to pay, so it wins over the node's estimator — the estimator is the default, not the
// authority.
func TestBTCDynamicSettlementRateOverrideOutranksTheEstimate(t *testing.T) {
	node := dynamicNode()

	rate := decimal.MustParse("2")

	plan, err := Build(context.Background(), node, chain.BTC, node.inputSats, func(fee int64) (string, error) {
		return fmt.Sprintf("fin:%d", fee), nil
	}, Options{FeeRate: &rate, MaxPercent: decimal.MustParse("50")})

	if err != nil {
		t.Fatalf("a rates override was refused: %v", err)
	}

	// 131 vB × 2 sat/vB = 262 sats. The default absolute ceiling does not bind, and the
	// 2,500 sat HTLC is large enough that 262 is 10.5% — so the builder must be given a
	// cap that permits it, which is what makes this a test of the rate and not the cap.
	if plan.FeeSats != 262 {
		t.Fatalf("fee = %d sats, want 262 for 131 vB at 2 sat/vB", plan.FeeSats)
	}
}
