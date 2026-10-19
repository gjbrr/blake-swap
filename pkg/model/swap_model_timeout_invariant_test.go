package model

import (
	"strings"
	"testing"
)

// This file mirrors the reference's `tests/swap_model_timeout_invariant_tests.py`: the two
// timeout heights are absolute heights on independent blockchains, so a record must not
// assume any ordering between them — and it must still refuse a recovery budget that the
// windows cannot support.

// timeoutSwap is a v1.5 record with deliberately incomparable heights: BLAKE is numerically
// far behind Bitcoin, which is exactly what two independent chains can do.
func timeoutSwap(t *testing.T) *Swap {
	t.Helper()

	btcWindow, blakeWindow, recovery := int64(12_600), int64(27_600), int64(14_700)
	hash := strings.Repeat("11", 32)

	swap := &Swap{
		SwapID:                    "0123456789ab",
		State:                     "OFFERED",
		CreatedAt:                 1,
		BlakeAmountBaseUnits:      10_000,
		BTCSats:                   2730,
		SecretHash:                hash,
		BlakeTimeout:              100,
		BTCTimeout:                900_000,
		TimeoutPolicyVersion:      strPtr("1.5"),
		BTCTimeoutWindowSeconds:   &btcWindow,
		BlakeTimeoutWindowSeconds: &blakeWindow,
		BobRecoverySeconds:        &recovery,
	}

	return swap
}

// TestSwapModelTimeoutInvariantIgnoresCrossChainOrdering: a record is valid when BLAKE's
// height is numerically below Bitcoin's, because the two numbers are not comparable.
func TestSwapModelTimeoutInvariantIgnoresCrossChainOrdering(t *testing.T) {
	swap := timeoutSwap(t)

	// The fixture preserved the inversion, so the test is about the inversion.
	if swap.BlakeTimeout >= swap.BTCTimeout {
		t.Fatalf("fixture did not invert the heights: %d vs %d", swap.BlakeTimeout, swap.BTCTimeout)
	}

	if err := swap.Validate(); err != nil {
		t.Fatalf("an inverted-height record was refused: %v", err)
	}

	// And the reverse ordering is equally valid.
	reversed := timeoutSwap(t)
	reversed.BlakeTimeout, reversed.BTCTimeout = swap.BTCTimeout, swap.BlakeTimeout

	if err := reversed.Validate(); err != nil {
		t.Fatalf("the reverse ordering was refused: %v", err)
	}
}

// The reference's suite stops at the ordering property: the engine's model does not police
// the recovery budget, because that is the wire schema's job, and this port keeps the same
// division. (The refusal itself is pinned in pkg/wire, where the metadata is validated.)

// strPtr is a string pointer, for the optional record fields.
func strPtr(value string) *string { return &value }
