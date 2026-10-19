//go:build !js

package swap_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/internal/rigtest"
)

// These tests run the Go engine against the Python reference as the counterparty,
// in both directions, over a shared file-transport spool and the local regtest rig.
//
// They are the honest answer to "is this a port or a rewrite?". A shared spool means
// every message the Go side writes is parsed by the Python engine and vice versa,
// and every record each side writes is loaded by the other — so a field name, an
// amount rendering, or a sender identity that only agrees with itself shows up here
// as a failed swap rather than as a passing unit test.
//
// The rig, the two participants and the process plumbing come from
// internal/rigtest, which every integration suite in this repository shares.

// TestCrossImplHandshakeAndSettlement runs both directions of the protocol with one
// implementation on each side.
// Parity: tests/swap_cross_impl_offer_tests.py, tests/swap_cross_impl_accept_tests.py, tests/swap_cross_impl_blake_funded_tests.py, tests/swap_cross_impl_btc_funded_tests.py, tests/swap_cross_impl_btc_claimed_tests.py, tests/swap_cross_impl_blake_claimed_tests.py, tests/swap_cross_impl_full_happy_tests.py
func TestCrossImplHandshakeAndSettlement(t *testing.T) {
	r := rigtest.Load(t)

	for _, dir := range rigtest.Directions() {
		t.Run(dir.Name, func(t *testing.T) {
			alice, bob, messages, id := r.SetupSwap(t, dir)

			r.FundBothLegs(t, alice, bob, messages, id)

			r.AssertReconcileAgrees(t, alice, bob, messages, id)

			r.Run(t, alice, messages, "claim-btc", id)
			r.Run(t, alice, messages, "publish-btc-claimed", id)

			r.Run(t, bob, messages, "receive-btc-claimed", id)
			r.Run(t, bob, messages, "extract-secret", id)
			r.Run(t, bob, messages, "claim-blake", id)
			r.Run(t, bob, messages, "publish-blake-claimed", id)

			r.Run(t, alice, messages, "receive-blake-claimed", id)
			r.Run(t, alice, messages, "complete", id)

			r.AssertState(t, alice, messages, id, "COMPLETE")
			r.AssertState(t, bob, messages, id, "BLAKE_CLAIMED")
		})
	}
}

// TestCrossImplRefundBothLegs runs the abortive path in both directions: Bob
// refunds Bitcoin once its timelock is reached, and Alice refunds BLAKE once hers
// is. This is the path that matters when the secret never moves, so it is the one
// where a message-shape disagreement would strand real money.
func TestCrossImplRefundBothLegs(t *testing.T) {
	r := rigtest.Load(t)

	for _, dir := range rigtest.Directions() {
		t.Run(dir.Name, func(t *testing.T) {
			alice, bob, messages, id := r.SetupSwap(t, dir)

			r.FundBothLegs(t, alice, bob, messages, id)

			// Bob can only refund once Bitcoin's CLTV is reached, and the node
			// refuses a non-final transaction rather than reporting a protocol
			// error — so the test mines to the deadline the way an operator would.
			btcTimeout := r.FieldInt(t, alice, messages, id, "btc_timeout")
			r.MineTo(t, "btc", bob.Role, btcTimeout)

			r.Run(t, bob, messages, "refund-btc", id)
			r.Run(t, bob, messages, "publish-btc-refunded", id)
			r.Run(t, alice, messages, "receive-btc-refunded", id)

			blakeTimeout := r.FieldInt(t, alice, messages, id, "blake_timeout")
			r.MineTo(t, "blake", alice.Role, blakeTimeout)

			r.Run(t, alice, messages, "refund-blake", id)

			r.AssertState(t, alice, messages, id, "BLAKE_REFUNDED")
			r.AssertState(t, bob, messages, id, "BTC_REFUNDED")
		})
	}
}

// TestCrossImplRecordIsReadableByPython is the cheapest interop check, and the one
// that fails first when a field name drifts: a record written by the Go engine must
// load in the Python engine.
func TestCrossImplRecordIsReadableByPython(t *testing.T) {
	r := rigtest.Load(t)

	messages := t.TempDir()
	goSide := rigtest.NewSide(t, "go", "alice")

	output := r.RunGo(t, goSide, messages, "new", "--blake", "0.002", "--sats", "30000")
	id := rigtest.SwapID(t, output)

	// The Python engine holds no record for this swap, so it re-reads the file the
	// Go engine wrote under the same state directory.
	script := fmt.Sprintf(`
import sys
sys.path.insert(0, %q)
import swap_v2
record = swap_v2.load_swap(%q, role="alice")
print("python loaded:", record.swap_id)
print("state:", record.state)
print("blake base units:", record.blake_amount_base_units)
`, filepath.Join(r.Repo, "src"), id)

	loaded := r.RunPythonScript(t, goSide, messages, script)

	if !strings.Contains(loaded, "python loaded: "+id) {
		t.Fatalf("Python did not load the record:\n%s", loaded)
	}
}

// TestCrossImplStageByStage asserts the protocol's checkpoints one message at a time.
//
// The reference's cross-implementation suites are organised by stage — offer, accept, BLAKE
// funded, Bitcoin funded, Bitcoin claimed, BLAKE claimed, the whole happy path — because a
// counterpoint that agrees at the end can still have agreed for the wrong reasons. This walks
// the same stages in the same order, in both directions, and asserts what each side believes
// after every message rather than only at the end.
func TestCrossImplStageByStage(t *testing.T) {
	r := rigtest.Load(t)

	for _, dir := range rigtest.Directions() {
		t.Run(dir.Name, func(t *testing.T) {
			messages := t.TempDir()

			alice, bob := rigtest.Sides(t, dir)

			// Stage 1: offer. Alice writes it; Bob's accept must refuse anything but the
			// Alice identity the offer names.
			output := r.Run(t, alice, messages, "new", "--blake", "0.001", "--sats", "25000")
			id := rigtest.SwapID(t, output)

			r.AssertState(t, alice, messages, id, "OFFERED")

			r.Run(t, alice, messages, "publish-offer", id)

			// Stage 2: accept. Bob consumes the offer and pins Alice's identity from the
			// message itself, so the pairing is authenticated rather than configured.
			r.Run(t, bob, messages, "accept-offer", id, "--expected-alice-pub", alice.Public)

			r.AssertState(t, bob, messages, id, "OFFERED")

			r.Run(t, alice, messages, "receive-accept", id)

			// Alice now knows Bob's two contract keys and nothing else, which is the
			// property the accept message exists to carry. `show` renders the reference's
			// print_swap, which does not list contract keys, so the record itself is read:
			// the claim here is about what is stored, not about what is displayed.
			rigtest.AssertFieldPresent(t, alice, id, "blake_bob_pub")
			rigtest.AssertFieldPresent(t, alice, id, "btc_bob_pub")

			// And nothing in Alice's record may be Bob's secret: she has the hashlock.
			rigtest.AssertFieldAbsent(t, alice, id, "revealed_secret_hex")

			// Stage 3: BLAKE funded. Alice funds and announces; Bob validates the
			// announcement against the chain rather than trusting it.
			r.Run(t, alice, messages, "fund-blake", id)

			r.AssertState(t, alice, messages, id, "BLAKE_FUNDED")

			r.Run(t, alice, messages, "publish-blake-funded", id)
			r.Run(t, bob, messages, "receive-blake-funded", id)

			r.AssertState(t, bob, messages, id, "BLAKE_FUNDED")

			// Stage 4: Bitcoin funded. Bob funds only after independently checking the
			// BLAKE leg, and Alice then verifies his announcement the same way.
			r.Run(t, bob, messages, "fund-btc", id)

			r.AssertState(t, bob, messages, id, "BTC_FUNDED")

			r.Run(t, bob, messages, "publish-btc-funded", id)
			r.Run(t, alice, messages, "receive-btc-funded", id)

			r.AssertState(t, alice, messages, id, "BTC_FUNDED")

			// Stage 5: Bitcoin claimed. Alice reveals the preimage in the claim, which is
			// how Bob learns the secret — there is no message that carries it.
			r.Run(t, alice, messages, "claim-btc", id)

			r.AssertState(t, alice, messages, id, "BTC_CLAIMED")

			r.Run(t, alice, messages, "publish-btc-claimed", id)
			r.Run(t, bob, messages, "receive-btc-claimed", id)

			// Stage 6: BLAKE claimed. Bob recovers the secret from the Bitcoin chain, not
			// from Alice, then claims with it.
			r.Run(t, bob, messages, "extract-secret", id)
			r.Run(t, bob, messages, "claim-blake", id)

			r.AssertState(t, bob, messages, id, "BLAKE_CLAIMED")

			r.Run(t, bob, messages, "publish-blake-claimed", id)
			r.Run(t, alice, messages, "receive-blake-claimed", id)

			// Stage 7: complete. Alice's record closes; Bob's stays at BLAKE_CLAIMED
			// because the BLAKE claim is his last protocol act.
			r.Run(t, alice, messages, "complete", id)

			r.AssertState(t, alice, messages, id, "COMPLETE")
			r.AssertState(t, bob, messages, id, "BLAKE_CLAIMED")
		})
	}
}
