//go:build !js

package rigsuites

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/internal/rigtest"
)

// Parity: tests/swap_peer_recovery_tests.py
//
// An actor disappears mid-settlement and comes back. The claim and refund paths
// are both covered, the durable funding survives a confirmed spend, and an idle
// poll is proved idle: a poll that finds nothing must not publish, must not
// advance, and must not move money.
func TestPeerRecoverySuite(t *testing.T) {
	r := rigtest.Load(t)

	t.Run("actor disappears on the claim path", func(t *testing.T) {
		scene := prepare(t, r, operations()[0]) // BTC claim

		// Alice dies with the claim broadcast but unrecorded as confirmed.
		output, exit, err := r.Exec(t, scene.actor, scene.messages,
			[]string{"BLAKE_SWAP_TEST_CRASH_POINT=btc-claim-after-broadcast"},
			"claim-btc", scene.id)

		if err != nil {
			t.Fatalf("running the armed claim: %v", err)
		}

		if exit != 86 {
			t.Fatalf("exit = %d, want 86\n%s", exit, output)
		}

		// Bob, who is still alive, must be able to prove the claim on the chain
		// even though the protocol message was never sent: the secret is in the
		// Bitcoin transaction's witness, not in Alice's message.
		fundingTxID, fundingVout := scene.funding(t, operations()[0])

		if utxo := r.Node(t, "btc", "gettxout", fundingTxID,
			fmt.Sprint(fundingVout)); strings.TrimSpace(utxo) != "" {
			t.Fatal("the claim is not in the mempool after the broadcast")
		}

		r.Mine(t, "btc", scene.actor.Role, 1)

		r.Run(t, scene.bob, scene.messages, "sync-settlement", scene.id)
		r.Run(t, scene.bob, scene.messages, "extract-secret", scene.id)

		rigtest.AssertFieldPresent(t, scene.bob, scene.id, "revealed_secret_hex")

		// And Bob can then claim BLAKE with it, without Alice ever sending anything.
		r.Run(t, scene.bob, scene.messages, "claim-blake", scene.id)

		r.AssertState(t, scene.bob, scene.messages, scene.id, "BLAKE_CLAIMED")
	})

	t.Run("actor disappears on the refund path", func(t *testing.T) {
		scene := prepare(t, r, operations()[3]) // BLAKE refund

		output, exit, err := r.Exec(t, scene.actor, scene.messages,
			[]string{"BLAKE_SWAP_TEST_CRASH_POINT=blake-refund-after-broadcast"},
			"refund-blake", scene.id)

		if err != nil {
			t.Fatalf("running the armed refund: %v", err)
		}

		if exit != 86 {
			t.Fatalf("exit = %d, want 86\n%s", exit, output)
		}

		txid, _ := rigtest.RecordField(t, scene.actor, scene.id, "blake_refund_txid")

		if fmt.Sprint(txid) == "" || fmt.Sprint(txid) == "<nil>" {
			t.Fatal("the killed refund left no durable transaction")
		}

		r.Mine(t, "blake", scene.actor.Role, 1)
		r.Run(t, scene.actor, scene.messages, "refund-blake", scene.id)

		r.AssertState(t, scene.actor, scene.messages, scene.id, "BLAKE_REFUNDED")

		// Bob, who never funded anything, converges on the same fact.
		r.Run(t, scene.bob, scene.messages, "sync-settlement", scene.id)

		r.AssertState(t, scene.bob, scene.messages, scene.id, "BLAKE_REFUNDED")
	})

	t.Run("recorded funding survives a confirmed spend", func(t *testing.T) {
		alice, bob, messages, id := r.SetupGoSwap(t)

		r.FundBothLegs(t, alice, bob, messages, id)

		r.Run(t, alice, messages, "claim-btc", id)

		// The HTLC is spent, so the funding is no longer *live*, but the record
		// must still name it: that is what lets a later reader prove which
		// outpoint the settlement spent.
		rigtest.AssertFieldPresent(t, alice, id, "btc_funding_txid")
		rigtest.AssertFieldPresent(t, alice, id, "btc_claim_txid")

		report := r.ReconcileJSON(t, alice, messages, id)

		leg, _ := report["btc"].(map[string]any)

		if status, _ := leg["status"].(string); status != "spent" {
			t.Fatalf("a confirmed claim reconciles to %q, want spent", status)
		}

		if decision, _ := report["decision_class"].(string); decision == "manual-attention" {
			t.Fatalf("a confirmed claim needs manual attention: %v", report["reason"])
		}
	})

	t.Run("idle poll changes nothing", func(t *testing.T) {
		alice, bob, messages, id := r.SetupGoSwap(t)

		r.FundBothLegs(t, alice, bob, messages, id)

		before := r.Snapshot(t, bob, id)
		spool := r.MessageSpool(t, messages)

		// Nothing has happened on either chain since the last sync, so a poll
		// must report the same state and publish nothing.
		r.Run(t, bob, messages, "sync-funding", id)
		r.Run(t, bob, messages, "sync-settlement", id)

		r.AssertSnapshotUnchanged(t, bob, id, before, "an idle poll")
		r.AssertNoNewMessages(t, messages, spool, "an idle poll")
	})
}

// Parity: tests/swap_chain_autonomy_tests.py
//
// The engine has to be able to reconstruct its position from the chains alone: a
// participant that was offline while a leg was funded, or while it was claimed,
// must converge without a coordination message. The mirror of that is the rule
// this suite exists for: when there is nothing to say, it says nothing.
func TestChainAutonomySuite(t *testing.T) {
	r := rigtest.Load(t)

	t.Run("idle funding poll", func(t *testing.T) {
		_, bob, messages, id := r.SetupGoSwap(t)

		// Nothing funded at all: a poll must be a no-op with no message.
		before := r.Snapshot(t, bob, id)
		spool := r.MessageSpool(t, messages)

		r.Run(t, bob, messages, "sync-funding", id)

		r.AssertSnapshotUnchanged(t, bob, id, before, "an idle funding poll")
		r.AssertNoNewMessages(t, messages, spool, "an idle funding poll")
	})

	t.Run("sync funding from the chain", func(t *testing.T) {
		alice, bob, messages, id := r.SetupGoSwap(t)

		// Alice funds and Bob never sees the announcement.
		r.Run(t, alice, messages, "fund-blake", id)

		r.AssertState(t, bob, messages, id, "OFFERED")

		spool := r.MessageSpool(t, messages)

		r.Run(t, bob, messages, "sync-funding", id)

		r.AssertState(t, bob, messages, id, "BLAKE_FUNDED")

		// Sync reads the chain; it does not announce. Publishing an announcement
		// for evidence the peer can read for itself is noise at best.
		r.AssertNoNewMessages(t, messages, spool, "a sync that found funding")
	})

	t.Run("sync settlement from the chain", func(t *testing.T) {
		alice, bob, messages, id := r.SetupGoSwap(t)

		r.FundBothLegs(t, alice, bob, messages, id)

		// Alice claims, and Bob's announcement never arrives.
		r.Run(t, alice, messages, "claim-btc", id)

		spool := r.MessageSpool(t, messages)

		r.Run(t, bob, messages, "sync-settlement", id)

		r.AssertState(t, bob, messages, id, "BTC_CLAIMED")
		r.AssertNoNewMessages(t, messages, spool, "a sync that found a settlement")

		// Bob now holds the secret because the chain revealed it.
		rigtest.AssertFieldPresent(t, bob, id, "revealed_secret_hex")
	})

	t.Run("nothing to say means nothing said", func(t *testing.T) {
		alice, bob, messages, id := r.SetupGoSwap(t)

		r.FundBothLegs(t, alice, bob, messages, id)

		// Both announce; Bob's second publish of the same fact is refused rather
		// than duplicating it on the wire.
		r.Run(t, bob, messages, "publish-btc-funded", id)

		spool := r.MessageSpool(t, messages)
		count := len(spool)

		r.Run(t, bob, messages, "publish-btc-funded", id)

		if after := len(r.MessageSpool(t, messages)); after != count {
			t.Fatalf("a repeated announcement published %d new message(s)", after-count)
		}
	})
}

// Parity: tests/swap_participant_isolation_tests.py
//
// Two participants, separate everything. The suite exists because a port that
// keeps one process's records in another's directory, or one participant's key in
// a shared file, can still pass every protocol test while leaking.
func TestParticipantIsolationSuite(t *testing.T) {
	r := rigtest.Load(t)

	alice, bob, messages, id := r.SetupGoSwap(t)

	if alice.State == bob.State {
		t.Fatal("the two participants share a state directory")
	}

	if alice.KeyFile == bob.KeyFile {
		t.Fatal("the two participants share an identity file")
	}

	if alice.Public == bob.Public {
		t.Fatal("the two participants derived the same identity")
	}

	// A record belongs to one participant's directory, and the other directory
	// has no copy of it until that participant writes one.
	if _, err := os.Stat(fmt.Sprintf("%s/%s.json", bob.State, id)); err != nil {
		entries, _ := os.ReadDir(bob.State)

		names := make([]string, 0, len(entries))

		for _, entry := range entries {
			names = append(names, entry.Name())
		}

		t.Fatalf("Bob has no record in his own directory (%v); it holds %v",
			err, names)
	}

	r.FundBothLegs(t, alice, bob, messages, id)

	// Alice is the secret holder; Bob's directory must not contain it.
	secret, _ := rigtest.RecordField(t, alice, id, "secret_hex")

	if fmt.Sprint(secret) == "" || fmt.Sprint(secret) == "<nil>" {
		t.Fatal("Alice holds no secret")
	}

	rigtest.AssertFieldAbsent(t, bob, id, "secret_hex")
	rigtest.AssertFieldAbsent(t, bob, id, "revealed_secret_hex")

	if strings.Contains(r.Snapshot(t, bob, id), fmt.Sprint(secret)) {
		t.Fatal("Bob's record contains Alice's secret")
	}

	// Alice's economic action changes Alice's record and not Bob's.
	bobBefore := r.Snapshot(t, bob, id)

	r.Run(t, alice, messages, "claim-btc", id)

	r.AssertSnapshotUnchanged(t, bob, id, bobBefore, "Alice's Bitcoin claim")

	// And Bob still cannot perform Alice's BLAKE funding with his own key.
	r.Expect(t, bob, messages, "protocol requires alice", "fund-blake", id)
}

// assertTimeoutWindows checks the cross-chain safety margin the record carries.
//
// The record prints BTC window, Bob's recovery budget and the BLAKE window in seconds for a
// v1.5 record; the BLAKE window must exceed the BTC window by at least the recovery budget.
// Heights are deliberately not compared: they belong to independent chains.
func assertTimeoutWindows(
	t *testing.T, r *rigtest.Rig, side *rigtest.Side, messages, id string,
) {
	t.Helper()

	btcWindow := r.FieldInt(t, side, messages, id, "btc_window")
	recovery := r.FieldInt(t, side, messages, id, "bob_recovery")
	blakeWindow := r.FieldInt(t, side, messages, id, "blake_window")

	if btcWindow <= 0 || recovery <= 0 || blakeWindow <= 0 {
		t.Fatalf("the record does not carry the timeout windows: %d/%d/%d",
			btcWindow, recovery, blakeWindow)
	}

	if gap := blakeWindow - btcWindow; gap < recovery {
		t.Fatalf(
			"BLAKE window %d is only %d s past Bitcoin's %d, less than Bob's %d s budget",
			blakeWindow, gap, btcWindow, recovery,
		)
	}
}

// Parity: tests/pre_btc_refund_tests.py
//
// Alice funds BLAKE and Bob never funds Bitcoin. Until BLAKE's own timelock she
// has no way out and must not pretend to; after it she refunds directly, and both
// participants converge on BLAKE_REFUNDED without a Bitcoin leg ever existing and
// without a settlement message from Alice.
func TestPreBTCRefundSuite(t *testing.T) {
	r := rigtest.Load(t)

	alice, bob, messages, id := r.SetupGoSwap(t)

	r.Run(t, alice, messages, "fund-blake", id)
	r.Run(t, alice, messages, "publish-blake-funded", id)
	r.Run(t, bob, messages, "receive-blake-funded", id)

	//
	// The two CLTV heights are absolute heights on two independent chains, so their
	// ordering means nothing: a BLAKE height can be above or below a Bitcoin height
	// depending on how many blocks each chain happens to have mined. The policy records
	// the safety margins as wall-clock windows instead, and that difference is the
	// invariant. Asserting the heights here was comparing two unrelated counters.
	//
	// pkg/timeout is the authority: ValidatePlan accepts a plan when
	// BlakeWindowSeconds - BTCWindowSeconds covers Bob's recovery budget. This test
	// restates the same gap from what the record prints, so a policy change that
	// silently dropped the margin would fail here as well as in the unit suite.
	//
	assertTimeoutWindows(t, r, alice, messages, id)

	// And the same invariant on a pair of chains at very different heights, which is
	// the case an absolute comparison cannot survive: the heights differ by hundreds of
	// blocks, the windows do not.
	r.Mine(t, "btc", alice.Role, 300)

	other := r.Handshake(t, alice, bob, messages)

	r.Run(t, alice, messages, "fund-blake", other)

	assertTimeoutWindows(t, r, alice, messages, other)

	// The CLTV deadline is a BLAKE height, so it is read from the swap this test refunds.
	blakeTimeout := r.FieldInt(t, alice, messages, id, "blake_timeout")

	// Before her own timelock Alice waits. There is nothing to claim and nothing
	// to refund, and a refusal is the honest answer.
	r.Expect(t, alice, messages, "", "refund-blake", id)

	r.AssertState(t, alice, messages, id, "BLAKE_FUNDED")

	// The CLTV is a BLAKE height, so the deadline is reached by mining that chain.
	// A refund attempted before it is refused by the chain and by the engine.
	r.MineTo(t, "blake", alice.Role, blakeTimeout-1)

	r.Expect(t, alice, messages, "", "refund-blake", id)

	r.AssertState(t, alice, messages, id, "BLAKE_FUNDED")

	// After it, she refunds directly with no Bitcoin leg in existence.
	r.MineTo(t, "blake", alice.Role, blakeTimeout)

	// A refund submitted exactly at its CLTV height can be final for the next
	// block rather than this one, so the reported settlement is legitimately
	// pending. Mining a block and asking again adopts the transaction the record
	// already names; it does not sign a second one.
	output := r.Run(t, alice, messages, "refund-blake", id)

	before, _ := rigtest.RecordField(t, alice, id, "blake_refund_txid")

	if strings.Contains(output, "BLAKE_FUNDED") {
		r.Mine(t, "blake", alice.Role, 1)

		r.Run(t, alice, messages, "refund-blake", id)

		after, _ := rigtest.RecordField(t, alice, id, "blake_refund_txid")

		if after != before {
			t.Fatalf("the confirming retry signed a second refund: %v -> %v", before, after)
		}
	}

	r.AssertState(t, alice, messages, id, "BLAKE_REFUNDED")

	blakeTxID, blakeVout := fundingOutpoint(t, alice, id, "blake")

	if utxo := r.Node(t, "blake", "gettxout", blakeTxID,
		fmt.Sprint(blakeVout)); strings.TrimSpace(utxo) != "" {
		t.Fatal("the BLAKE HTLC is unspent after a refund")
	}

	// Bob still says BLAKE_FUNDED, and only the chain can tell him otherwise: the
	// report names the recovery, and the autonomous path performs it without any
	// message from Alice.
	report := r.ReconcileJSON(t, bob, messages, id)

	leg, _ := report["blake"].(map[string]any)

	if status, _ := leg["status"].(string); status != "spent" {
		t.Fatalf("Bob does not observe the confirmed BLAKE spend: %v", status)
	}

	if branch, _ := leg["branch"].(string); branch != "refund" {
		t.Fatalf("Bob does not identify the refund branch: %v", branch)
	}

	if action, _ := report["next_action"].(string); action != "sync-settlement" {
		t.Fatalf("Bob is not offered a chain-derived settlement sync: %v", action)
	}

	r.Run(t, bob, messages, "resume", id)

	r.AssertState(t, bob, messages, id, "BLAKE_REFUNDED")

	for _, side := range []*rigtest.Side{alice, bob} {
		final := r.ReconcileJSON(t, side, messages, id)

		if checkpoint, _ := final["checkpoint_state"].(string); checkpoint != "BLAKE_REFUNDED" {
			t.Fatalf("%s converged on %q, want BLAKE_REFUNDED", side.Name, checkpoint)
		}
	}
}
