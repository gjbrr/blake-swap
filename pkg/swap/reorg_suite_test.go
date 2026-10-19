//go:build !js

package swap_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/internal/rigtest"
)

// Parity: tests/swap_reorg_safety_tests.py
//
// A reorg is the one failure the durable record cannot prevent: the chain simply
// stops agreeing with what a participant wrote down. The suite uses the real node
// (invalidateblock / reconsiderblock) rather than editing a record, because the
// question is what the engine does when the chain changes under it.
//
// The invariant every case here shares: a record may not keep claiming a
// confirmation the active chain no longer has, and an announcement whose evidence
// has been rolled back must be refused.
func TestReorgSafetySuite(t *testing.T) {
	r := rigtest.Load(t)

	t.Run("blake funding reorg blocks bitcoin funding", func(t *testing.T) {
		alice, bob, messages, id := r.SetupGoSwap(t)

		r.Run(t, alice, messages, "fund-blake", id)
		r.Run(t, alice, messages, "publish-blake-funded", id)
		r.Run(t, bob, messages, "receive-blake-funded", id)

		blockHash, _ := rigtest.RecordField(t, alice, id, "blake_funding_blockhash")

		restore := r.InvalidateBlock(t, "blake", fmt.Sprint(blockHash))
		defer restore()

		// Bob's durable state still says BLAKE_FUNDED -- the record is history --
		// but he must not act on it: funding his own leg against a counterparty
		// commitment that is no longer confirmed is exactly the mistake this case
		// exists to catch.
		r.AssertState(t, bob, messages, id, "BLAKE_FUNDED")

		output := r.Expect(t, bob, messages, "", "fund-btc", id)

		lowered := strings.ToLower(output)

		if !strings.Contains(lowered, "not confirmed") &&
			!strings.Contains(lowered, "unavailable") &&
			!strings.Contains(lowered, "already spent") {
			t.Fatalf("Bob's fund-btc failed for an unexpected reason:\n%s", output)
		}

		// And it works again once the chain agrees with the record.
		restore()

		r.Run(t, bob, messages, "fund-btc", id)
	})

	t.Run("bitcoin funding reorg blocks the bitcoin claim", func(t *testing.T) {
		alice, bob, messages, id := r.SetupGoSwap(t)

		r.FundBothLegs(t, alice, bob, messages, id)

		blockHash, _ := rigtest.RecordField(t, bob, id, "btc_funding_blockhash")

		restore := r.InvalidateBlock(t, "btc", fmt.Sprint(blockHash))
		defer restore()

		// Alice must not claim an outpoint the active chain no longer funds.
		r.Expect(t, alice, messages, "", "claim-btc", id)

		r.AssertState(t, alice, messages, id, "BTC_FUNDED")

		restore()

		// The funding transaction returns to the mempool, so it needs a block
		// before the claim is buildable again.
		r.Mine(t, "btc", alice.Role, 1)
		r.Run(t, bob, messages, "sync-funding", id)
		r.Run(t, alice, messages, "sync-funding", id)

		r.AssertState(t, alice, messages, id, "BTC_FUNDED")
		r.Run(t, alice, messages, "claim-btc", id)
	})

	t.Run("a reorged claim announcement is refused", func(t *testing.T) {
		alice, bob, messages, id := r.SetupGoSwap(t)

		r.FundBothLegs(t, alice, bob, messages, id)

		r.Run(t, alice, messages, "claim-btc", id)
		r.Run(t, alice, messages, "publish-btc-claimed", id)

		blockHash, _ := rigtest.RecordField(t, alice, id, "btc_claim_blockhash")

		restore := r.InvalidateBlock(t, "btc", fmt.Sprint(blockHash))
		defer restore()

		// The announcement is authentic and correctly signed. It is refused
		// because the settlement it names is not in the active chain, which is a
		// different reason from a bad signature and must be reported as such.
		before := r.Snapshot(t, bob, id)

		r.Expect(t, bob, messages, "", "receive-btc-claimed", id)

		r.AssertSnapshotUnchanged(t, bob, id, before, "a reorged claim announcement")

		// Unused: reorged announcement refused for the claim path.
	})

	t.Run("a reorged bitcoin refund announcement is refused", func(t *testing.T) {
		alice, bob, messages, id := r.SetupGoSwap(t)

		r.FundBothLegs(t, alice, bob, messages, id)
		r.MineTo(t, "btc", bob.Role, r.FieldInt(t, alice, messages, id, "btc_timeout"))

		r.Run(t, bob, messages, "refund-btc", id)
		r.Run(t, bob, messages, "publish-btc-refunded", id)

		blockHash, _ := rigtest.RecordField(t, bob, id, "btc_refund_blockhash")

		restore := r.InvalidateBlock(t, "btc", fmt.Sprint(blockHash))
		defer restore()

		before := r.Snapshot(t, alice, id)

		r.Expect(t, alice, messages, "", "receive-btc-refunded", id)

		r.AssertSnapshotUnchanged(t, alice, id, before, "a reorged refund announcement")
	})

	t.Run("a reorged blake claim announcement is refused", func(t *testing.T) {
		alice, bob, messages, id := r.SetupGoSwap(t)

		r.FundBothLegs(t, alice, bob, messages, id)
		r.Run(t, alice, messages, "claim-btc", id)
		r.Run(t, alice, messages, "publish-btc-claimed", id)
		r.Run(t, bob, messages, "receive-btc-claimed", id)
		r.Run(t, bob, messages, "extract-secret", id)
		r.Run(t, bob, messages, "claim-blake", id)
		r.Run(t, bob, messages, "publish-blake-claimed", id)

		blockHash, _ := rigtest.RecordField(t, bob, id, "blake_claim_blockhash")

		restore := r.InvalidateBlock(t, "blake", fmt.Sprint(blockHash))
		defer restore()

		before := r.Snapshot(t, alice, id)

		r.Expect(t, alice, messages, "", "receive-blake-claimed", id)

		r.AssertSnapshotUnchanged(t, alice, id, before, "a reorged BLAKE claim announcement")
	})

	t.Run("a final claim that is rolled back is no longer final", func(t *testing.T) {
		alice, bob, messages, id := r.SetupGoSwap(t)

		r.FundBothLegs(t, alice, bob, messages, id)
		r.Run(t, alice, messages, "claim-btc", id)
		r.Run(t, alice, messages, "publish-btc-claimed", id)
		r.Run(t, bob, messages, "receive-btc-claimed", id)
		r.Run(t, bob, messages, "extract-secret", id)
		r.Run(t, bob, messages, "claim-blake", id)

		r.AssertState(t, bob, messages, id, "BLAKE_CLAIMED")

		blockHash, _ := rigtest.RecordField(t, bob, id, "blake_claim_blockhash")

		restore := r.InvalidateBlock(t, "blake", fmt.Sprint(blockHash))
		defer restore()

		// The record still says BLAKE_CLAIMED -- it is history -- but the report
		// must not present the settlement as confirmed, and must not recommend a
		// terminal action.
		report := r.ReconcileJSON(t, bob, messages, id)

		leg, _ := report["blake"].(map[string]any)

		if status, _ := leg["status"].(string); status == "spent" {
			t.Fatalf("a rolled-back claim is still reported as spent: %v", leg)
		}

		if class, _ := report["decision_class"].(string); class == "settled" {
			t.Fatalf("a rolled-back claim is reported as settled: %v", report["reason"])
		}

		// Reconnecting the chain restores the settlement, with the same
		// transaction: nothing was re-signed while it was gone.
		txid, _ := rigtest.RecordField(t, bob, id, "blake_claim_txid")

		restore()
		r.Mine(t, "blake", bob.Role, 1)
		r.Run(t, bob, messages, "sync-settlement", id)

		r.AssertState(t, bob, messages, id, "BLAKE_CLAIMED")

		if after, _ := rigtest.RecordField(t, bob, id, "blake_claim_txid"); after != txid {
			t.Fatalf("the reappearing claim is a different transaction: %v -> %v",
				txid, after)
		}
	})

	t.Run("a final claim rolled back is rebroadcast from the record", func(t *testing.T) {
		alice, bob, messages, id := r.SetupGoSwap(t)

		r.FundBothLegs(t, alice, bob, messages, id)
		r.Run(t, alice, messages, "claim-btc", id)
		r.Run(t, alice, messages, "publish-btc-claimed", id)
		r.Run(t, bob, messages, "receive-btc-claimed", id)
		r.Run(t, bob, messages, "extract-secret", id)
		r.Run(t, bob, messages, "claim-blake", id)

		txid, _ := rigtest.RecordField(t, bob, id, "blake_claim_txid")
		blockHash, _ := rigtest.RecordField(t, bob, id, "blake_claim_blockhash")

		restore := r.InvalidateBlock(t, "blake", fmt.Sprint(blockHash))
		defer restore()

		// The claim transaction goes back to the mempool with the block that
		// confirmed it; the node's mempool is then permitted to forget it. The
		// durable record still holds the signed transaction, and that is what the
		// engine must rebroadcast -- not a freshly signed second claim.
		r.Mine(t, "blake", bob.Role, 1)
		r.Run(t, bob, messages, "sync-settlement", id)

		after, _ := rigtest.RecordField(t, bob, id, "blake_claim_txid")

		if after != txid {
			t.Fatalf("the recovered claim is a different transaction: %v -> %v", txid, after)
		}

		if _, err := r.NodeTry(t, "blake", "getmempoolentry", fmt.Sprint(txid)); err != nil {
			// Confirmed again is also correct; in the pool is what a rebroadcast
			// produces.
			fundingTxID, fundingVout := fundingOutpoint(t, bob, id, "blake")

			if utxo := r.Node(t, "blake", "gettxout", fundingTxID,
				fmt.Sprint(fundingVout)); strings.TrimSpace(utxo) != "" {
				t.Fatalf("the claim is neither confirmed nor in the mempool: %v", err)
			}
		}
	})

	t.Run("a final refund that is rolled back is no longer final", func(t *testing.T) {
		alice, bob, messages, id := r.SetupGoSwap(t)

		r.Run(t, alice, messages, "fund-blake", id)
		r.Run(t, alice, messages, "publish-blake-funded", id)
		r.Run(t, bob, messages, "receive-blake-funded", id)
		r.MineTo(t, "blake", alice.Role, r.FieldInt(t, alice, messages, id, "blake_timeout"))
		r.Run(t, alice, messages, "refund-blake", id)

		if state, _ := rigtest.RecordField(t, alice, id, "state"); fmt.Sprint(state) != "BLAKE_REFUNDED" {
			r.Mine(t, "blake", alice.Role, 1)
			r.Run(t, alice, messages, "refund-blake", id)
		}

		r.AssertState(t, alice, messages, id, "BLAKE_REFUNDED")

		blockHash, _ := rigtest.RecordField(t, alice, id, "blake_refund_blockhash")

		restore := r.InvalidateBlock(t, "blake", fmt.Sprint(blockHash))
		defer restore()

		report := r.ReconcileJSON(t, alice, messages, id)

		leg, _ := report["blake"].(map[string]any)

		if status, _ := leg["status"].(string); status == "spent" {
			t.Fatalf("a rolled-back refund is still reported as spent: %v", leg)
		}

		txid, _ := rigtest.RecordField(t, alice, id, "blake_refund_txid")

		restore()
		r.Mine(t, "blake", alice.Role, 1)
		r.Run(t, alice, messages, "sync-settlement", id)

		if after, _ := rigtest.RecordField(t, alice, id, "blake_refund_txid"); after != txid {
			t.Fatalf("the reappearing refund is a different transaction: %v -> %v",
				txid, after)
		}
	})

	t.Run("a disconnected funding anchor is refused", func(t *testing.T) {
		alice, bob, messages, id := r.SetupGoSwap(t)

		r.FundBothLegs(t, alice, bob, messages, id)

		// The record's funding block is no longer in the active chain, so the
		// anchor it names cannot be used to prove anything. A settlement built on
		// an anchor the chain has forgotten is refused rather than trusted.
		blockHash, _ := rigtest.RecordField(t, alice, id, "btc_funding_blockhash")

		restore := r.InvalidateBlock(t, "btc", fmt.Sprint(blockHash))
		defer restore()

		before := r.Snapshot(t, alice, id)

		r.Expect(t, alice, messages, "", "claim-btc", id)

		r.AssertSnapshotUnchanged(t, alice, id, before, "a claim against a disconnected anchor")
	})
}
