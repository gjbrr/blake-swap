//go:build !js

package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gjbrr/blake-swap/internal/rigtest"
)

// Parity: tests/swap_protocol_tests.py, tests/command_surface_tests.py
//
// The success output of a command that *changes* something cannot be compared by
// running both implementations on one record: the first one moves it forward. So
// the same scripted walk is run twice, once with each implementation driving, and
// the step-by-step output is compared after the identifiers that legitimately
// differ are normalised.
//
// Both walks start from the same heights and the same explicit timeouts, so a
// difference in the output is wording rather than a different situation.

// walkParticipant is one participant in a scripted walk.
type walkParticipant struct {
	side  *rigtest.Side
	spool string
}

// walk runs one scripted sequence with a single implementation on both sides.
type walk struct {
	t        *testing.T
	r        *rigtest.Rig
	impl     string
	alice    *rigtest.Side
	bob      *rigtest.Side
	spool    string
	outputs  map[string]string
	failures map[string]string
}

// newWalk prepares two participants for one implementation.
func newWalk(t *testing.T, r *rigtest.Rig, impl string) *walk {
	t.Helper()

	participant := func(role string) *rigtest.Side {
		dir := t.TempDir()
		keyFile := filepath.Join(dir, role+".key")

		secret := make([]byte, 32)

		if _, err := rand.Read(secret); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(keyFile, []byte(hex.EncodeToString(secret)), 0o600); err != nil {
			t.Fatal(err)
		}

		return &rigtest.Side{Name: impl, Role: role, State: dir, KeyFile: keyFile}
	}

	return &walk{
		t: t, r: r, impl: impl,
		alice: participant("alice"), bob: participant("bob"),
		spool:    t.TempDir(),
		outputs:  map[string]string{},
		failures: map[string]string{},
	}
}

// do runs one step and records what it printed.
func (w *walk) do(name string, side *rigtest.Side, args ...string) string {
	w.t.Helper()

	return w.doEnv(name, side, nil, args...)
}

// doEnv runs one step with extra environment, which is how a step asks for a
// settlement that is broadcast but not mined: the confirmation has to be arranged by
// the script, not by the engine.
func (w *walk) doEnv(name string, side *rigtest.Side, env []string, args ...string) string {
	w.t.Helper()

	output, exit, err := w.r.Exec(w.t, side, w.spool, env, args...)

	if err != nil {
		w.t.Fatalf("%s %v: %v", w.impl, args, err)
	}

	if exit != 0 {
		w.failures[name] = fmt.Sprintf("exit %d: %s", exit, output)

		return output
	}

	w.outputs[name] = output

	return output
}

// swapID pulls the identifier the first step printed.
func (w *walk) swapID(output string) string {
	w.t.Helper()

	return rigtest.SwapID(w.t, output)
}

// TestCLIParityProtocolWalk runs the same protocol walk with each implementation
// and compares every step's output.
func TestCLIParityProtocolWalk(t *testing.T) {
	r := rigtest.Load(t)

	// The timeouts are read from the chain inside each walk rather than before
	// both: a walk mines blocks and funds legs, so the second walk would otherwise
	// start from a different height and the two would be describing different
	// situations.

	// The walk is written once and run twice.
	type script struct {
		name string
		role string
		args []string
	}

	steps := func(id string) []script {
		return []script{
			{name: "show", role: "alice", args: []string{"show", id}},
			{name: "list", role: "alice", args: []string{"list"}},
			{name: "status", role: "alice", args: []string{"status"}},
			{name: "reconcile", role: "alice", args: []string{"reconcile", id}},
			{name: "reconcile json", role: "alice", args: []string{"reconcile", id, "--json"}},
			{name: "publish-offer", role: "alice", args: []string{"publish-offer", id}},
			{name: "accept-offer", role: "bob", args: []string{"accept-offer", id, "--allow-unpinned-author"}},
			{name: "receive-accept", role: "alice", args: []string{"receive-accept", id}},
			{name: "fund-blake", role: "alice", args: []string{"fund-blake", id}},
			{name: "verify-blake", role: "alice", args: []string{"verify-blake", id}},
			{name: "publish-blake-funded", role: "alice", args: []string{"publish-blake-funded", id}},
			{name: "receive-blake-funded", role: "bob", args: []string{"receive-blake-funded", id}},
			{name: "fund-btc", role: "bob", args: []string{"fund-btc", id}},
			{name: "verify-btc", role: "bob", args: []string{"verify-btc", id}},
			{name: "publish-btc-funded", role: "bob", args: []string{"publish-btc-funded", id}},
			{name: "receive-btc-funded", role: "alice", args: []string{"receive-btc-funded", id}},
			{name: "dashboard", role: "alice", args: []string{"dashboard"}},
			{name: "claim-btc", role: "alice", args: []string{"claim-btc", id}},
			{name: "publish-btc-claimed", role: "alice", args: []string{"publish-btc-claimed", id}},
			{name: "receive-btc-claimed", role: "bob", args: []string{"receive-btc-claimed", id}},
			{name: "extract-secret", role: "bob", args: []string{"extract-secret", id}},
			{name: "claim-blake", role: "bob", args: []string{"claim-blake", id}},
			{name: "publish-blake-claimed", role: "bob", args: []string{"publish-blake-claimed", id}},
			{name: "receive-blake-claimed", role: "alice", args: []string{"receive-blake-claimed", id}},
			{name: "complete", role: "alice", args: []string{"complete", id}},
			{name: "status after", role: "alice", args: []string{"status"}},
			{name: "resume-all", role: "alice", args: []string{"resume-all"}},
		}
	}

	run := func(impl string) map[string]string {
		w := newWalk(t, r, impl)

		btcHeight := r.HeightOf(t, "btc")
		blakeHeight := r.HeightOf(t, "blake")

		created := w.do("new", w.alice, "new", "--blake", "0.001", "--sats", "25000",
			"--btc-timeout", fmt.Sprint(btcHeight+40),
			"--blake-timeout", fmt.Sprint(blakeHeight+80))

		id := w.swapID(created)

		for _, step := range steps(id) {
			side := w.alice

			if step.role == "bob" {
				side = w.bob
			}

			w.do(step.name, side, step.args...)
		}

		// A walk that failed a step has nothing to compare past that point, and the
		// failure is worth reporting on its own.
		for name, failure := range w.failures {
			t.Errorf("%s: step %q failed: %s", impl, name, failure)
		}

		collected := map[string]string{"new": created}

		for name, output := range w.outputs {
			collected[name] = output
		}

		return collected
	}

	reference := run("python")
	port := run("go")

	for name, want := range reference {
		got, present := port[name]

		if !present {
			t.Errorf("%s: the port produced no output for this step", name)

			continue
		}

		if normalise(want) != normalise(got) {
			t.Errorf("%s differs\n--- reference ---\n%s\n--- port ---\n%s",
				name, normalise(want), normalise(got))
		}
	}
}

// TestCLIParityRefundWalk is the abortive path: both legs refunded, each announced
// and imported, which is where the refund renderers are exercised.
func TestCLIParityRefundWalk(t *testing.T) {
	r := rigtest.Load(t)

	run := func(impl string) map[string]string {
		w := newWalk(t, r, impl)

		btcHeight := r.HeightOf(t, "btc")
		blakeHeight := r.HeightOf(t, "blake")

		created := w.do("new", w.alice, "new", "--blake", "0.001", "--sats", "25000",
			"--btc-timeout", fmt.Sprint(btcHeight+40),
			"--blake-timeout", fmt.Sprint(blakeHeight+80))

		id := w.swapID(created)

		w.do("publish-offer", w.alice, "publish-offer", id)
		w.do("accept-offer", w.bob, "accept-offer", id, "--allow-unpinned-author")
		w.do("receive-accept", w.alice, "receive-accept", id)
		w.do("fund-blake", w.alice, "fund-blake", id)
		w.do("publish-blake-funded", w.alice, "publish-blake-funded", id)
		w.do("receive-blake-funded", w.bob, "receive-blake-funded", id)
		w.do("fund-btc", w.bob, "fund-btc", id)
		w.do("publish-btc-funded", w.bob, "publish-btc-funded", id)
		w.do("receive-btc-funded", w.alice, "receive-btc-funded", id)

		// Bitcoin's deadline, then Bob refunds and Alice imports it.
		timeout := r.FieldInt(t, w.alice, w.spool, id, "btc_timeout")
		r.MineTo(t, "btc", "bob", timeout)

		w.do("refund-btc", w.bob, "refund-btc", id)
		w.do("publish-btc-refunded", w.bob, "publish-btc-refunded", id)
		w.do("receive-btc-refunded", w.alice, "receive-btc-refunded", id)

		// BLAKE's, then Alice refunds.
		//
		// Auto-mine is held off for this step so the refund is reported as broadcast
		// rather than confirmed. That is the shape a second invocation has to adopt:
		// after the block lands, the record names a confirmed refund, and asking again
		// must recognise it instead of signing a second transaction.
		timeout = r.FieldInt(t, w.alice, w.spool, id, "blake_timeout")
		r.MineTo(t, "blake", "alice", timeout)

		w.doEnv("refund-blake", w.alice, []string{"BLAKE_SWAP_AUTO_MINE=0"}, "refund-blake", id)
		r.Mine(t, "blake", "alice", 1)
		w.do("refund-blake-adopt", w.alice, "refund-blake", id)
		w.do("status", w.alice, "status")

		for name, failure := range w.failures {
			t.Errorf("%s: step %q failed: %s", impl, name, failure)
		}

		collected := map[string]string{"new": created}

		for name, output := range w.outputs {
			collected[name] = output
		}

		return collected
	}

	reference := run("python")
	port := run("go")

	for name, want := range reference {
		got, present := port[name]

		if !present {
			t.Errorf("%s: the port produced no output for this step", name)

			continue
		}

		if normalise(want) != normalise(got) {
			t.Errorf("%s differs\n--- reference ---\n%s\n--- port ---\n%s",
				name, normalise(want), normalise(got))
		}
	}
}
