//go:build !js

package swap_test

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
