package swap

import (
	"os"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/htlc"
)

// These tests pin the fault-injection contract itself, because everything the
// settlement crash and fault suites prove rests on it: if a name drifts, the
// suite arms nothing, the engine dies nowhere, and the test still passes.
//
// Parity: tests/swap_settlement_crash_matrix_tests.py,
// tests/swap_settlement_fault_matrix_tests.py, tests/swap_settlement_race_tests.py

// exitPanic is how a stubbed exit unwinds to the test, standing in for the
// process ending.
type exitPanic struct{ code int }

// stubExit replaces the process exit so a triggered failpoint can be observed
// instead of ending the test binary.
func stubExit(t *testing.T) *int {
	t.Helper()

	code := 0
	previous := exitProcess

	exitProcess = func(status int) {
		code = status

		panic(exitPanic{status})
	}

	t.Cleanup(func() { exitProcess = previous })

	return &code
}

// catchExit runs one call and reports the exit status it triggered, or zero.
func catchExit(run func()) (code int) {
	defer func() {
		if recovered := recover(); recovered != nil {
			panicCode, ok := recovered.(exitPanic)

			if !ok {
				panic(recovered)
			}

			code = panicCode.code
		}
	}()

	run()

	return 0
}

// operation is one settlement the matrices exercise.
type operation struct {
	chain  chain.Chain
	branch htlc.Branch
	slug   string
}

// operations is the reference's four operations, in its order.
func operations() []operation {
	return []operation{
		{chain.BTC, htlc.Claim, "btc_claim"},
		{chain.Blake, htlc.Claim, "blake_claim"},
		{chain.BTC, htlc.Refund, "btc_refund"},
		{chain.Blake, htlc.Refund, "blake_refund"},
	}
}

// TestFailpointNamesMatchTheReference spells out all twelve boundary names and
// all four crash points exactly as the reference suites spell them.
func TestFailpointNamesMatchTheReference(t *testing.T) {
	for _, op := range operations() {
		want := map[string]string{
			BoundaryCandidateSave: "after_" + op.slug + "_candidate_save_before_broadcast",
			BoundaryBroadcast:     "after_" + op.slug + "_broadcast",
			BoundaryConfirmation:  "after_" + op.slug + "_confirmation_before_state_save",
		}

		for boundary, name := range want {
			if got := FailpointName(op.chain, op.branch, boundary); got != name {
				t.Errorf("failpoint name for %s/%s: got %q, want %q",
					op.slug, boundary, got, name)
			}
		}

		wantCrash := strings.ReplaceAll(op.slug, "_", "-") + "-after-broadcast"

		if got := CrashPointName(op.chain, op.branch); got != wantCrash {
			t.Errorf("crash point for %s: got %q, want %q", op.slug, got, wantCrash)
		}
	}

	// Twelve boundaries and four crash points is the whole surface: a fifth
	// boundary would be a name the reference never arms.
	names := map[string]bool{}

	for _, op := range operations() {
		for _, boundary := range Boundaries() {
			names[FailpointName(op.chain, op.branch, boundary)] = true
		}
	}

	if len(names) != 12 {
		t.Fatalf("the failpoint surface is %d names, want 12", len(names))
	}

	if got := len(Boundaries()); got != 3 {
		t.Fatalf("a settlement has %d boundaries, want 3", got)
	}
}

// TestFailpointRequiresTestMode: an armed name must not fire in a process that is
// not in test mode.
func TestFailpointRequiresTestMode(t *testing.T) {
	code := stubExit(t)

	name := FailpointName(chain.BTC, htlc.Claim, BoundaryCandidateSave)

	t.Setenv(FailpointEnv, name)
	t.Setenv(TestModeEnv, "")

	if got := catchExit(func() { failpoint(name) }); got != 0 {
		t.Fatalf("an unarmed failpoint exited %d", got)
	}

	if *code != 0 {
		t.Fatalf("an unarmed failpoint set the exit status to %d", *code)
	}
}

// TestFailpointFiresOnlyOnItsOwnName: in test mode, one armed name fires and the
// other eleven do not. That is what makes a matrix case specific.
func TestFailpointFiresOnlyOnItsOwnName(t *testing.T) {
	stubExit(t)

	armed := FailpointName(chain.Blake, htlc.Refund, BoundaryConfirmation)

	t.Setenv(TestModeEnv, "1")
	t.Setenv(FailpointEnv, armed)

	for _, op := range operations() {
		for _, boundary := range Boundaries() {
			name := FailpointName(op.chain, op.branch, boundary)

			code := catchExit(func() { failpoint(name) })

			if name == armed {
				if code != FailpointExit {
					t.Fatalf("%s exited %d, want %d", name, code, FailpointExit)
				}

				continue
			}

			if code != 0 {
				t.Fatalf("%s fired when %s was armed (exit %d)", name, armed, code)
			}
		}
	}

	for _, op := range operations() {
		name := CrashPointName(op.chain, op.branch)

		if code := catchExit(func() { failpoint(name) }); code != 0 {
			t.Fatalf("failpoint %s fired for a crash-point name", name)
		}
	}
}

// TestCrashPointFiresAfterBroadcastOnly: the four post-broadcast points exit 86,
// and only when test mode, the exact name and the file transport all agree.
func TestCrashPointFiresAfterBroadcastOnly(t *testing.T) {
	stubExit(t)

	armed := CrashPointName(chain.BTC, htlc.Refund)

	t.Setenv(TestModeEnv, "1")
	t.Setenv(CrashPointEnv, armed)
	t.Setenv(TransportEnv, "file")

	for _, op := range operations() {
		c, branch := op.chain, op.branch
		name := CrashPointName(c, branch)

		matched := c == chain.BTC && branch == htlc.Refund

		code := catchExit(func() {
			if err := crashAfterBroadcast(c, branch); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		})

		if matched {
			if code != CrashPointExit {
				t.Fatalf("%s exited %d, want %d", name, code, CrashPointExit)
			}

			continue
		}

		if code != 0 {
			t.Fatalf("%s fired when %s was armed (exit %d)", name, armed, code)
		}
	}
}

// TestCrashPointReportsItsExitStatus is the one case above that cannot assert the
// status through a helper, because the first match ends the call.
func TestCrashPointReportsItsExitStatus(t *testing.T) {
	stubExit(t)

	t.Setenv(TestModeEnv, "1")
	t.Setenv(CrashPointEnv, CrashPointName(chain.Blake, htlc.Claim))
	t.Setenv(TransportEnv, "file")

	code := catchExit(func() { _ = crashAfterBroadcast(chain.Blake, htlc.Claim) })

	if code != CrashPointExit {
		t.Fatalf("crash point exited %d, want %d", code, CrashPointExit)
	}
}

// TestCrashPointRefusesARelayTransport: the reference raises rather than crashing
// when the transport is not the file spool.
func TestCrashPointRefusesARelayTransport(t *testing.T) {
	stubExit(t)

	t.Setenv(TestModeEnv, "1")
	t.Setenv(CrashPointEnv, CrashPointName(chain.Blake, htlc.Refund))
	t.Setenv(TransportEnv, "nostr")

	err := crashAfterBroadcast(chain.Blake, htlc.Refund)

	if err == nil {
		t.Fatal("a crash point fired on a relay transport")
	}

	if want := "settlement crash injection requires file transport"; err.Error() != want {
		t.Fatalf("refusal is %q, want %q", err, want)
	}
}

// TestFailpointIsInertOutsideTestMode is the guard that keeps a stray variable in
// an operator's shell from killing a live swap.
func TestFailpointIsInertOutsideTestMode(t *testing.T) {
	stubExit(t)

	t.Setenv(FailpointEnv, FailpointName(chain.Blake, htlc.Claim, BoundaryBroadcast))
	t.Setenv(CrashPointEnv, CrashPointName(chain.Blake, htlc.Claim))
	t.Setenv(TestModeEnv, "0")
	t.Setenv(TransportEnv, "file")

	if code := catchExit(func() {
		failpoint(FailpointName(chain.Blake, htlc.Claim, BoundaryBroadcast))
	}); code != 0 {
		t.Fatalf("a failpoint fired with test mode off (exit %d)", code)
	}

	if err := crashAfterBroadcast(chain.Blake, htlc.Claim); err != nil {
		t.Fatalf("a crash point with test mode off: %v", err)
	}

	if os.Getenv(TestModeEnv) != "0" {
		t.Fatal("the test mode variable did not reach the process")
	}
}
