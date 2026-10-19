// Package agent is the autonomous participant's step policy.
//
// The Python system ships a second, smaller program beside the engine: a loop that asks one
// question per tick — given this participant's role and the state of their record, is there
// a command that should run now? — and sleeps between ticks. It is deliberately dumber than
// the engine: it knows nothing about chains, fees or protocol rules, only which command
// belongs to which state.
//
// This package is that loop, written so the interesting parts can be tested without a node,
// a relay or a filesystem: the table is data, the decisions are pure, and the I/O the loop
// needs is one small interface the caller implements.
package agent

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/gjbrr/blake-swap/pkg/state"
)

// Role is the protocol role the agent is driving.
type Role string

// The two protocol roles.
const (
	Alice Role = "alice"
	Bob   Role = "bob"
)

// ParseRole validates a role name.
func ParseRole(value string) (Role, error) {
	switch Role(value) {
	case Alice:
		return Alice, nil
	case Bob:
		return Bob, nil
	}

	return "", fmt.Errorf("role must be alice or bob")
}

// Command is one command the agent may run on its own.
type Command string

// The commands a step can name. They are the ordinary operator commands, not a private
// vocabulary: the agent runs what a person would run, which is what keeps its behaviour
// auditable.
const (
	FundBlake      Command = "fund-blake"
	FundBTC        Command = "fund-btc"
	SyncFunding    Command = "sync-funding"
	SyncSettlement Command = "sync-settlement"
	ClaimBTC       Command = "claim-btc"
	ClaimBlake     Command = "claim-blake"
	ExtractSecret  Command = "extract-secret"
	Complete       Command = "complete"
)

// Step is one command with the conditions under which it still applies.
type Step struct {
	// Command is what to run.
	Command Command
	// PassRole adds the protocol role to the command, for the commands that take it.
	PassRole bool
	// OnlyIf runs the step only when the record is in this state at the moment the step
	// is reached — after any earlier step in the same tick has run. Empty means always.
	OnlyIf state.State
	// OnlyIfSecretMissing runs the step only while the record holds no revealed secret.
	OnlyIfSecretMissing bool
}

// Policy is the loop's pacing and budgets.
type Policy struct {
	// Interval is how long to sleep between ticks.
	Interval time.Duration
	// MaxFailures stops the loop after this many consecutive failed ticks.
	MaxFailures int
	// MaxTicks bounds the loop. Zero means unbounded, which is how an agent is meant to
	// run; tests set it to make the loop finite.
	MaxTicks int
	// Sleep is the clock. Zero means time.Sleep.
	Sleep func(time.Duration)
}

// DefaultPolicy is the reference agent's pacing: a quarter second between ticks and a
// budget of twenty consecutive failures.
func DefaultPolicy() Policy {
	return Policy{Interval: 250 * time.Millisecond, MaxFailures: 20}
}

// Host is everything the loop needs from a participant.
type Host interface {
	// State reports the current protocol state of the watched swap.
	State(ctx context.Context) (state.State, error)
	// SecretRevealed reports whether the record now holds the revealed secret.
	SecretRevealed(ctx context.Context) (bool, error)
	// Execute runs one command, with the role supplied when the step asks for it.
	Execute(ctx context.Context, step Step) error
}

// Outcome is why the loop stopped.
type Outcome struct {
	// Ticks is how many ticks ran.
	Ticks int
	// Final is the state the loop stopped in, when it could be read.
	Final state.State
	// Finished reports that the swap reached a terminal state.
	Finished bool
	// Failures reports that the failure budget ran out.
	Failures bool
	// TicksExhausted reports that MaxTicks stopped the loop.
	TicksExhausted bool
}

// ExitCode is the process exit code this outcome implies.
//
// The reference agent returns 0 when the swap is finished, 2 when it stops on a security
// failure and 3 when it gives up after too many failures; a supervisor reads those numbers,
// so they are part of the contract.
func (o Outcome) ExitCode() int {
	switch {
	case o.Finished:
		return 0
	case o.Failures:
		return 3
	default:
		return 0
	}
}

// Terminal reports whether this state means the agent has nothing left to do.
func Terminal(current state.State) bool {
	return current == state.Complete || current == state.BlakeRefunded
}

// Plan returns this tick's steps for a role in a state.
//
// The order matters. Where the reference re-reads the record between two commands — to
// decide whether the second one is still needed — the second step names the state it
// requires, and the loop re-reads before running it.
func Plan(role Role, current state.State) ([]Step, error) {
	if Terminal(current) {
		return nil, nil
	}

	if role == Alice {
		return planAlice(current)
	}

	if role == Bob {
		return planBob(current)
	}

	return nil, fmt.Errorf("role must be alice or bob")
}

// planAlice is the seller's table: she funds BLAKE, watches, then claims Bitcoin.
func planAlice(current state.State) ([]Step, error) {
	switch current {
	case state.Offered:
		return []Step{{Command: FundBlake}}, nil
	case state.BlakeFunded:
		return []Step{{Command: SyncFunding, PassRole: true}}, nil
	case state.BTCFunded:
		return []Step{
			{Command: SyncSettlement, PassRole: true},
			{Command: ClaimBTC, OnlyIf: state.BTCFunded},
		}, nil
	case state.BTCClaimed:
		return []Step{{Command: SyncSettlement, PassRole: true}}, nil
	case state.BlakeClaimed:
		return []Step{{Command: Complete, PassRole: true}}, nil
	case state.BTCRefunded:
		return []Step{{Command: SyncSettlement, PassRole: true}}, nil
	}

	return nil, fmt.Errorf("Alice does not know how to act from state %s", current)
}

// planBob is the buyer's table: he watches for BLAKE, funds Bitcoin, then claims BLAKE.
func planBob(current state.State) ([]Step, error) {
	switch current {
	case state.Offered:
		return []Step{{Command: SyncFunding, PassRole: true}}, nil
	case state.BlakeFunded:
		return []Step{
			{Command: SyncFunding, PassRole: true},
			{Command: FundBTC, OnlyIf: state.BlakeFunded},
		}, nil
	case state.BTCFunded:
		return []Step{{Command: SyncSettlement, PassRole: true}}, nil
	case state.BTCClaimed:
		return []Step{
			{Command: ExtractSecret, OnlyIfSecretMissing: true},
			{Command: SyncSettlement, PassRole: true},
			{Command: ClaimBlake, OnlyIf: state.BTCClaimed},
		}, nil
	case state.BlakeClaimed:
		return []Step{{Command: Complete, PassRole: true}}, nil
	case state.BTCRefunded:
		return []Step{{Command: SyncSettlement, PassRole: true}}, nil
	}

	return nil, fmt.Errorf("Bob does not know how to act from state %s", current)
}

// Run drives one swap until it finishes, the budget runs out, or a step fails in a way that
// must stop the agent.
//
// A security failure stops it immediately and reports it: the reference agent treats that
// as the one class of error worth a distinct exit code, because continuing to run commands
// against a swap whose invariants are broken is exactly how a person loses money.
func Run(
	ctx context.Context, host Host, role Role, policy Policy, out io.Writer,
) (Outcome, error) {
	if host == nil {
		return Outcome{}, fmt.Errorf("the agent needs a host")
	}

	if policy.MaxFailures <= 0 {
		policy.MaxFailures = DefaultPolicy().MaxFailures
	}

	if policy.Interval <= 0 {
		policy.Interval = DefaultPolicy().Interval
	}

	sleep := policy.Sleep

	if sleep == nil {
		sleep = time.Sleep
	}

	fmt.Fprintf(out, "AUTONOMOUS %s STARTED\n", upper(string(role)))

	outcome := Outcome{}
	failures := 0

	for {
		if err := ctx.Err(); err != nil {
			return outcome, err
		}

		if policy.MaxTicks > 0 && outcome.Ticks >= policy.MaxTicks {
			outcome.TicksExhausted = true

			return outcome, nil
		}

		current, err := host.State(ctx)

		if err != nil {
			return outcome, err
		}

		outcome.Ticks++
		outcome.Final = current

		fmt.Fprintf(out, "[%s] state=%s\n", role, current)

		if Terminal(current) {
			outcome.Finished = true

			fmt.Fprintf(out, "AUTONOMOUS %s FINISHED state=%s\n", upper(string(role)), current)

			return outcome, nil
		}

		if err := runTick(ctx, host, role, current, out); err != nil {
			failures++

			fmt.Fprintf(out, "[%s] ERROR: %v\n", role, err)

			if isSecurityFailure(err) {
				return outcome, err
			}
		} else {
			failures = 0
		}

		if failures >= policy.MaxFailures {
			outcome.Failures = true

			fmt.Fprintf(out, "[%s] too many consecutive failures\n", role)

			return outcome, nil
		}

		sleep(policy.Interval)
	}
}

// runTick runs one tick's steps, re-reading the record before each conditional one.
//
// The re-read is the point: a step that was right when the tick began can be wrong by the
// time the previous command finished, and the condition exists to catch exactly that.
func runTick(
	ctx context.Context, host Host, role Role, current state.State, out io.Writer,
) error {
	steps, err := Plan(role, current)

	if err != nil {
		return err
	}

	for _, step := range steps {
		if step.OnlyIf != "" || step.OnlyIfSecretMissing {
			latest, err := host.State(ctx)

			if err != nil {
				return err
			}

			if step.OnlyIf != "" && latest != step.OnlyIf {
				continue
			}

			if step.OnlyIfSecretMissing {
				revealed, err := host.SecretRevealed(ctx)

				if err != nil {
					return err
				}

				if revealed {
					continue
				}
			}
		}

		if err := host.Execute(ctx, step); err != nil {
			return err
		}
	}

	return nil
}

// isSecurityFailure reports whether an error must stop the agent outright.
func isSecurityFailure(err error) bool {
	return err != nil && contains(err.Error(), "SECURITY FAILURE")
}

// contains is a small substring test, kept here so the security check reads plainly.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}

	return false
}

// upper uppercases ASCII letters, for the banner lines.
func upper(value string) string {
	out := []byte(value)

	for i, c := range out {
		if c >= 'a' && c <= 'z' {
			out[i] = c - ('a' - 'A')
		}
	}

	return string(out)
}
