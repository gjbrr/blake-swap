package agent

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gjbrr/blake-swap/pkg/state"
)

// TestPlanAlice: the seller's table, state by state.
//
// The table is data, so the test reads as the table: what runs, in what order, and which
// steps only run if the record is still in the state that justified them.
func TestPlanAlice(t *testing.T) {
	cases := map[state.State][]Step{
		state.Offered: {{Command: FundBlake}},
		state.BlakeFunded: {
			{Command: SyncFunding, PassRole: true},
		},
		state.BTCFunded: {
			{Command: SyncSettlement, PassRole: true},
			{Command: ClaimBTC, OnlyIf: state.BTCFunded},
		},
		state.BTCClaimed: {
			{Command: SyncSettlement, PassRole: true},
		},
		state.BlakeClaimed: {
			{Command: Complete, PassRole: true},
		},
		state.BTCRefunded: {
			{Command: SyncSettlement, PassRole: true},
		},
	}

	for current, want := range cases {
		got, err := Plan(Alice, current)

		if err != nil {
			t.Fatalf("%s: %v", current, err)
		}

		if !sameSteps(got, want) {
			t.Fatalf("%s: plan = %v, want %v", current, got, want)
		}
	}

	for _, terminal := range []state.State{state.Complete, state.BlakeRefunded} {
		got, err := Plan(Alice, terminal)

		if err != nil || len(got) != 0 {
			t.Fatalf("%s: plan = %v, %v", terminal, got, err)
		}
	}
}

// TestPlanBob: the buyer's table, including the step that only runs before the secret is
// known.
func TestPlanBob(t *testing.T) {
	cases := map[state.State][]Step{
		state.Offered: {{Command: SyncFunding, PassRole: true}},
		state.BlakeFunded: {
			{Command: SyncFunding, PassRole: true},
			{Command: FundBTC, OnlyIf: state.BlakeFunded},
		},
		state.BTCFunded: {
			{Command: SyncSettlement, PassRole: true},
		},
		state.BTCClaimed: {
			{Command: ExtractSecret, OnlyIfSecretMissing: true},
			{Command: SyncSettlement, PassRole: true},
			{Command: ClaimBlake, OnlyIf: state.BTCClaimed},
		},
		state.BlakeClaimed: {
			{Command: Complete, PassRole: true},
		},
		state.BTCRefunded: {
			{Command: SyncSettlement, PassRole: true},
		},
	}

	for current, want := range cases {
		got, err := Plan(Bob, current)

		if err != nil {
			t.Fatalf("%s: %v", current, err)
		}

		if !sameSteps(got, want) {
			t.Fatalf("%s: plan = %v, want %v", current, got, want)
		}
	}
}

// TestPlanRefusesUnknownStates: a state the agent cannot act from must stop it with a
// message naming the state, not fall through to doing nothing.
func TestPlanRefusesUnknownStates(t *testing.T) {
	alice := state.State("FUNDED_SOMEWHERE")

	_, err := Plan(Alice, alice)

	if err == nil || !strings.Contains(err.Error(), "Alice does not know how to act from state FUNDED_SOMEWHERE") {
		t.Fatalf("Alice: %v", err)
	}

	_, err = Plan(Bob, alice)

	if err == nil || !strings.Contains(err.Error(), "Bob does not know how to act from state FUNDED_SOMEWHERE") {
		t.Fatalf("Bob: %v", err)
	}

	if _, err := Plan(Role("both"), state.Offered); err == nil {
		t.Fatal("an unknown role was accepted")
	}
}

// TestTerminalStates: the two states that mean the agent is done.
func TestTerminalStates(t *testing.T) {
	for _, terminal := range []state.State{state.Complete, state.BlakeRefunded} {
		if !Terminal(terminal) {
			t.Errorf("%s is not terminal", terminal)
		}
	}

	for _, live := range []state.State{
		state.Offered, state.BlakeFunded, state.BTCFunded, state.BTCClaimed,
		state.BlakeClaimed, state.BTCRefunded, state.SecretRevealed,
	} {
		if Terminal(live) {
			t.Errorf("%s is terminal", live)
		}
	}
}

// TestRunFinishesOnATerminalState: the loop reads the state, sees the end and stops with
// the finished outcome and its exit code.
func TestRunFinishesOnATerminalState(t *testing.T) {
	host := &fakeHost{state: state.Complete}

	var out bytes.Buffer

	outcome, err := Run(context.Background(), host, Bob, fastPolicy(), &out)

	if err != nil {
		t.Fatal(err)
	}

	if !outcome.Finished || outcome.ExitCode() != 0 || outcome.Ticks != 1 {
		t.Fatalf("outcome = %+v", outcome)
	}

	if !strings.Contains(out.String(), "AUTONOMOUS BOB STARTED") {
		t.Fatalf("no banner:\n%s", out.String())
	}

	if !strings.Contains(out.String(), "AUTONOMOUS BOB FINISHED state=COMPLETE") {
		t.Fatalf("no finish line:\n%s", out.String())
	}
}

// TestRunSkipsConditionalStepsWhoseStateMoved: the second command in a tick runs only if the
// record is still where the first command left it expected to be.
func TestRunSkipsConditionalStepsWhoseStateMoved(t *testing.T) {
	host := &fakeHost{state: state.BTCFunded}

	// The reconciling sync moves the record on, so the claim must not run in the same tick.
	host.onExecute = func(step Step) (state.State, error) {
		return state.BTCClaimed, nil
	}

	var out bytes.Buffer

	outcome, err := Run(context.Background(), host, Alice, fastPolicyWithTicks(1), &out)

	if err != nil {
		t.Fatal(err)
	}

	if outcome.Ticks != 1 {
		t.Fatalf("ticks = %d", outcome.Ticks)
	}

	for _, step := range host.ran {
		if step.Command == ClaimBTC {
			t.Fatalf("claimed in the same tick as the reconciling sync: %v", host.ran)
		}
	}

	if len(host.ran) != 1 || host.ran[0].Command != SyncSettlement {
		t.Fatalf("ran = %v", host.ran)
	}
}

// TestRunExtractsTheSecretOnlyOnce: the extraction step is skipped as soon as the record
// holds the secret, which is the whole reason the condition exists.
func TestRunExtractsTheSecretOnlyOnce(t *testing.T) {
	host := &fakeHost{state: state.BTCClaimed, revealed: true}

	var out bytes.Buffer

	if _, err := Run(context.Background(), host, Bob, fastPolicyWithTicks(1), &out); err != nil {
		t.Fatal(err)
	}

	for _, step := range host.ran {
		if step.Command == ExtractSecret {
			t.Fatalf("extracted a secret that was already known: %v", host.ran)
		}
	}

	if len(host.ran) != 2 {
		t.Fatalf("ran = %v", host.ran)
	}
}

// TestRunStopsOnASecurityFailure: a broken invariant ends the loop immediately, with the
// error surfaced rather than counted against the budget.
func TestRunStopsOnASecurityFailure(t *testing.T) {
	host := &fakeHost{state: state.Offered}

	host.onExecute = func(Step) (state.State, error) {
		return state.Offered, errors.New("SECURITY FAILURE: the secret hash does not match")
	}

	var out bytes.Buffer

	outcome, err := Run(context.Background(), host, Alice, fastPolicy(), &out)

	if err == nil || !strings.Contains(err.Error(), "SECURITY FAILURE") {
		t.Fatalf("err = %v", err)
	}

	if outcome.Ticks != 1 {
		t.Fatalf("continued after a security failure: %+v", outcome)
	}
}

// TestRunGivesUpAfterTheFailureBudget: an agent that keeps failing must stop rather than
// spin, and say so.
func TestRunGivesUpAfterTheFailureBudget(t *testing.T) {
	host := &fakeHost{state: state.Offered}

	host.onExecute = func(Step) (state.State, error) {
		return state.Offered, errors.New("the node is unreachable")
	}

	policy := fastPolicy()
	policy.MaxFailures = 3

	var out bytes.Buffer

	outcome, err := Run(context.Background(), host, Alice, policy, &out)

	if err != nil {
		t.Fatal(err)
	}

	if !outcome.Failures || outcome.ExitCode() != 3 {
		t.Fatalf("outcome = %+v", outcome)
	}

	// The budget is the number of failures that stops it, not one more.
	if outcome.Ticks != 3 {
		t.Fatalf("ticks = %d, want exactly the budget", outcome.Ticks)
	}

	if !strings.Contains(out.String(), "too many consecutive failures") {
		t.Fatalf("no give-up line:\n%s", out.String())
	}
}

// TestRunResetsTheFailureBudgetAfterASuccess: the budget is consecutive failures, so one
// success must reset it.
func TestRunResetsTheFailureBudgetAfterASuccess(t *testing.T) {
	host := &fakeHost{state: state.Offered}

	calls := 0

	host.onExecute = func(Step) (state.State, error) {
		calls++

		if calls%2 == 1 {
			return state.Offered, errors.New("transient")
		}

		return state.Offered, nil
	}

	policy := fastPolicy()
	policy.MaxFailures = 2
	policy.MaxTicks = 8

	var out bytes.Buffer

	outcome, err := Run(context.Background(), host, Bob, policy, &out)

	if err != nil {
		t.Fatal(err)
	}

	if outcome.Failures || !outcome.TicksExhausted {
		t.Fatalf("the budget did not reset: %+v", outcome)
	}
}

// TestRunHonoursTheTickBudget: tests need a finite loop, and the budget must stop it without
// claiming the swap finished.
func TestRunHonoursTheTickBudget(t *testing.T) {
	host := &fakeHost{state: state.BlakeFunded}

	var out bytes.Buffer

	outcome, err := Run(context.Background(), host, Bob, fastPolicyWithTicks(2), &out)

	if err != nil {
		t.Fatal(err)
	}

	if outcome.Ticks != 2 || !outcome.TicksExhausted || outcome.Finished {
		t.Fatalf("outcome = %+v", outcome)
	}
}

// fastPolicy is the loop's shape with no sleeping and a small budget.
func fastPolicy() Policy {
	policy := DefaultPolicy()
	policy.Sleep = func(time.Duration) {}

	return policy
}

// fastPolicyWithTicks bounds the loop, for tests that would otherwise run forever.
func fastPolicyWithTicks(ticks int) Policy {
	policy := fastPolicy()
	policy.MaxTicks = ticks

	return policy
}

// sameSteps compares two step lists.
func sameSteps(left, right []Step) bool {
	if len(left) != len(right) {
		return false
	}

	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}

	return true
}

// fakeHost is a participant whose state the test controls.
type fakeHost struct {
	state     state.State
	revealed  bool
	ran       []Step
	onExecute func(Step) (state.State, error)
}

func (f *fakeHost) State(context.Context) (state.State, error) { return f.state, nil }

func (f *fakeHost) SecretRevealed(context.Context) (bool, error) { return f.revealed, nil }

func (f *fakeHost) Execute(_ context.Context, step Step) error {
	f.ran = append(f.ran, step)

	if f.onExecute == nil {
		return nil
	}

	next, err := f.onExecute(step)
	f.state = next

	return err
}
