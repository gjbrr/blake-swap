package observe

import (
	"context"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/chain"
)

// This file mirrors the reference's `tests/swap_bounded_scan_tests.py`: a funding scan is a
// recovery tool, not a licence to walk the whole chain.
//
// The property is operational as much as it is security: an unbounded historical scan on a
// public node is a denial of service against the node and against the operator's patience, and
// it happens exactly when a record is inconsistent — the moment a client is most tempted to
// try harder.

// TestSwapBoundedScanRefusesWithoutAnAnchor: a record with no anchor has no bounded scan, so
// the answer is a refusal naming the override rather than a walk from genesis.
func TestSwapBoundedScanRefusesWithoutAnAnchor(t *testing.T) {
	node, swap, _ := fundedSwap(t, chain.BTC)
	swap.BTCFundingTxID = nil
	swap.BTCFundingVout = nil
	swap.BTCScanStartHeight = nil

	got := observeFake(t, node, swap, chain.BTC)

	if got.Status != Unknown || !strings.Contains(got.Reason, "no recorded Bitcoin funding scan anchor") {
		t.Fatalf("got %s: %s", got.Status, got.Reason)
	}

	// The override is the documented way out, and it is opt-in.
	observation, err := Observe(context.Background(), node, swap, chain.BTC, Config{
		AllowUnanchoredScan: true,
	})

	if err != nil {
		t.Fatal(err)
	}

	if observation.Status != Funded {
		t.Fatalf("with the override: %s: %s", observation.Status, observation.Reason)
	}
}

// TestSwapBoundedScanRefusesAScanWiderThanTheLimit: history older than the limit is refused,
// and the refusal names the limit rather than the symptom.
func TestSwapBoundedScanRefusesAScanWiderThanTheLimit(t *testing.T) {
	node, swap, _ := fundedSwap(t, chain.BTC)
	swap.BTCFundingTxID = nil
	swap.BTCFundingVout = nil

	// An anchor far enough back that the scan would cover more than the limit.
	anchor := int64(1)
	swap.BTCScanStartHeight = &anchor

	// The scan is what is bounded, so it is called directly: `Observe` may satisfy the
	// question from a wallet that already holds the transaction, which is a different path
	// and not the one the limit exists for.
	_, err := ScanFunding(context.Background(), node, swap, chain.BTC, Config{
		MaxScanBlocks: 10,
	})

	if err == nil {
		t.Fatal("a scan wider than the limit was performed")
	}

	for _, want := range []string{"above the", "block limit", "BLAKE_SWAP_MAX_FUNDING_SCAN_BLOCKS"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal is missing %q: %v", want, err)
		}
	}
}

// TestSwapBoundedScanStartsAtTheAnchor: the recorded anchor is where the scan starts, which is
// what keeps old chain history from making the scan more expensive.
func TestSwapBoundedScanStartsAtTheAnchor(t *testing.T) {
	node, swap, _ := fundedSwap(t, chain.BTC)
	swap.BTCFundingTxID = nil
	swap.BTCFundingVout = nil

	anchor := node.height
	swap.BTCScanStartHeight = &anchor

	// At the anchor the funding is still found: the scan is bounded, not blind.
	funding, err := ScanFunding(context.Background(), node, swap, chain.BTC, Config{
		MaxScanBlocks: 10,
	})

	if err != nil {
		t.Fatal(err)
	}

	if funding == nil || funding.TxID == "" {
		t.Fatal("a scan at the anchor found no funding")
	}
}
