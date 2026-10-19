package main

import (
	"context"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/decimal"
	"github.com/gjbrr/blake-swap/pkg/reconcile"
	"github.com/gjbrr/blake-swap/pkg/state"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// driverReport is a report whose next action is safe, as a base for mutation.
func driverReport() reconcile.Report {
	ready := true

	return reconcile.Report{
		SwapID:          "e333364be8ee",
		ProtocolRole:    reconcile.Alice,
		CheckpointState: state.BTCFunded,
		DecisionClass:   reconcile.ClassActionable,
		NextAction:      reconcile.ActionClaimBTC,
		Reason:          "Bitcoin HTLC is funded and the BLAKE HTLC is funded",
		Blake:           reconcile.Observation{Status: reconcile.Funded},
		BTC:             reconcile.Observation{Status: reconcile.Funded},
		Secret: reconcile.Secret{
			Known:       true,
			Source:      "locally-generated",
			HashMatches: boolPtr(true),
		},
		Timeouts:  reconcile.Timeouts{BTCBlocksRemaining: 40},
		Readiness: reconcile.Readiness{Checked: true, Ready: &ready},
	}
}

func boolPtr(value bool) *bool { return &value }

// TestRenderDriverHeaderMatchesTheReference pins the driver's opening block.
func TestRenderDriverHeaderMatchesTheReference(t *testing.T) {
	want := "\nBLAKE SWAP AUTONOMOUS DRIVER\n" +
		"============================\n" +
		"swap id: e333364be8ee\n" +
		"\n" +
		"POLICY: recovery, local completion, protocol-Alice Bitcoin claim, and " +
		"protocol-Bob BLAKE claim may execute automatically\n" +
		"POLICY: BLAKE funding and Nostr publication are disabled\n"

	if got := renderDriverHeader("e333364be8ee"); got != want {
		t.Fatalf("renderDriverHeader mismatch:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

// TestRenderDriverDecisionMatchesTheReference pins the per-cycle block.
func TestRenderDriverDecisionMatchesTheReference(t *testing.T) {
	got := renderDriverDecision(2, driverReport())

	want := "\nRECONCILIATION 2\n----------------\n" +
		"\nBLAKE SWAP RUNNER\n" +
		"=================\n" +
		"swap id:          e333364be8ee\n" +
		"protocol role:    alice\n" +
		"checkpoint:       BTC_FUNDED\n" +
		"decision class:   actionable\n" +
		"next action:      claim-btc\n" +
		"\n" +
		"reason:\n" +
		"Bitcoin HTLC is funded and the BLAKE HTLC is funded\n"

	if got != want {
		t.Fatalf("renderDriverDecision mismatch:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

// TestRenderEconomicActionRequiredRefusesToSpend: funding BLAKE is the one action a runner
// must never take, and the block has to say so before it says anything else.
func TestRenderEconomicActionRequiredRefusesToSpend(t *testing.T) {
	got := renderEconomicActionRequired(reconcile.ActionFundBlake)

	want := "\nACTION REQUIRED\n" +
		"===============\n" +
		"fund-blake is currently recommended by the reconciler\n" +
		"\n" +
		"Automatic economic actions are disabled in this runner version.\n" +
		"PASS: no funds were moved\n"

	if got != want {
		t.Fatalf("renderEconomicActionRequired mismatch:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

// TestRenderDriverFailureNamesTheStep: a failed step must say which step, and that nothing
// further was attempted.
func TestRenderDriverFailureNamesTheStep(t *testing.T) {
	got := renderDriverFailure(3, 6, errTest("the node is unreachable"))

	for _, want := range []string{
		"\nRUN RESULT: step failed\n",
		"step:  3 of 6\n",
		"error: the node is unreachable\n",
		"PASS: no further automatic action was attempted\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output is missing %q:\n%s", want, got)
		}
	}
}

// TestRenderDriverBudgetExhaustedMatchesTheReference pins the end of a spent budget.
func TestRenderDriverBudgetExhaustedMatchesTheReference(t *testing.T) {
	want := "\nRUN RESULT\n" +
		"==========\n" +
		"maximum reconciliation steps reached\n" +
		"Step budget exhausted normally; reconcile again before deciding whether " +
		"another action is appropriate.\n"

	if got := renderDriverBudgetExhausted(); got != want {
		t.Fatalf("renderDriverBudgetExhausted mismatch:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

// TestDriverStopConditions: the four ways a cycle ends without acting.
func TestDriverStopConditions(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*reconcile.Report)
		want   string
	}{
		{"settled", func(r *reconcile.Report) {
			r.DecisionClass = reconcile.ClassSettled
			r.NextAction = reconcile.ActionNone
		}, "RUN RESULT: settled; no action required"},
		{"waiting", func(r *reconcile.Report) {
			r.DecisionClass = reconcile.ClassWaiting
			r.NextAction = reconcile.ActionWait
		}, "RUN RESULT: waiting for counterparty or chain condition"},
		{"manual", func(r *reconcile.Report) {
			r.DecisionClass = reconcile.ClassManualAttention
			r.NextAction = reconcile.ActionManualAttention
		}, "PASS: runner refused to guess or mutate state"},
		{"economic", func(r *reconcile.Report) {
			r.NextAction = reconcile.ActionFundBlake
		}, "Automatic economic actions are disabled"},
		{"not allowlisted", func(r *reconcile.Report) {
			r.NextAction = reconcile.Action("publish-offer")
		}, "refusing unrecognized or non-allowlisted action: 'publish-offer'"},
		{"unknown role", func(r *reconcile.Report) {
			r.ProtocolRole = reconcile.Role("both")
		}, "protocol role is not securely known"},
	}

	for _, test := range cases {
		report := driverReport()
		test.mutate(&report)

		got, err := driverStep(context.Background(), &swap.Engine{}, "e333364be8ee", report, 1, 6)

		if err != nil {
			t.Errorf("%s: unexpected error %v", test.name, err)

			continue
		}

		if !strings.Contains(got, test.want) {
			t.Errorf("%s: output = %q, want it to mention %q", test.name, got, test.want)
		}
	}
}

// TestDriverPreconditionClaimBTC: an automatic Bitcoin claim needs the whole fact set, and
// every missing fact is a refusal naming it.
func TestDriverPreconditionClaimBTC(t *testing.T) {
	t.Setenv("BLAKE_SWAP_RUN_MIN_BTC_CLAIM_BLOCKS", "2")

	if got := driverPrecondition(driverReport()); got != "" {
		t.Fatalf("a fully proven claim was refused: %s", got)
	}

	cases := map[string]struct {
		mutate func(*reconcile.Report)
		want   string
	}{
		"not alice": {func(r *reconcile.Report) {
			r.ProtocolRole = reconcile.Bob
		}, "reason: protocol role is not Alice"},
		"blake not funded": {func(r *reconcile.Report) {
			r.Blake.Status = reconcile.Pending
		}, "reason: both HTLCs must be confirmed and unspent"},
		"secret unknown": {func(r *reconcile.Report) {
			r.Secret.Known = false
		}, "secret preconditions are not proven"},
		"secret from peer": {func(r *reconcile.Report) {
			r.Secret.Source = "revealed-by-peer"
		}, "secret preconditions are not proven"},
		"hash mismatch": {func(r *reconcile.Report) {
			r.Secret.HashMatches = boolPtr(false)
		}, "secret preconditions are not proven"},
		"margin too small": {func(r *reconcile.Report) {
			r.Timeouts.BTCBlocksRemaining = 1
		}, "timeout margin is too small (1 blocks remaining; minimum 2)"},
	}

	for name, test := range cases {
		report := driverReport()
		test.mutate(&report)

		if got := driverPrecondition(report); !strings.Contains(got, test.want) {
			t.Errorf("%s: output = %q, want it to mention %q", name, got, test.want)
		}
	}
}

// TestDriverPreconditionFundBTC: funding Bitcoin automatically requires readiness to be
// evaluated and to pass, with its reasons printed when it does not.
func TestDriverPreconditionFundBTC(t *testing.T) {
	report := driverReport()
	report.ProtocolRole = reconcile.Bob
	report.NextAction = reconcile.ActionFundBTC
	report.CheckpointState = state.BlakeFunded
	report.BTC.Status = reconcile.Absent

	if got := driverPrecondition(report); got != "" {
		t.Fatalf("a ready fund-btc was refused: %s", got)
	}

	// Readiness that failed prints every reason it recorded.
	failed := report
	notReady := false
	failed.Readiness.Ready = &notReady
	failed.Readiness.Reasons = []string{"wallet balance 0.4 is below required 0.5"}

	got := driverPrecondition(failed)

	for _, want := range []string{
		"reason: local funding readiness did not pass",
		" - wallet balance 0.4 is below required 0.5",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output is missing %q:\n%s", want, got)
		}
	}

	// And an unevaluated readiness is a refusal, not an assumption.
	unchecked := report
	unchecked.Readiness.Checked = false

	if got := driverPrecondition(unchecked); !strings.Contains(
		got, "funding readiness was not evaluated",
	) {
		t.Fatalf("unevaluated readiness was accepted: %q", got)
	}

	// A BLAKE leg that is not funded cannot be the basis of a BTC funding.
	unfunded := report
	unfunded.Blake.Status = reconcile.Absent

	if got := driverPrecondition(unfunded); !strings.Contains(
		got, "BLAKE HTLC is not confirmed and unspent",
	) {
		t.Fatalf("unfunded BLAKE was accepted: %q", got)
	}

	// Existing Bitcoin funding means the action is stale.
	present := report
	present.BTC.Status = reconcile.Funded

	if got := driverPrecondition(present); !strings.Contains(
		got, "Bitcoin funding is no longer absent",
	) {
		t.Fatalf("already-funded Bitcoin was accepted: %q", got)
	}
}

// TestDriverPreconditionRefunds: a refund is only automatic when the timelock is mature and
// the local checkpoint says this participant should be refunding.
func TestDriverPreconditionRefunds(t *testing.T) {
	btcRefund := driverReport()
	btcRefund.ProtocolRole = reconcile.Bob
	btcRefund.NextAction = reconcile.ActionRefundBTC
	btcRefund.CheckpointState = state.BTCFunded
	btcRefund.Timeouts.BTCBlocksRemaining = 0

	if got := driverPrecondition(btcRefund); got != "" {
		t.Fatalf("a mature refund was refused: %s", got)
	}

	immature := btcRefund
	immature.Timeouts.BTCBlocksRemaining = 1

	if got := driverPrecondition(immature); !strings.Contains(
		got, "Bitcoin refund timelock is not mature",
	) {
		t.Fatalf("an immature refund was accepted: %q", got)
	}

	wrongCheckpoint := btcRefund
	wrongCheckpoint.CheckpointState = state.BlakeFunded

	if got := driverPrecondition(wrongCheckpoint); !strings.Contains(
		got, "local checkpoint is not BTC_FUNDED",
	) {
		t.Fatalf("a wrong checkpoint was accepted: %q", got)
	}

	blakeRefund := driverReport()
	blakeRefund.NextAction = reconcile.ActionRefundBlake
	blakeRefund.CheckpointState = state.BTCRefunded
	blakeRefund.Timeouts.BlakeBlocksRemaining = 0

	if got := driverPrecondition(blakeRefund); got != "" {
		t.Fatalf("a mature BLAKE refund was refused: %s", got)
	}

	// Before Bitcoin has refunded, a BLAKE refund needs Bitcoin funding to be absent.
	preBTC := blakeRefund
	preBTC.CheckpointState = state.BlakeFunded
	preBTC.BTC.Status = reconcile.Pending

	if got := driverPrecondition(preBTC); !strings.Contains(
		got, "pre-BTC refund requires Bitcoin funding to be independently absent",
	) {
		t.Fatalf("a pre-BTC refund with live Bitcoin funding was accepted: %q", got)
	}

	notEligible := blakeRefund
	notEligible.CheckpointState = state.Offered

	if got := driverPrecondition(notEligible); !strings.Contains(
		got, "local checkpoint is not refund-eligible",
	) {
		t.Fatalf("an ineligible checkpoint was accepted: %q", got)
	}
}

// TestDriverPreconditionClaimBlakeRequiresBob: the action that reveals the secret is Bob's
// alone, and a wrong role is refused rather than attempted.
func TestDriverPreconditionClaimBlakeRequiresBob(t *testing.T) {
	report := driverReport()
	report.NextAction = reconcile.ActionClaimBlake
	report.CheckpointState = state.BTCClaimed
	report.ProtocolRole = reconcile.Alice

	if got := driverPrecondition(report); !strings.Contains(got, "protocol role is not Bob") {
		t.Fatalf("Alice was allowed to claim BLAKE: %q", got)
	}

	report.ProtocolRole = reconcile.Bob

	if got := driverPrecondition(report); got != "" {
		t.Fatalf("Bob was refused: %s", got)
	}
}

// TestMinBTCClaimBlocksReadsTheEnvironment: the default is the reference's, and a bad value
// is refused rather than silently replaced.
func TestMinBTCClaimBlocksReadsTheEnvironment(t *testing.T) {
	t.Setenv("BLAKE_SWAP_RUN_MIN_BTC_CLAIM_BLOCKS", "")

	if got, err := minBTCClaimBlocks(); err != nil || got != 2 {
		t.Fatalf("default = %d, %v", got, err)
	}

	t.Setenv("BLAKE_SWAP_RUN_MIN_BTC_CLAIM_BLOCKS", "5")

	if got, err := minBTCClaimBlocks(); err != nil || got != 5 {
		t.Fatalf("configured = %d, %v", got, err)
	}

	for _, bad := range []string{"0", "-1", "many"} {
		t.Setenv("BLAKE_SWAP_RUN_MIN_BTC_CLAIM_BLOCKS", bad)

		if _, err := minBTCClaimBlocks(); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// TestPinFeeRateOutranksTheEnvironment: the client's parameter is what settlement uses, and
// a bad one is refused before anything is built.
func TestPinFeeRateOutranksTheEnvironment(t *testing.T) {
	engine := &swap.Engine{}

	if err := pinFeeRate(engine, "3.5"); err != nil {
		t.Fatal(err)
	}

	if engine.Settlement.FeeRate == nil ||
		engine.Settlement.FeeRate.String() != decimal.MustParse("3.5").String() {
		t.Fatalf("fee rate = %v", engine.Settlement.FeeRate)
	}

	for _, bad := range []string{"0", "-2", "fast"} {
		fresh := &swap.Engine{}

		if err := pinFeeRate(fresh, bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}

		if fresh.Settlement.FeeRate != nil {
			t.Errorf("%q was applied anyway", bad)
		}
	}

	// An absent flag leaves the engine's own configuration alone.
	untouched := &swap.Engine{}

	if err := pinFeeRate(untouched, "  "); err != nil {
		t.Fatal(err)
	}

	if untouched.Settlement.FeeRate != nil {
		t.Fatal("an empty flag changed the engine")
	}
}
