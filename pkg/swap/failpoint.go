package swap

import (
	"fmt"
	"os"

	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/htlc"
)

// The reference engine ships two fault-injection mechanisms for its settlement
// crash, fault and race suites. Both are reproduced here with the reference's
// names, exit codes and placement, because a suite that asserts "the process died
// exactly here and the record still recovered" is only meaningful when the death
// really happens exactly there.
//
// A failpoint is not an error path. It does not unwind, log and return: it ends
// the process the way a power cut would, so nothing that would have run after it
// can run. Anything that hid the crash — a sleep, a retry, an injected error the
// caller recovers from — would test a different program.
const (
	// FailpointEnv names one settlement boundary to die at.
	FailpointEnv = "BLAKE_SWAP_FAILPOINT"
	// CrashPointEnv names one settlement to die just after broadcasting.
	CrashPointEnv = "BLAKE_SWAP_TEST_CRASH_POINT"
	// TestModeEnv arms both mechanisms. Without it neither can fire, so a
	// production process cannot be killed by a stray environment variable.
	TestModeEnv = "BLAKE_SWAP_TEST_MODE"
	// TransportEnv selects the coordination transport. The crash point refuses to
	// fire unless it is the file transport, because a broadcast crash is only
	// recoverable evidence from a local spool the next process can read.
	TransportEnv = "BLAKE_SWAP_TRANSPORT"

	// FailpointExit is the exit status a triggered failpoint leaves behind.
	FailpointExit = 97
	// CrashPointExit is the exit status a triggered crash point leaves behind.
	CrashPointExit = 86
)

// The three boundaries each settlement operation passes through.
const (
	// BoundaryCandidateSave fires after the signed candidate is durable and
	// before it is broadcast.
	BoundaryCandidateSave = "candidate_save_before_broadcast"
	// BoundaryBroadcast fires after the node accepted the transaction.
	BoundaryBroadcast = "broadcast"
	// BoundaryConfirmation fires after the protocol state advanced in memory and
	// before it was persisted.
	BoundaryConfirmation = "confirmation_before_state_save"
)

// Boundaries is every boundary, in the order a settlement reaches them.
func Boundaries() []string {
	return []string{BoundaryCandidateSave, BoundaryBroadcast, BoundaryConfirmation}
}

// chainSlug is a chain as the failpoint names spell it.
func chainSlug(ch chain.Chain) string {
	if ch == chain.Blake {
		return "blake"
	}

	return "btc"
}

// FailpointName is the reference's name for one settlement boundary.
//
// The names are a contract: `tests/swap_settlement_crash_matrix_tests.py` and
// `tests/swap_settlement_fault_matrix_tests.py` spell them out, and a suite that
// armed a name the engine no longer honours would silently test nothing.
func FailpointName(ch chain.Chain, branch htlc.Branch, boundary string) string {
	return fmt.Sprintf("after_%s_%s_%s", chainSlug(ch), branch, boundary)
}

// CrashPointName is the reference's name for a post-broadcast crash point.
func CrashPointName(ch chain.Chain, branch htlc.Branch) string {
	return fmt.Sprintf("%s-%s-after-broadcast", chainSlug(ch), branch)
}

// exitProcess ends the process, and is replaceable so the mechanism can be tested
// without the test binary dying.
var exitProcess = os.Exit

// failpoint ends the process at one settlement boundary when it is armed.
func failpoint(name string) {
	if os.Getenv(TestModeEnv) != "1" {
		return
	}

	if os.Getenv(FailpointEnv) != name {
		return
	}

	fmt.Printf("TEST FAILPOINT TRIGGERED: %s\n", name)

	exitProcess(FailpointExit)
}

// crashAfterBroadcast ends the process immediately after a settlement was
// accepted by the node, which is the window where money has moved but nothing
// local has noticed yet.
//
// It refuses to fire on any transport but the file transport: on a relay the
// evidence a restarting process needs is not on this machine, so the crash would
// test the relay's retention rather than this engine's durability.
func crashAfterBroadcast(ch chain.Chain, branch htlc.Branch) error {
	if os.Getenv(TestModeEnv) != "1" {
		return nil
	}

	if os.Getenv(CrashPointEnv) != CrashPointName(ch, branch) {
		return nil
	}

	if transport := os.Getenv(TransportEnv); transport != "" && transport != "file" {
		return fmt.Errorf("settlement crash injection requires file transport")
	}

	exitProcess(CrashPointExit)

	return nil
}
