package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/decimal"
	"github.com/gjbrr/blake-swap/pkg/reconcile"
	"github.com/gjbrr/blake-swap/pkg/state"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// runDriver is the reference's conservative autonomous driver.
//
// It reconciles, then executes at most one *allowlisted* action per cycle, and it does
// nothing on its own judgment: the reconciler's next action decides, an allowlist bounds
// what may be run without a person, and every irreversible branch re-checks the report
// facts it depends on before it runs. Funding BLAKE is not on the allowlist, because that
// is the one action that commits this participant's money.
func runDriver(ctx context.Context, engine *swap.Engine, args []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)

	maxSteps := flags.Int("max-steps", 6, "maximum reconcile/action cycles")
	feeRate := flags.String("fee-rate", "", "settlement feerate in sat/vB")

	positional, err := parsePositional(flags, args, 1)

	if err != nil {
		return err
	}

	if len(positional) != 1 {
		return errors.New("run requires a swap id")
	}

	if *maxSteps < 1 || *maxSteps > 20 {
		return errors.New("--max-steps must be between 1 and 20")
	}

	if err := pinFeeRate(engine, *feeRate); err != nil {
		return err
	}

	swapID := positional[0]

	fmt.Print(renderDriverHeader(swapID))

	for step := 1; step <= *maxSteps; step++ {
		report, err := engine.Reconcile(ctx, swapID)

		if err != nil {
			fmt.Print(renderDriverFailure(step, *maxSteps, err))

			return err
		}

		fmt.Print(renderDriverDecision(step, *report))

		outcome, err := driverStep(ctx, engine, swapID, *report, step, *maxSteps)

		if err != nil {
			var stop driverStop

			if errors.As(err, &stop) {
				fmt.Print(stop.text)

				return nil
			}

			fmt.Print(renderDriverFailure(step, *maxSteps, err))

			return err
		}

		if outcome != "" {
			fmt.Print(outcome)

			return nil
		}
	}

	fmt.Print(renderDriverBudgetExhausted())

	return nil
}

// driverStop ends the run with a printed result and no error.
type driverStop struct {
	text string
}

func (d driverStop) Error() string { return "driver stopped" }

// driverStep decides and, when allowed, performs one cycle's action.
//
// A non-empty return string ends the run. An error carrying a driverStop ends it too, but
// as a refusal with a reason printed rather than an error path.
func driverStep(
	ctx context.Context,
	engine *swap.Engine,
	swapID string,
	report reconcile.Report,
	step, maxSteps int,
) (string, error) {
	safe := map[reconcile.Action]bool{
		reconcile.ActionSyncFunding:    true,
		reconcile.ActionSyncSettlement: true,
		reconcile.ActionComplete:       true,
		reconcile.ActionClaimBlake:     true,
		reconcile.ActionClaimBTC:       true,
		reconcile.ActionFundBTC:        true,
		reconcile.ActionRefundBlake:    true,
		reconcile.ActionRefundBTC:      true,
	}

	switch report.DecisionClass {
	case reconcile.ClassSettled:
		return "\nRUN RESULT: settled; no action required\n", nil
	case reconcile.ClassWaiting:
		return "\nRUN RESULT: waiting for counterparty or chain condition\n", nil
	case reconcile.ClassManualAttention:
		return "\nRUN RESULT: manual attention required\n" +
			"PASS: runner refused to guess or mutate state\n", nil
	}

	action := report.NextAction

	// Funding BLAKE is the action that commits money. The reference disables it in the
	// runner rather than guarding it, and so does this.
	if action == reconcile.ActionFundBlake {
		return renderEconomicActionRequired(action), nil
	}

	if !safe[action] {
		return fmt.Sprintf(
			"\nRUN RESULT: refusing unrecognized or non-allowlisted action: '%s'\n"+
				"PASS: no automatic action was executed\n", action,
		), nil
	}

	if report.ProtocolRole != reconcile.Alice && report.ProtocolRole != reconcile.Bob {
		return "\nRUN RESULT: protocol role is not securely known; " +
			"refusing automatic action\n", nil
	}

	if reason := driverPrecondition(report); reason != "" {
		return reason, nil
	}

	fmt.Printf("\nAUTOMATIC ACTION\n================\n%s\n\n", action)

	if err := runDriverAction(ctx, engine, swapID, action); err != nil {
		return "", err
	}

	fmt.Print("\nPASS: automatic action completed; reconciling again\n")

	return "", nil
}

// driverPrecondition applies the per-action safety gates.
//
// These are not policy: each one names a fact the reconciler observed, and refuses when
// that fact does not hold. The runner acts on a report it read a moment ago, and between
// that read and the action the world can move, so the action is only taken when the report
// it was chosen from still justifies it.
func driverPrecondition(report reconcile.Report) string {
	refuse := func(reason string) string {
		return "\nRUN RESULT: refusing automatic " + string(report.NextAction) + "\n" +
			"reason: " + reason + "\n"
	}

	switch report.NextAction {
	case reconcile.ActionClaimBTC:
		if report.ProtocolRole != reconcile.Alice {
			return refuse("protocol role is not Alice")
		}

		if report.Blake.Status != reconcile.Funded || report.BTC.Status != reconcile.Funded {
			return refuse("both HTLCs must be confirmed and unspent")
		}

		if !report.Secret.Known || report.Secret.Source != "locally-generated" ||
			report.Secret.HashMatches == nil || !*report.Secret.HashMatches {
			return refuse("protocol Alice secret preconditions are not proven")
		}

		minimum, err := minBTCClaimBlocks()

		if err != nil {
			return refuse(err.Error())
		}

		if report.Timeouts.BTCBlocksRemaining < minimum {
			return refuse(fmt.Sprintf(
				"Bitcoin timeout margin is too small (%d blocks remaining; minimum %d)",
				report.Timeouts.BTCBlocksRemaining, minimum,
			))
		}

	case reconcile.ActionFundBTC:
		if report.ProtocolRole != reconcile.Bob {
			return refuse("protocol role is not Bob")
		}

		if !report.Readiness.Checked {
			return refuse("funding readiness was not evaluated")
		}

		if report.Readiness.Ready == nil || !*report.Readiness.Ready {
			var out strings.Builder

			out.WriteString("\nRUN RESULT: refusing automatic fund-btc\n")
			out.WriteString("reason: local funding readiness did not pass\n")

			for _, item := range report.Readiness.Reasons {
				out.WriteString(" - ")
				out.WriteString(item)
				out.WriteString("\n")
			}

			return out.String()
		}

		if report.Blake.Status != reconcile.Funded {
			return refuse("BLAKE HTLC is not confirmed and unspent")
		}

		if report.BTC.Status != reconcile.Absent {
			return refuse("Bitcoin funding is no longer absent")
		}

	case reconcile.ActionRefundBTC:
		if report.ProtocolRole != reconcile.Bob {
			return refuse("protocol role is not Bob")
		}

		if report.CheckpointState != state.BTCFunded {
			return refuse("local checkpoint is not BTC_FUNDED")
		}

		if report.BTC.Status != reconcile.Funded {
			return refuse("Bitcoin HTLC is not confirmed and unspent")
		}

		if report.Timeouts.BTCBlocksRemaining > 0 {
			return refuse("Bitcoin refund timelock is not mature")
		}

	case reconcile.ActionRefundBlake:
		if report.ProtocolRole != reconcile.Alice {
			return refuse("protocol role is not Alice")
		}

		if report.CheckpointState != state.BlakeFunded &&
			report.CheckpointState != state.BTCRefunded {
			return refuse("local checkpoint is not refund-eligible")
		}

		if report.Blake.Status != reconcile.Funded {
			return refuse("BLAKE HTLC is not confirmed and unspent")
		}

		if report.CheckpointState == state.BlakeFunded && report.BTC.Status != reconcile.Absent {
			return refuse("pre-BTC refund requires Bitcoin funding to be independently absent")
		}

		if report.Timeouts.BlakeBlocksRemaining > 0 {
			return refuse("BLAKE refund timelock is not mature")
		}

	case reconcile.ActionClaimBlake:
		if report.ProtocolRole != reconcile.Bob {
			return refuse("protocol role is not Bob")
		}
	}

	return ""
}

// runDriverAction performs one allowlisted action through the ordinary command path.
//
// The commands are the same functions a person runs by hand, so the runner cannot acquire
// privileges a manual invocation lacks — and a bug in the runner cannot become a second
// implementation of settlement.
func runDriverAction(
	ctx context.Context, engine *swap.Engine, swapID string, action reconcile.Action,
) error {
	switch action {
	case reconcile.ActionSyncFunding:
		return runSync(ctx, engine, []string{swapID}, engine.SyncFunding, "FUNDING")

	case reconcile.ActionSyncSettlement:
		return runSync(ctx, engine, []string{swapID}, engine.SyncSettlement, "SETTLEMENT")

	case reconcile.ActionComplete:
		return runComplete(ctx, engine, []string{swapID})

	case reconcile.ActionClaimBlake:
		return runSettle(ctx, engine, []string{swapID}, engine.ClaimBlake, "blake-claim")

	case reconcile.ActionClaimBTC:
		return runSettle(ctx, engine, []string{swapID}, engine.ClaimBTC, "btc-claim")

	case reconcile.ActionFundBTC:
		return runFund(ctx, engine, []string{swapID}, chain.BTC)

	case reconcile.ActionRefundBTC:
		return runSettle(ctx, engine, []string{swapID}, engine.RefundBTC, "btc-refund")

	case reconcile.ActionRefundBlake:
		return runSettle(ctx, engine, []string{swapID}, engine.RefundBlake, "blake-refund")
	}

	return fmt.Errorf("Internal runner allowlist mismatch")
}

// minBTCClaimBlocks is the Bitcoin timeout margin an automatic claim requires.
func minBTCClaimBlocks() (int64, error) {
	raw := os.Getenv("BLAKE_SWAP_RUN_MIN_BTC_CLAIM_BLOCKS")

	if raw == "" {
		return 2, nil
	}

	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)

	if err != nil {
		return 0, fmt.Errorf("BLAKE_SWAP_RUN_MIN_BTC_CLAIM_BLOCKS must be an integer")
	}

	if value < 1 {
		return 0, fmt.Errorf("BLAKE_SWAP_RUN_MIN_BTC_CLAIM_BLOCKS must be >= 1")
	}

	return value, nil
}

// pinFeeRate applies an explicit settlement feerate to the engine for one command.
//
// The parameter outranks the environment, which is the point of offering it: the
// environment is a host default and the client driving the swap is the party that knows
// what it wants to pay. Both fee caps still apply — this says what to pay, not what may be
// paid.
func pinFeeRate(engine *swap.Engine, raw string) error {
	if strings.TrimSpace(raw) == "" || engine == nil {
		return nil
	}

	parsed, err := decimal.Parse(strings.TrimSpace(raw))

	if err != nil {
		return fmt.Errorf("--fee-rate must be a positive decimal: %w", err)
	}

	if parsed.Sign() <= 0 {
		return fmt.Errorf("--fee-rate must be positive")
	}

	engine.Settlement.FeeRate = &parsed

	return nil
}

// renderDriverHeader is the reference's opening block.
func renderDriverHeader(swapID string) string {
	return "\nBLAKE SWAP AUTONOMOUS DRIVER\n" +
		"============================\n" +
		"swap id: " + swapID + "\n" +
		"\n" +
		"POLICY: recovery, local completion, protocol-Alice Bitcoin claim, and " +
		"protocol-Bob BLAKE claim may execute automatically\n" +
		"POLICY: BLAKE funding and Nostr publication are disabled\n"
}

// renderDriverDecision is the per-cycle decision block, the reference's
// _run_print_decision.
func renderDriverDecision(step int, report reconcile.Report) string {
	var out strings.Builder

	fmt.Fprintf(&out, "\nRECONCILIATION %d\n----------------\n", step)
	out.WriteString("\nBLAKE SWAP RUNNER\n")
	out.WriteString("=================\n")
	fmt.Fprintf(&out, "swap id:          %s\n", report.SwapID)
	fmt.Fprintf(&out, "protocol role:    %s\n", report.ProtocolRole)
	fmt.Fprintf(&out, "checkpoint:       %s\n", report.CheckpointState)
	fmt.Fprintf(&out, "decision class:   %s\n", report.DecisionClass)
	fmt.Fprintf(&out, "next action:      %s\n", report.NextAction)
	out.WriteString("\n")
	out.WriteString("reason:\n")
	out.WriteString(report.Reason)
	out.WriteString("\n")

	return out.String()
}

// renderEconomicActionRequired is the refusal to move money automatically.
func renderEconomicActionRequired(action reconcile.Action) string {
	return "\nACTION REQUIRED\n" +
		"===============\n" +
		string(action) + " is currently recommended by the reconciler\n" +
		"\n" +
		"Automatic economic actions are disabled in this runner version.\n" +
		"PASS: no funds were moved\n"
}

// renderDriverFailure contains one step's failure and says which step it was.
//
// The reference prints the exception's type name before the message; this port prints the
// message, which is the part an operator acts on, and the docs record that difference.
func renderDriverFailure(step, maxSteps int, err error) string {
	return "\nRUN RESULT: step failed\n" +
		fmt.Sprintf("step:  %d of %d\n", step, maxSteps) +
		fmt.Sprintf("error: %v\n", err) +
		"PASS: no further automatic action was attempted\n"
}

// renderDriverBudgetExhausted is what a run that used its whole budget prints.
func renderDriverBudgetExhausted() string {
	return "\nRUN RESULT\n" +
		"==========\n" +
		"maximum reconciliation steps reached\n" +
		"Step budget exhausted normally; reconcile again before deciding whether " +
		"another action is appropriate.\n"
}
