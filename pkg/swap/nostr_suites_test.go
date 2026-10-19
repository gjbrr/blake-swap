//go:build !js

package swap_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/internal/rigtest"
	"github.com/gjbrr/blake-swap/pkg/relaystub"
)

// Two participants with nothing in common but a relay: separate state
// directories, separate identity files, separate spools. Everything they know
// about each other arrives as a signed event, which is what makes these the
// isolated suites rather than the spool-driven ones.

// isolated is two participants and the relay between them.
type isolated struct {
	r      *rigtest.Rig
	stub   *relaystub.Stub
	alice  *rigtest.Side
	bob    *rigtest.Side
	aliceS string
	bobS   string
}

// newIsolated starts a loopback relay and two participants that share nothing else.
func newIsolated(t *testing.T) *isolated {
	t.Helper()

	r := rigtest.Load(t)

	stub, err := relaystub.New()

	if err != nil {
		t.Fatalf("starting the relay: %v", err)
	}

	t.Cleanup(stub.Close)

	return &isolated{
		r:      r,
		stub:   stub,
		alice:  rigtest.NewSide(t, "go", "alice"),
		bob:    rigtest.NewSide(t, "go", "bob"),
		aliceS: t.TempDir(),
		bobS:   t.TempDir(),
	}
}

// env points a participant at the relay rather than at the local spool.
func (i *isolated) env(extra ...string) []string {
	environment := []string{
		"BLAKE_SWAP_TRANSPORT=nostr",
		"BLAKE_SWAP_NOSTR_RELAYS=" + i.stub.URL,
	}

	return append(environment, extra...)
}

// run executes one command as one participant, over the relay.
func (i *isolated) run(t *testing.T, s *rigtest.Side, spool string, args ...string) string {
	t.Helper()

	output, exit, err := i.r.Exec(t, s, spool, i.env(), args...)

	if err != nil {
		t.Fatalf("running %v: %v", args, err)
	}

	if exit != 0 {
		t.Fatalf("%s %v failed with exit %d:\n%s", s.Name, args, exit, output)
	}

	return output
}

// expect runs one command and requires it to be refused.
func (i *isolated) expect(t *testing.T, s *rigtest.Side, spool, fragment string, args ...string) string {
	t.Helper()

	output, exit, err := i.r.Exec(t, s, spool, i.env(), args...)

	if err != nil {
		t.Fatalf("running %v: %v", args, err)
	}

	if exit == 0 {
		t.Fatalf("%v was accepted; expected a refusal containing %q\n%s",
			args, fragment, output)
	}

	if fragment != "" && !strings.Contains(output, fragment) {
		t.Fatalf("%v refused for the wrong reason; expected %q:\n%s",
			args, fragment, output)
	}

	return output
}

// Parity: tests/swap_isolated_nostr_handshake_tests.py
//
// The handshake with no shared filesystem: Alice publishes her offer as a signed
// event, Bob reads it from the relay and pins her identity, and Alice imports his
// acceptance the same way. A message from an author Bob did not expect is refused.
func TestIsolatedNostrHandshakeSuite(t *testing.T) {
	i := newIsolated(t)

	output := i.run(t, i.alice, i.aliceS, "new", "--blake", "0.001", "--sats", "25000")
	id := rigtest.SwapID(t, output)

	i.run(t, i.alice, i.aliceS, "publish-offer", id)

	published := i.stub.Events()

	if len(published) == 0 {
		t.Fatal("the offer never reached the relay")
	}

	// Bob has nothing but the relay and the swap id.
	i.run(t, i.bob, i.bobS, "accept-offer", id, "--expected-alice-pub", i.alice.Public)

	// A second offer, and a pin that names the wrong participant: the acceptance
	// must be refused rather than trusting whichever author the relay returns.
	second := rigtest.SwapID(t, i.run(t, i.alice, i.aliceS, "new", "--blake", "0.002", "--sats", "30000"))

	i.run(t, i.alice, i.aliceS, "publish-offer", second)

	i.expect(t, i.bob, i.bobS, "", "accept-offer", second, "--expected-alice-pub", i.bob.Public)

	i.run(t, i.alice, i.aliceS, "receive-accept", id)

	// Alice now holds Bob's two contract keys, which is the only thing the accept
	// message exists to carry.
	rigtest.AssertFieldPresent(t, i.alice, id, "blake_bob_pub")
	rigtest.AssertFieldPresent(t, i.alice, id, "btc_bob_pub")

	// And neither side learned the other's secret.
	rigtest.AssertFieldPresent(t, i.alice, id, "secret_hex")
	rigtest.AssertFieldAbsent(t, i.bob, id, "secret_hex")

	// The acceptance travelled as a signed event, so the relay holds it too.
	if events := i.stub.Events(); len(events) < 2 {
		t.Fatalf("the relay holds %d event(s); the acceptance did not arrive", len(events))
	}
}

// Parity: tests/swap_isolated_full_chain_swap_tests.py
//
// The whole swap over the relay: fund BLAKE, fund Bitcoin, claim Bitcoin, recover
// the secret from the chain, claim BLAKE, complete. Every announcement and every
// import in that walk crosses the relay, and nothing crosses a shared file.
func TestIsolatedFullChainSwapSuite(t *testing.T) {
	i := newIsolated(t)

	output := i.run(t, i.alice, i.aliceS, "new", "--blake", "0.001", "--sats", "25000")
	id := rigtest.SwapID(t, output)

	i.run(t, i.alice, i.aliceS, "publish-offer", id)
	i.run(t, i.bob, i.bobS, "accept-offer", id, "--expected-alice-pub", i.alice.Public)
	i.run(t, i.alice, i.aliceS, "receive-accept", id)

	// BLAKE funded.
	i.run(t, i.alice, i.aliceS, "fund-blake", id)
	i.run(t, i.alice, i.aliceS, "publish-blake-funded", id)
	i.run(t, i.bob, i.bobS, "receive-blake-funded", id)

	// Bitcoin funded, on the strength of the BLAKE leg Bob verified himself.
	i.run(t, i.bob, i.bobS, "fund-btc", id)
	i.run(t, i.bob, i.bobS, "publish-btc-funded", id)
	i.run(t, i.alice, i.aliceS, "receive-btc-funded", id)

	// Bitcoin claimed: the claim's witness is how Bob learns the secret.
	i.run(t, i.alice, i.aliceS, "claim-btc", id)
	i.run(t, i.alice, i.aliceS, "publish-btc-claimed", id)
	i.run(t, i.bob, i.bobS, "receive-btc-claimed", id)
	i.run(t, i.bob, i.bobS, "extract-secret", id)

	rigtest.AssertFieldPresent(t, i.bob, id, "revealed_secret_hex")

	// BLAKE claimed, and the record closed.
	i.run(t, i.bob, i.bobS, "claim-blake", id)
	i.run(t, i.bob, i.bobS, "publish-blake-claimed", id)
	i.run(t, i.alice, i.aliceS, "receive-blake-claimed", id)
	i.run(t, i.alice, i.aliceS, "complete", id)

	i.r.AssertState(t, i.alice, i.aliceS, id, "COMPLETE")
	i.r.AssertState(t, i.bob, i.bobS, id, "BLAKE_CLAIMED")

	// Both HTLCs are spent, so the swap is settled on both chains rather than only
	// in the records.
	for _, which := range []string{"blake", "btc"} {
		txid, vout := fundingOutpoint(t, i.alice, id, which)

		if utxo := i.r.Node(t, which, "gettxout", txid,
			fmt.Sprint(vout)); strings.TrimSpace(utxo) != "" {
			t.Fatalf("the %s HTLC is unspent after a completed isolated swap", which)
		}
	}
}

// TestIsolatedNostrRefundSuite exercises the seventh settlement message type over
// the relay. The handshake and the happy path cover six; a refund announcement is
// the one that only exists on the abortive path, and it must travel the same way.
//
// Parity: tests/swap_isolated_full_chain_swap_tests.py
func TestIsolatedNostrRefundSuite(t *testing.T) {
	i := newIsolated(t)

	output := i.run(t, i.alice, i.aliceS, "new", "--blake", "0.001", "--sats", "25000")
	id := rigtest.SwapID(t, output)

	i.run(t, i.alice, i.aliceS, "publish-offer", id)
	i.run(t, i.bob, i.bobS, "accept-offer", id, "--expected-alice-pub", i.alice.Public)
	i.run(t, i.alice, i.aliceS, "receive-accept", id)

	i.run(t, i.alice, i.aliceS, "fund-blake", id)
	i.run(t, i.alice, i.aliceS, "publish-blake-funded", id)
	i.run(t, i.bob, i.bobS, "receive-blake-funded", id)

	i.run(t, i.bob, i.bobS, "fund-btc", id)
	i.run(t, i.bob, i.bobS, "publish-btc-funded", id)
	i.run(t, i.alice, i.aliceS, "receive-btc-funded", id)

	// Bitcoin's deadline is reached, so Bob refunds and announces it over the relay.
	timeout := i.r.FieldInt(t, i.alice, i.aliceS, id, "btc_timeout")
	i.r.MineTo(t, "btc", i.bob.Role, timeout)

	i.run(t, i.bob, i.bobS, "refund-btc", id)
	i.run(t, i.bob, i.bobS, "publish-btc-refunded", id)
	i.run(t, i.alice, i.aliceS, "receive-btc-refunded", id)

	i.r.AssertState(t, i.bob, i.bobS, id, "BTC_REFUNDED")
	i.r.AssertState(t, i.alice, i.aliceS, id, "BTC_REFUNDED")

	// The secret never moved, and neither record claims it did.
	rigtest.AssertFieldAbsent(t, i.alice, id, "revealed_secret_hex")
	rigtest.AssertFieldAbsent(t, i.bob, id, "revealed_secret_hex")
}
