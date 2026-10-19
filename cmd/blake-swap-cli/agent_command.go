package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gjbrr/blake-swap/pkg/agent"
	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/state"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// runAgent is the autonomous participant loop, the port of the reference's separate
// `swap_agent.py` program.
//
// It is a command here rather than a second binary because the loop needs nothing the CLI
// does not already have; what it must keep is the reference's shape — the same step table,
// the same banner and per-tick lines, and the same exit codes, because a supervisor watches
// those numbers rather than the text.
func runAgent(ctx context.Context, engine *swap.Engine, args []string) error {
	flags := flag.NewFlagSet("agent", flag.ContinueOnError)

	roleFlag := flags.String("role", "", "protocol role to drive (alice or bob)")
	interval := flags.Float64("interval", 0.25, "seconds to wait between ticks")
	maxTicks := flags.Int("max-ticks", 0, "stop after this many ticks (0 = unbounded)")

	positional, err := parsePositional(flags, args, 1)

	if err != nil {
		return err
	}

	if len(positional) != 1 {
		return errors.New("agent requires a swap id")
	}

	role, err := agent.ParseRole(*roleFlag)

	if err != nil {
		return err
	}

	if *interval <= 0 {
		return errors.New("--interval must be positive")
	}

	if *maxTicks < 0 {
		return errors.New("--max-ticks must not be negative")
	}

	previous := engine.Role
	engine.Role = string(role)

	defer func() { engine.Role = previous }()

	policy := agent.DefaultPolicy()
	policy.Interval = time.Duration(*interval * float64(time.Second))
	policy.MaxTicks = *maxTicks

	outcome, runErr := agent.Run(
		ctx,
		&engineAgentHost{engine: engine, swapID: positional[0], role: role},
		role,
		policy,
		os.Stdout,
	)

	if runErr != nil {
		if strings.Contains(runErr.Error(), "SECURITY FAILURE") {
			return exitError{code: 2, message: runErr.Error()}
		}

		return runErr
	}

	if outcome.Failures {
		return exitError{code: 3}
	}

	return nil
}

// engineAgentHost is the CLI's side of the agent: the loop's three questions, answered from
// the engine.
type engineAgentHost struct {
	engine *swap.Engine
	swapID string
	role   agent.Role
}

// State reads the current protocol state of the watched swap.
func (h *engineAgentHost) State(ctx context.Context) (state.State, error) {
	record, err := h.engine.Show(ctx, h.swapID)

	if err != nil {
		return "", err
	}

	return record.State, nil
}

// SecretRevealed reports whether the record now holds the revealed secret.
func (h *engineAgentHost) SecretRevealed(ctx context.Context) (bool, error) {
	record, err := h.engine.Show(ctx, h.swapID)

	if err != nil {
		return false, err
	}

	return record.RevealedSecretHex != nil && *record.RevealedSecretHex != "", nil
}

// Execute runs one step through the ordinary command path.
//
// The loop cannot run anything a person could not: each step names a command, and the
// command is the same function the CLI dispatches. The role is applied to the engine before
// the call, which is how the reference passes `--role` to its child process.
func (h *engineAgentHost) Execute(ctx context.Context, step agent.Step) error {
	previous := h.engine.Role
	h.engine.Role = string(h.role)

	defer func() { h.engine.Role = previous }()

	args := []string{h.swapID}

	switch step.Command {
	case agent.FundBlake:
		return runFund(ctx, h.engine, args, chain.Blake)

	case agent.FundBTC:
		return runFund(ctx, h.engine, args, chain.BTC)

	case agent.SyncFunding:
		return runSync(ctx, h.engine, args, h.engine.SyncFunding, "FUNDING")

	case agent.SyncSettlement:
		return runSync(ctx, h.engine, args, h.engine.SyncSettlement, "SETTLEMENT")

	case agent.ClaimBTC:
		return runSettle(ctx, h.engine, args, h.engine.ClaimBTC, "btc-claim")

	case agent.ClaimBlake:
		return runSettle(ctx, h.engine, args, h.engine.ClaimBlake, "blake-claim")

	case agent.ExtractSecret:
		return runExtractSecret(ctx, h.engine, args)

	case agent.Complete:
		return runComplete(ctx, h.engine, args)
	}

	return fmt.Errorf("the agent tried to run an unknown command: %s", step.Command)
}
