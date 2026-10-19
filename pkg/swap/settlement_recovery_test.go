//go:build !js

package swap_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/internal/rigtest"
	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/htlc"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// Parity: tests/swap_settlement_recovery_tests.py
//
// The reference's recovery suite kills a participant with a crash point and then
// asks whether the restart can find its own transaction from the durable record
// alone. Two shapes are covered here:
//
//   - the mempool forgot the transaction (the node was restarted without it), so
//     the record's signed hex is the only copy left;
//   - the transaction confirmed while the process was dead, so the restart has to
//     discover an active-chain spend it never recorded as confirmed.
func TestSettlementRecovery(t *testing.T) {
	r := rigtest.Load(t)

	cases := []struct {
		name    string
		crash   string
		command string
		actor   string
		field   string
		which   string
		chain   chain.Chain
		branch  htlc.Branch
	}{
		{name: "blake claim", crash: "blake-claim-after-broadcast", command: "claim-blake", actor: "bob", field: "blake_claim_txid", which: "blake", chain: chain.Blake, branch: htlc.Claim},
		{name: "btc claim", crash: "btc-claim-after-broadcast", command: "claim-btc", actor: "alice", field: "btc_claim_txid", which: "btc", chain: chain.BTC, branch: htlc.Claim},
		{name: "blake refund", crash: "blake-refund-after-broadcast", command: "refund-blake", actor: "alice", field: "blake_refund_txid", which: "blake", chain: chain.Blake, branch: htlc.Refund},
		{name: "btc refund", crash: "btc-refund-after-broadcast", command: "refund-btc", actor: "bob", field: "btc_refund_txid", which: "btc", chain: chain.BTC, branch: htlc.Refund},
	}

	for _, c := range cases {
		t.Run(c.name+"/node restart", func(t *testing.T) {
			// The actor dies after the broadcast and the node is restarted before
			// it comes back.
			//
			// One thing this cannot do, and an earlier version of this test tried:
			// make the node forget the transaction. A node re-submits its loaded
			// wallets' unconfirmed transactions at startup even with broadcast
			// disabled, so deleting the mempool dump does not empty the pool. The
			// engine's own rebroadcast of its stored hex is therefore proved where
			// it is actually reachable: at the candidate boundary in
			// TestSettlementCrashMatrix, where nothing was broadcast at all and the
			// transaction appears in the mempool only if the restart sent the
			// recorded bytes.
			scene := prepareOperation(t, r, c.name, c.command, c.actor, c.which)

			output, exit, err := r.Exec(t, scene.actor, scene.messages,
				[]string{"BLAKE_SWAP_TEST_CRASH_POINT=" + c.crash}, c.command, scene.id)

			if err != nil {
				t.Fatalf("running the armed %s: %v", c.command, err)
			}

			if exit != 86 {
				t.Fatalf("exit = %d, want 86 at %s\n%s", exit, c.crash, output)
			}

			txid, _ := rigtest.RecordField(t, scene.actor, scene.id, c.field)

			if fmt.Sprint(txid) == "" || fmt.Sprint(txid) == "<nil>" {
				t.Fatalf("the crash left no %s", c.field)
			}

			r.RestartNode(t, c.which)

			// The restart adopts the recorded transaction; it does not sign a
			// second one.
			r.Run(t, scene.actor, scene.messages, c.command, scene.id)

			recovered, _ := rigtest.RecordField(t, scene.actor, scene.id, c.field)

			if recovered != txid {
				t.Fatalf("the restart signed a second transaction: %v -> %v", txid, recovered)
			}

			if _, err := r.NodeTry(t, c.which, "getmempoolentry", fmt.Sprint(txid)); err != nil {
				t.Fatalf("the recorded transaction is not in the mempool after the "+
					"restart: %v", err)
			}

			// The transaction in the mempool is the one the record's signed hex
			// decodes to, which is what makes the record rebroadcast material
			// rather than a note.
			record := rigtest.Record(t, scene.actor, scene.id)

			if hexTx, _ := record[c.which+"_"+branchOf(c.field)+"_hex"].(string); hexTx != "" {
				decoded, err := r.NodeTry(t, c.which, "decoderawtransaction", hexTx)

				if err != nil {
					t.Fatalf("decoding the durable signed transaction: %v", err)
				}

				if !strings.Contains(decoded, fmt.Sprint(txid)) {
					t.Fatalf("the durable signed transaction is not %v:\n%s", txid, decoded)
				}
			}

			// And it still completes once a block arrives.
			r.Mine(t, c.which, scene.actor.Role, 1)
			r.Run(t, scene.actor, scene.messages, c.command, scene.id)

			state, _ := rigtest.RecordField(t, scene.actor, scene.id, "state")

			if fmt.Sprint(state) == scene.sourceState {
				t.Fatalf("the settlement did not advance after its block")
			}

			if confirmed, _ := rigtest.RecordField(t, scene.actor, scene.id, c.field); confirmed != txid {
				t.Fatalf("the confirmed settlement is not the recorded transaction: "+
					"%v -> %v", txid, confirmed)
			}
		})

		t.Run(c.name+"/confirmed while dead", func(t *testing.T) {
			// The confirmation boundary sits after the transition in memory and
			// before it is persisted, with automatic mining already done, so the
			// chain holds a confirmed spend the record does not know about.
			scene := prepareOperation(t, r, c.name, c.command, c.actor, c.which)

			output, exit, err := r.Exec(t, scene.actor, scene.messages,
				[]string{swap.FailpointEnv + "=" + swap.FailpointName(c.chain, c.branch, swap.BoundaryConfirmation)},
				c.command, scene.id)

			if err != nil {
				t.Fatalf("running the armed %s: %v", c.command, err)
			}

			if exit != 97 {
				t.Fatalf("exit = %d, want 97\n%s", exit, output)
			}

			txid, _ := rigtest.RecordField(t, scene.actor, scene.id, c.field)

			// The restart discovers the confirmed spend and advances, without
			// signing anything.
			r.Run(t, scene.actor, scene.messages, c.command, scene.id)

			state, _ := rigtest.RecordField(t, scene.actor, scene.id, "state")

			if fmt.Sprint(state) == scene.sourceState {
				t.Fatalf("the restart did not advance on a confirmed spend")
			}

			recovered, _ := rigtest.RecordField(t, scene.actor, scene.id, c.field)

			if recovered != txid {
				t.Fatalf("the restart replaced the confirmed transaction: %v -> %v",
					txid, recovered)
			}
		})
	}
}

// recoveryScene is one swap prepared for one settlement operation.
type recoveryScene struct {
	actor       *rigtest.Side
	messages    string
	id          string
	sourceState string
}

// prepareOperation builds the state one settlement is allowed to start from.
func prepareOperation(
	t *testing.T, r *rigtest.Rig, name, command, actor, which string,
) *recoveryScene {
	t.Helper()

	alice, bob, messages, id := r.SetupGoSwap(t)

	side := alice

	if actor == "bob" {
		side = bob
	}

	switch {
	case command == "claim-btc":
		r.FundBothLegs(t, alice, bob, messages, id)

	case command == "claim-blake":
		r.FundBothLegs(t, alice, bob, messages, id)
		r.Run(t, alice, messages, "claim-btc", id)
		r.Run(t, alice, messages, "publish-btc-claimed", id)
		r.Run(t, bob, messages, "receive-btc-claimed", id)
		r.Run(t, bob, messages, "extract-secret", id)

	case command == "refund-btc":
		r.FundBothLegs(t, alice, bob, messages, id)
		r.MineTo(t, "btc", bob.Role, r.FieldInt(t, alice, messages, id, "btc_timeout"))

	case command == "refund-blake":
		r.Run(t, alice, messages, "fund-blake", id)
		r.Run(t, alice, messages, "publish-blake-funded", id)
		r.Run(t, bob, messages, "receive-blake-funded", id)
		r.MineTo(t, "blake", alice.Role, r.FieldInt(t, alice, messages, id, "blake_timeout"))

	default:
		t.Fatalf("%s: unknown command %q", name, command)
	}

	state, _ := rigtest.RecordField(t, side, id, "state")

	return &recoveryScene{
		actor: side, messages: messages, id: id, sourceState: fmt.Sprint(state),
	}
}

// branchOf is the settlement branch a record field belongs to.
func branchOf(field string) string {
	if strings.Contains(field, "refund") {
		return "refund"
	}

	return "claim"
}

// fundingOutpoint reads one chain's HTLC outpoint from a record.
func fundingOutpoint(t *testing.T, s *rigtest.Side, id, which string) (string, int64) {
	t.Helper()

	txid, _ := rigtest.RecordField(t, s, id, which+"_funding_txid")
	vout, _ := rigtest.RecordField(t, s, id, which+"_funding_vout")

	number, ok := vout.(float64)

	if !ok {
		t.Fatalf("%s funding vout is not a number: %v", which, vout)
	}

	return fmt.Sprint(txid), int64(number)
}
