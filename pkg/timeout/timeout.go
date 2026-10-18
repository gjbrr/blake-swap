// Package timeout is the CLTV and economic timeout policy.
//
// It is the port of swap_timeout_policy.py. The rule it encodes is the one that
// keeps a swap safe across two chains whose block heights cannot be compared:
// Alice's Bitcoin window is rounded up to whole Bitcoin blocks first, and only
// then is Bob's full recovery budget added on the BLAKE side. The BLAKE
// deadline is therefore built from the *encoded* Bitcoin window, never from the
// ideal duration, so the recovery gap survives integer block rounding.
package timeout

import (
	"fmt"
	"strings"

	"github.com/gjbrr/blake-swap/pkg/decimal"
)

// Policy is the configuration the plan is built from.
type Policy struct {
	// BTCBlockSeconds and BlakeBlockSeconds are the expected block intervals
	// used to convert wall-clock safety budgets into CLTV heights.
	BTCBlockSeconds   int
	BlakeBlockSeconds int

	// BlockDelayMultiplier deliberately does not assume blocks arrive on
	// time. It must be >= 1.
	BlockDelayMultiplier decimal.Dec

	// Operational delays.
	ObservationSeconds int
	RestartSeconds     int
	BroadcastSeconds   int

	// Confirmation requirements used by protocol policy.
	BTCClaimConfirmations   int
	BlakeClaimConfirmations int

	// CrossChainSafetySeconds is the extra time Bob must retain after Alice
	// can still successfully claim Bitcoin.
	CrossChainSafetySeconds int

	// MinimumRecoverySeconds is the absolute minimum permitted recovery gap.
	MinimumRecoverySeconds int
}

// DefaultPolicy is the reference policy.
func DefaultPolicy() Policy {
	return Policy{
		BTCBlockSeconds:         600,
		BlakeBlockSeconds:       600,
		BlockDelayMultiplier:    decimal.MustParse("2.0"),
		ObservationSeconds:      15 * 60,
		RestartSeconds:          15 * 60,
		BroadcastSeconds:        15 * 60,
		BTCClaimConfirmations:   2,
		BlakeClaimConfirmations: 2,
		CrossChainSafetySeconds: 2 * 60 * 60,
		MinimumRecoverySeconds:  4 * 60 * 60,
	}
}

// Plan is the computed timeout plan for one swap.
type Plan struct {
	BTCStartHeight   int64
	BlakeStartHeight int64

	BTCTimeoutHeight   int64
	BlakeTimeoutHeight int64

	BTCWindowSeconds   int64
	BlakeWindowSeconds int64

	BobRecoverySeconds int64

	BTCBlocks   int64
	BlakeBlocks int64
}

// DelayedBlockSeconds is the wall-clock budget for one block, allowing for
// late blocks.
func DelayedBlockSeconds(nominalSeconds int, multiplier decimal.Dec) (int64, error) {
	if nominalSeconds <= 0 {
		return 0, fmt.Errorf("nominal block interval must be positive")
	}

	if multiplier.Sign() <= 0 {
		return 0, fmt.Errorf("block delay multiplier must be positive")
	}

	delayed := decimal.FromInt64(int64(nominalSeconds)).Mul(multiplier)

	return decimal.FromInt(delayed.CeilInt()).Int64()
}

// ConfirmationsBudgetSeconds is the wall-clock budget for n confirmations.
func ConfirmationsBudgetSeconds(confirmations, nominalBlockSeconds int, multiplier decimal.Dec) (int64, error) {
	if confirmations < 1 {
		return 0, fmt.Errorf("confirmations must be at least 1")
	}

	perBlock, err := DelayedBlockSeconds(nominalBlockSeconds, multiplier)

	if err != nil {
		return 0, err
	}

	return int64(confirmations) * perBlock, nil
}

// BobRecoveryBudgetSeconds is the worst-case policy budget after Alice's
// Bitcoin claim becomes relevant to Bob.
//
// Bob may need to observe or restart, obtain the Bitcoin confirmations the
// policy requires, recover S, broadcast his BLAKE claim, obtain BLAKE
// confirmations, and retain an additional cross-chain safety margin.
func BobRecoveryBudgetSeconds(p Policy) (int64, error) {
	btcConfirm, err := ConfirmationsBudgetSeconds(
		p.BTCClaimConfirmations, p.BTCBlockSeconds, p.BlockDelayMultiplier,
	)

	if err != nil {
		return 0, err
	}

	blakeConfirm, err := ConfirmationsBudgetSeconds(
		p.BlakeClaimConfirmations, p.BlakeBlockSeconds, p.BlockDelayMultiplier,
	)

	if err != nil {
		return 0, err
	}

	calculated := int64(p.ObservationSeconds) +
		int64(p.RestartSeconds) +
		btcConfirm +
		int64(p.BroadcastSeconds) +
		blakeConfirm +
		int64(p.CrossChainSafetySeconds)

	if calculated < int64(p.MinimumRecoverySeconds) {
		return int64(p.MinimumRecoverySeconds), nil
	}

	return calculated, nil
}

// AliceBTCWindowSeconds is the budget after Bob funds Bitcoin during which
// Alice can notice the funding and safely get her claim confirmed.
func AliceBTCWindowSeconds(p Policy) (int64, error) {
	confirm, err := ConfirmationsBudgetSeconds(
		p.BTCClaimConfirmations, p.BTCBlockSeconds, p.BlockDelayMultiplier,
	)

	if err != nil {
		return 0, err
	}

	return int64(p.ObservationSeconds) +
		int64(p.RestartSeconds) +
		int64(p.BroadcastSeconds) +
		confirm +
		int64(p.CrossChainSafetySeconds), nil
}

// SecondsToBlocks converts a wall-clock budget to whole blocks, rounding up and
// never returning zero.
func SecondsToBlocks(seconds int64, nominalBlockSeconds int) (int64, error) {
	if seconds <= 0 {
		return 0, fmt.Errorf("seconds must be positive")
	}

	if nominalBlockSeconds <= 0 {
		return 0, fmt.Errorf("nominal block interval must be positive")
	}

	blocks, err := decimal.FromInt64(seconds).
		Quo(decimal.FromInt64(int64(nominalBlockSeconds)), 0, decimal.Ceil)

	if err != nil {
		return 0, err
	}

	n, err := blocks.Int64()

	if err != nil {
		return 0, err
	}

	if n < 1 {
		return 1, nil
	}

	return n, nil
}

// BuildPlan computes the timeout plan for a swap starting at the given heights.
func BuildPlan(btcHeight, blakeHeight int64, p Policy) (Plan, error) {
	if btcHeight < 0 {
		return Plan{}, fmt.Errorf("Bitcoin height cannot be negative")
	}

	if blakeHeight < 0 {
		return Plan{}, fmt.Errorf("BLAKE height cannot be negative")
	}

	desiredBTCWindow, err := AliceBTCWindowSeconds(p)

	if err != nil {
		return Plan{}, err
	}

	recoveryWindow, err := BobRecoveryBudgetSeconds(p)

	if err != nil {
		return Plan{}, err
	}

	//
	// First encode Alice's Bitcoin safety window into whole Bitcoin blocks.
	// This rounding matters: the BLAKE deadline is built from the encoded
	// window, not the ideal duration.
	//
	btcBlocks, err := SecondsToBlocks(desiredBTCWindow, p.BTCBlockSeconds)

	if err != nil {
		return Plan{}, err
	}

	encodedBTCWindow := btcBlocks * int64(p.BTCBlockSeconds)

	// Only now add Bob's full recovery budget.
	requiredBlakeWindow := encodedBTCWindow + recoveryWindow

	blakeBlocks, err := SecondsToBlocks(requiredBlakeWindow, p.BlakeBlockSeconds)

	if err != nil {
		return Plan{}, err
	}

	encodedBlakeWindow := blakeBlocks * int64(p.BlakeBlockSeconds)

	return Plan{
		BTCStartHeight:     btcHeight,
		BlakeStartHeight:   blakeHeight,
		BTCTimeoutHeight:   btcHeight + btcBlocks,
		BlakeTimeoutHeight: blakeHeight + blakeBlocks,
		BTCWindowSeconds:   encodedBTCWindow,
		BlakeWindowSeconds: encodedBlakeWindow,
		BobRecoverySeconds: recoveryWindow,
		BTCBlocks:          btcBlocks,
		BlakeBlocks:        blakeBlocks,
	}, nil
}

// ValidatePlan validates the economic relationship in time.
//
// Deliberately not checked: blake_timeout_height > btc_timeout_height. Those
// heights belong to different blockchains and have no useful ordering.
func ValidatePlan(plan Plan, p Policy) error {
	if plan.BTCTimeoutHeight <= plan.BTCStartHeight {
		return fmt.Errorf("Bitcoin timeout is already expired or has no future window")
	}

	if plan.BlakeTimeoutHeight <= plan.BlakeStartHeight {
		return fmt.Errorf("BLAKE timeout is already expired or has no future window")
	}

	requiredRecovery, err := BobRecoveryBudgetSeconds(p)

	if err != nil {
		return err
	}

	actualRecovery := plan.BlakeWindowSeconds - plan.BTCWindowSeconds

	if actualRecovery < requiredRecovery {
		return fmt.Errorf(
			"Unsafe timeout plan: Bob's post-Bitcoin recovery window is too short",
		)
	}

	//
	// Defence in depth. This is unreachable while BobRecoveryBudgetSeconds
	// returns at least MinimumRecoverySeconds, but it restates the absolute
	// floor where the plan is accepted, so a future change to the budget
	// helper cannot quietly drop it.
	//
	if actualRecovery < int64(p.MinimumRecoverySeconds) {
		return fmt.Errorf(
			"Unsafe timeout plan: below absolute minimum recovery window",
		)
	}

	return nil
}

// DescribePlan renders the plan the way the CLI prints it.
func DescribePlan(plan Plan) string {
	hours := func(seconds int64) string {
		value, err := decimal.FromInt64(seconds).
			Quo(decimal.FromInt64(3600), 2, decimal.HalfEven)

		if err != nil {
			return "?"
		}

		text, err := value.Fixed(2)

		if err != nil {
			return "?"
		}

		return text
	}

	var b strings.Builder

	fmt.Fprintf(&b, "TIMEOUT SAFETY PLAN\n")
	fmt.Fprintf(&b, "===================\n")
	fmt.Fprintf(&b, "Bitcoin: %d -> %d (%d blocks)\n",
		plan.BTCStartHeight, plan.BTCTimeoutHeight, plan.BTCBlocks)
	fmt.Fprintf(&b, "BLAKE:   %d -> %d (%d blocks)\n",
		plan.BlakeStartHeight, plan.BlakeTimeoutHeight, plan.BlakeBlocks)
	fmt.Fprintf(&b, "\n")
	fmt.Fprintf(&b, "Alice BTC window: %s h\n", hours(plan.BTCWindowSeconds))
	fmt.Fprintf(&b, "Bob recovery gap: %s h\n", hours(plan.BobRecoverySeconds))
	fmt.Fprintf(&b, "BLAKE total window: %s h\n", hours(plan.BlakeWindowSeconds))

	return b.String()
}
