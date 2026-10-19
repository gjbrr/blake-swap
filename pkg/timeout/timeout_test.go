package timeout

import (
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/decimal"
)

// Parity: tests/swap_timeout_policy_tests.py
func TestReferencePlan(t *testing.T) {
	p := DefaultPolicy()

	plan, err := BuildPlan(100_000, 500_000, p)

	if err != nil {
		t.Fatal(err)
	}

	// These are the Python reference values, pinned so the port cannot drift.
	want := Plan{
		BTCStartHeight:     100_000,
		BlakeStartHeight:   500_000,
		BTCTimeoutHeight:   100_021,
		BlakeTimeoutHeight: 500_046,
		BTCWindowSeconds:   12_600,
		BlakeWindowSeconds: 27_600,
		BobRecoverySeconds: 14_700,
		BTCBlocks:          21,
		BlakeBlocks:        46,
	}

	if plan != want {
		t.Fatalf("plan =\n%+v\nwant\n%+v", plan, want)
	}

	if err := ValidatePlan(plan, p); err != nil {
		t.Fatalf("the reference plan does not validate: %v", err)
	}
}

func TestBudgetHelpers(t *testing.T) {
	p := DefaultPolicy()

	alice, err := AliceBTCWindowSeconds(p)

	if err != nil {
		t.Fatal(err)
	}

	if alice != 12_300 {
		t.Fatalf("Alice window = %d, want 12300", alice)
	}

	bob, err := BobRecoveryBudgetSeconds(p)

	if err != nil {
		t.Fatal(err)
	}

	if bob != 14_700 {
		t.Fatalf("Bob recovery = %d, want 14700", bob)
	}

	perBlock, err := DelayedBlockSeconds(600, p.BlockDelayMultiplier)

	if err != nil || perBlock != 1_200 {
		t.Fatalf("delayed block = %d, %v", perBlock, err)
	}

	confirmations, err := ConfirmationsBudgetSeconds(2, 600, p.BlockDelayMultiplier)

	if err != nil || confirmations != 2_400 {
		t.Fatalf("confirmations budget = %d, %v", confirmations, err)
	}

	blocks, err := SecondsToBlocks(1, 600)

	if err != nil || blocks != 1 {
		t.Fatalf("seconds_to_blocks(1,600) = %d, %v", blocks, err)
	}
}

func TestTheRecoveryGapSurvivesRounding(t *testing.T) {
	// This is the invariant the whole package exists for: whatever the
	// rounding does, the encoded BLAKE window minus the encoded Bitcoin
	// window must still cover Bob's recovery budget.
	p := DefaultPolicy()

	for _, heights := range [][2]int64{
		{0, 0},
		{1, 1},
		{100_000, 500_000},
		{850_000, 4_200_000},
		{1 << 32, 1 << 32},
	} {
		plan, err := BuildPlan(heights[0], heights[1], p)

		if err != nil {
			t.Fatalf("heights %v: %v", heights, err)
		}

		if err := ValidatePlan(plan, p); err != nil {
			t.Fatalf("heights %v: %v", heights, err)
		}

		gap := plan.BlakeWindowSeconds - plan.BTCWindowSeconds

		if gap < plan.BobRecoverySeconds {
			t.Fatalf("heights %v: gap %d < recovery %d", heights, gap, plan.BobRecoverySeconds)
		}
	}
}

func TestValidationRefusesUnsafePlans(t *testing.T) {
	p := DefaultPolicy()

	good, err := BuildPlan(1_000, 1_000, p)

	if err != nil {
		t.Fatal(err)
	}

	// A timeout at the start height has no future window.
	expired := good
	expired.BTCTimeoutHeight = expired.BTCStartHeight

	if err := ValidatePlan(expired, p); err == nil ||
		!strings.Contains(err.Error(), "no future window") {
		t.Fatalf("expired Bitcoin window: %v", err)
	}

	expiredBlake := good
	expiredBlake.BlakeTimeoutHeight = expiredBlake.BlakeStartHeight

	if err := ValidatePlan(expiredBlake, p); err == nil ||
		!strings.Contains(err.Error(), "no future window") {
		t.Fatalf("expired BLAKE window: %v", err)
	}

	// A BLAKE window that is too close to the Bitcoin window is unsafe.
	short := good
	short.BlakeWindowSeconds = short.BTCWindowSeconds + 60

	if err := ValidatePlan(short, p); err == nil ||
		!strings.Contains(err.Error(), "recovery window is too short") {
		t.Fatalf("short recovery: %v", err)
	}

	// And so is one that is short but still above the budget helper's answer
	// only because the absolute floor was not restated here.
	belowFloor := good
	belowFloor.BlakeWindowSeconds = belowFloor.BTCWindowSeconds + 60

	if err := ValidatePlan(belowFloor, p); err == nil {
		t.Fatal("a below-floor recovery window was accepted")
	}
}

func TestPolicyInputValidation(t *testing.T) {
	if _, err := DelayedBlockSeconds(0, decimal.MustParse("2")); err == nil {
		t.Error("zero nominal interval was accepted")
	}

	if _, err := DelayedBlockSeconds(600, decimal.Zero()); err == nil {
		t.Error("zero multiplier was accepted")
	}

	if _, err := ConfirmationsBudgetSeconds(0, 600, decimal.MustParse("2")); err == nil {
		t.Error("zero confirmations were accepted")
	}

	if _, err := SecondsToBlocks(0, 600); err == nil {
		t.Error("zero seconds were accepted")
	}

	if _, err := SecondsToBlocks(600, 0); err == nil {
		t.Error("zero block interval was accepted")
	}

	if _, err := BuildPlan(-1, 0, DefaultPolicy()); err == nil {
		t.Error("negative Bitcoin height was accepted")
	}

	if _, err := BuildPlan(0, -1, DefaultPolicy()); err == nil {
		t.Error("negative BLAKE height was accepted")
	}
}

func TestDescribePlan(t *testing.T) {
	plan, err := BuildPlan(100_000, 500_000, DefaultPolicy())

	if err != nil {
		t.Fatal(err)
	}

	text := DescribePlan(plan)

	want := []string{
		"TIMEOUT SAFETY PLAN",
		"Bitcoin: 100000 -> 100021 (21 blocks)",
		"BLAKE:   500000 -> 500046 (46 blocks)",
		"Alice BTC window: 3.50 h",
		"Bob recovery gap: 4.08 h",
		"BLAKE total window: 7.67 h",
	}

	for _, line := range want {
		if !strings.Contains(text, line) {
			t.Errorf("description is missing %q:\n%s", line, text)
		}
	}
}
