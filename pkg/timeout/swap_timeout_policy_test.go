package timeout

import (
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/decimal"
)

// This file mirrors the reference's `tests/swap_timeout_policy_tests.py`: the same
// adversarial cases, one assertion per safety property. The timeout plan is the only place
// where two independent blockchains are compared, so its properties are the ones a mistake
// would turn into a loss rather than a retry.

// TestSwapTimeoutPolicyIndependentHeights: the two chains have no related height, so a plan
// must be safe whichever one is "ahead".
func TestSwapTimeoutPolicyIndependentHeights(t *testing.T) {
	policy := DefaultPolicy()

	plan, err := BuildPlan(900_000, 17, policy)

	if err != nil {
		t.Fatalf("huge Bitcoin height was refused: %v", err)
	}

	if err := ValidatePlan(plan, policy); err != nil {
		t.Fatalf("huge Bitcoin height made an invalid plan: %v", err)
	}

	if plan.BTCTimeoutHeight <= plan.BTCStartHeight {
		t.Fatal("Bitcoin timeout is not in the future")
	}

	if plan.BlakeTimeoutHeight <= plan.BlakeStartHeight {
		t.Fatal("BLAKE timeout is not in the future")
	}

	plan, err = BuildPlan(17, 9_000_000, policy)

	if err != nil {
		t.Fatalf("huge BLAKE height was refused: %v", err)
	}

	if err := ValidatePlan(plan, policy); err != nil {
		t.Fatalf("huge BLAKE height made an invalid plan: %v", err)
	}
}

// TestSwapTimeoutPolicyRecoveryBoundary: the encoded heights must preserve the full recovery
// margin, and one second less must be refused.
func TestSwapTimeoutPolicyRecoveryBoundary(t *testing.T) {
	policy := DefaultPolicy()

	plan, err := BuildPlan(1000, 2000, policy)

	if err != nil {
		t.Fatal(err)
	}

	required, err := BobRecoveryBudgetSeconds(policy)

	if err != nil {
		t.Fatal(err)
	}

	gap := plan.BlakeWindowSeconds - plan.BTCWindowSeconds

	if gap < required {
		t.Fatalf("encoded deadlines lost recovery margin: gap %d, required %d", gap, required)
	}

	if err := ValidatePlan(plan, policy); err != nil {
		t.Fatalf("a valid plan was refused: %v", err)
	}

	unsafe := plan
	unsafe.BlakeWindowSeconds = plan.BTCWindowSeconds + required - 1

	if err := ValidatePlan(unsafe, policy); err == nil {
		t.Fatal("one second below the recovery boundary was accepted")
	}
}

// TestSwapTimeoutPolicyRefusesExpiredDeadlines: a deadline at the current height is already
// expired, not "just in time".
func TestSwapTimeoutPolicyRefusesExpiredDeadlines(t *testing.T) {
	policy := DefaultPolicy()

	plan, err := BuildPlan(1000, 2000, policy)

	if err != nil {
		t.Fatal(err)
	}

	expiredBTC := plan
	expiredBTC.BTCTimeoutHeight = plan.BTCStartHeight

	if err := ValidatePlan(expiredBTC, policy); err == nil {
		t.Fatal("an already-expired Bitcoin deadline was accepted")
	}

	expiredBlake := plan
	expiredBlake.BlakeTimeoutHeight = plan.BlakeStartHeight

	if err := ValidatePlan(expiredBlake, policy); err == nil {
		t.Fatal("an already-expired BLAKE deadline was accepted")
	}
}

// TestSwapTimeoutPolicyDefaultsAndBounds: the default policy builds a usable plan, negative
// heights are refused, and the description names the plan's own heights.
func TestSwapTimeoutPolicyDefaultsAndBounds(t *testing.T) {
	plan, err := BuildPlan(100, 200, DefaultPolicy())

	if err != nil {
		t.Fatal(err)
	}

	if plan.BTCBlocks <= 0 || plan.BlakeBlocks <= 0 {
		t.Fatalf("the default plan has no window: %+v", plan)
	}

	if _, err := BuildPlan(-1, 10, DefaultPolicy()); err == nil {
		t.Fatal("a negative Bitcoin height was accepted")
	}

	if _, err := BuildPlan(10, -1, DefaultPolicy()); err == nil {
		t.Fatal("a negative BLAKE height was accepted")
	}

	described := DescribePlan(plan)

	for _, want := range []string{
		"TIMEOUT SAFETY PLAN",
		itoa(plan.BTCStartHeight),
		itoa(plan.BTCTimeoutHeight),
		itoa(plan.BlakeStartHeight),
		itoa(plan.BlakeTimeoutHeight),
	} {
		if !strings.Contains(described, want) {
			t.Fatalf("describe omitted %q:\n%s", want, described)
		}
	}
}

// TestSwapTimeoutPolicyRefusesMalformedPolicy: an impossible policy is refused rather than
// producing a plan nobody can trust.
func TestSwapTimeoutPolicyRefusesMalformedPolicy(t *testing.T) {
	cases := map[string]Policy{
		"zero Bitcoin block interval": func() Policy {
			p := DefaultPolicy()
			p.BTCBlockSeconds = 0

			return p
		}(),
		"zero BLAKE block interval": func() Policy {
			p := DefaultPolicy()
			p.BlakeBlockSeconds = 0

			return p
		}(),
		"zero delay multiplier": func() Policy {
			p := DefaultPolicy()
			p.BlockDelayMultiplier = decimal.MustParse("0")

			return p
		}(),
		"zero Bitcoin confirmations": func() Policy {
			p := DefaultPolicy()
			p.BTCClaimConfirmations = 0

			return p
		}(),
	}

	for name, policy := range cases {
		if _, err := BuildPlan(1, 1, policy); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// TestSwapTimeoutPolicySlowChainExpandsSafety: a chain that is slower than assumed must
// enlarge the budgets, not shrink the margin.
func TestSwapTimeoutPolicySlowChainExpandsSafety(t *testing.T) {
	normal := DefaultPolicy()

	normalPlan, err := BuildPlan(1000, 1000, normal)

	if err != nil {
		t.Fatal(err)
	}

	slowBTC := normal
	slowBTC.BTCBlockSeconds = 1800

	slowBTCPlan, err := BuildPlan(1000, 1000, slowBTC)

	if err != nil {
		t.Fatal(err)
	}

	if slowBTCPlan.BTCWindowSeconds <= normalPlan.BTCWindowSeconds {
		t.Fatal("slow Bitcoin did not expand the BTC window")
	}

	if slowBTCPlan.BobRecoverySeconds <= normalPlan.BobRecoverySeconds {
		t.Fatal("slow Bitcoin did not expand Bob's recovery budget")
	}

	slowBlake := normal
	slowBlake.BlakeBlockSeconds = 1800

	slowBlakePlan, err := BuildPlan(1000, 1000, slowBlake)

	if err != nil {
		t.Fatal(err)
	}

	if slowBlakePlan.BobRecoverySeconds <= normalPlan.BobRecoverySeconds {
		t.Fatal("slow BLAKE did not expand Bob's recovery budget")
	}
}

// TestSwapTimeoutPolicyMinimumRecoveryFloor: an optimistic chain assumption cannot push the
// recovery budget below the absolute floor.
func TestSwapTimeoutPolicyMinimumRecoveryFloor(t *testing.T) {
	policy := Policy{
		BTCBlockSeconds:         60,
		BlakeBlockSeconds:       60,
		BlockDelayMultiplier:    decimal.MustParse("2.0"),
		ObservationSeconds:      1,
		RestartSeconds:          1,
		BroadcastSeconds:        1,
		BTCClaimConfirmations:   1,
		BlakeClaimConfirmations: 1,
		CrossChainSafetySeconds: 1,
		MinimumRecoverySeconds:  6 * 3600,
	}

	recovery, err := BobRecoveryBudgetSeconds(policy)

	if err != nil {
		t.Fatal(err)
	}

	if recovery != 6*3600 {
		t.Fatalf("recovery = %d, want the 6h floor", recovery)
	}
}

// TestSwapTimeoutPolicyRoundsBlocksUpward: a partial block is a whole block of exposure, so
// 601 seconds at 600 seconds per block is two blocks.
func TestSwapTimeoutPolicyRoundsBlocksUpward(t *testing.T) {
	blocks, err := SecondsToBlocks(601, 600)

	if err != nil {
		t.Fatal(err)
	}

	if blocks != 2 {
		t.Fatalf("601/600 = %d blocks, want 2", blocks)
	}
}

// itoa renders a height, for the description check.
func itoa(value int64) string {
	if value == 0 {
		return "0"
	}

	var digits []byte

	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}

	return string(digits)
}
