//go:build !js

package swap_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/internal/rigtest"
	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/htlc"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// The settlement matrices are identical in shape: four economic operations, each
// killed at one of three durability boundaries, and the same question asked after
// every death -- can the participant come back and finish the exact transaction it
// started, without ever creating a second HTLC spend?
//
// This file is the shared harness. `settlement_crash_matrix_test.go` and
// `settlement_fault_matrix_test.go` are the two suites' own assertions on top of
// it.

// settlementOperation is one economic settlement the matrices exercise.
type settlementOperation struct {
	name string
	// chain and branch are the settlement's own coordinates.
	chain  chain.Chain
	branch htlc.Branch
	// command is the CLI verb that performs it.
	command string
	// actor is "alice" or "bob": whoever is allowed to perform it.
	actor string
}

// prefix is the record's field prefix for this settlement.
func (o settlementOperation) prefix() string {
	slug := "btc"

	if o.chain == chain.Blake {
		slug = "blake"
	}

	return slug + "_" + string(o.branch)
}

// operations is the reference's four operations, in its order.
func operations() []settlementOperation {
	return []settlementOperation{
		{name: "BTC CLAIM", chain: chain.BTC, branch: htlc.Claim, command: "claim-btc", actor: "alice"},
		{name: "BLAKE CLAIM", chain: chain.Blake, branch: htlc.Claim, command: "claim-blake", actor: "bob"},
		{name: "BTC REFUND", chain: chain.BTC, branch: htlc.Refund, command: "refund-btc", actor: "bob"},
		{name: "BLAKE REFUND", chain: chain.Blake, branch: htlc.Refund, command: "refund-blake", actor: "alice"},
	}
}

// boundaries is the reference's three durability boundaries, as failpoint names.
func boundaries(o settlementOperation) map[string]string {
	return map[string]string{
		swap.BoundaryCandidateSave: swap.FailpointName(o.chain, o.branch, swap.BoundaryCandidateSave),
		swap.BoundaryBroadcast:     swap.FailpointName(o.chain, o.branch, swap.BoundaryBroadcast),
		swap.BoundaryConfirmation:  swap.FailpointName(o.chain, o.branch, swap.BoundaryConfirmation),
	}
}

// settlementScene is one prepared swap with one operation ready to run.
type settlementScene struct {
	r     *rigtest.Rig
	alice *rigtest.Side
	bob   *rigtest.Side
	// actor is whichever participant performs this operation.
	actor    *rigtest.Side
	messages string
	id       string
	// sourceState is the durable state before the crash. It is read rather than
	// hardcoded: what the matrices assert is that a crash does not move it, not
	// that a particular path produced it.
	sourceState string
}

// prepare builds the state each operation is allowed to start from.
func prepare(t *testing.T, r *rigtest.Rig, o settlementOperation) *settlementScene {
	t.Helper()

	alice, bob, messages, id := r.SetupGoSwap(t)

	actor := alice

	if o.actor == "bob" {
		actor = bob
	}

	switch {
	case o.branch == htlc.Claim && o.chain == chain.BTC:
		r.FundBothLegs(t, alice, bob, messages, id)

	case o.branch == htlc.Claim:
		// Bob can only claim BLAKE with the secret Alice revealed on Bitcoin.
		r.FundBothLegs(t, alice, bob, messages, id)
		r.Run(t, alice, messages, "claim-btc", id)
		r.Run(t, alice, messages, "publish-btc-claimed", id)
		r.Run(t, bob, messages, "receive-btc-claimed", id)
		r.Run(t, bob, messages, "extract-secret", id)

	case o.chain == chain.BTC:
		r.FundBothLegs(t, alice, bob, messages, id)
		r.MineTo(t, "btc", bob.Role, r.FieldInt(t, alice, messages, id, "btc_timeout"))

	default:
		// Alice's abort-before-Bitcoin-funding refund: only the BLAKE leg exists,
		// which keeps the BLAKE refund independent of the Bitcoin refund path.
		r.Run(t, alice, messages, "fund-blake", id)
		r.Run(t, alice, messages, "publish-blake-funded", id)
		r.Run(t, bob, messages, "receive-blake-funded", id)
		r.MineTo(t, "blake", alice.Role, r.FieldInt(t, alice, messages, id, "blake_timeout"))
	}

	state, _ := rigtest.RecordField(t, actor, id, "state")

	return &settlementScene{
		r: r, alice: alice, bob: bob, actor: actor, messages: messages, id: id,
		sourceState: fmt.Sprint(state),
	}
}

// crash kills the operation at one boundary and returns its output.
func (s *settlementScene) crash(t *testing.T, o settlementOperation, point string) string {
	t.Helper()

	output, exit, err := s.r.Exec(t, s.actor, s.messages,
		[]string{swap.FailpointEnv + "=" + point}, o.command, s.id)

	if err != nil {
		t.Fatalf("%s %s: %v", o.name, point, err)
	}

	if exit != swap.FailpointExit {
		t.Fatalf("%s %s: exit = %d, want %d\n%s",
			o.name, point, exit, swap.FailpointExit, output)
	}

	return output
}

// durable reads the record fields one settlement owns.
func (s *settlementScene) durable(t *testing.T, o settlementOperation) (txid, blockHash, hexTx string) {
	t.Helper()

	record := rigtest.Record(t, s.actor, s.id)

	text := func(key string) string {
		value, present := record[o.prefix()+"_"+key]

		if !present || value == nil {
			return ""
		}

		return fmt.Sprint(value)
	}

	return text("txid"), text("blockhash"), text("hex")
}

// funding is the HTLC outpoint being spent.
func (s *settlementScene) funding(t *testing.T, o settlementOperation) (string, int64) {
	t.Helper()

	slug := "btc"

	if o.chain == chain.Blake {
		slug = "blake"
	}

	txid, _ := rigtest.RecordField(t, s.actor, s.id, slug+"_funding_txid")
	vout, _ := rigtest.RecordField(t, s.actor, s.id, slug+"_funding_vout")

	number, ok := vout.(float64)

	if !ok {
		t.Fatalf("%s funding vout is not a number: %v", o.name, vout)
	}

	return fmt.Sprint(txid), int64(number)
}

// spenders lists the mempool transactions spending one outpoint.
//
// The matrices care about this number being 0 or 1 and never 2: a second spender
// of the same HTLC is a second economic action, whichever one confirms.
func (s *settlementScene) spenders(t *testing.T, o settlementOperation) []string {
	t.Helper()

	fundingTxID, fundingVout := s.funding(t, o)

	chainName := "btc"

	if o.chain == chain.Blake {
		chainName = "blake"
	}

	var found []string

	for _, txid := range s.r.Mempool(t, chainName) {
		raw := s.r.Node(t, chainName, "getrawtransaction", txid, "true")

		var tx struct {
			Vin []struct {
				TxID string `json:"txid"`
				Vout int64  `json:"vout"`
			} `json:"vin"`
		}

		if err := json.Unmarshal([]byte(raw), &tx); err != nil {
			t.Fatalf("decoding %s: %v", txid, err)
		}

		for _, input := range tx.Vin {
			if input.TxID == fundingTxID && input.Vout == fundingVout {
				found = append(found, txid)

				break
			}
		}
	}

	return found
}

// inMempool reports whether one transaction is in the mempool.
func (s *settlementScene) inMempool(t *testing.T, o settlementOperation, txid string) bool {
	t.Helper()

	chainName := "btc"

	if o.chain == chain.Blake {
		chainName = "blake"
	}

	_, err := s.r.NodeTry(t, chainName, "getmempoolentry", txid)

	return err == nil
}

// confirmedBlock is the active-chain block one settlement confirmed in, read from
// the signing wallet because the nodes do not run with a transaction index.
func (s *settlementScene) confirmedBlock(t *testing.T, o settlementOperation, txid string) string {
	t.Helper()

	chainName := "btc"

	if o.chain == chain.Blake {
		chainName = "blake"
	}

	raw := s.r.Node(t, chainName, "-rpcwallet="+s.actor.Role, "gettransaction", txid)

	var walletTx struct {
		BlockHash     string `json:"blockhash"`
		Confirmations int64  `json:"confirmations"`
	}

	if err := json.Unmarshal([]byte(raw), &walletTx); err != nil {
		t.Fatalf("%s: decoding the wallet's transaction: %v", o.name, err)
	}

	if walletTx.BlockHash == "" || walletTx.Confirmations < 1 {
		t.Fatalf("%s: the settlement is not confirmed in an active block", o.name)
	}

	return walletTx.BlockHash
}

// assertSourceStateUnchanged requires the crash to have moved no durable state.
func (s *settlementScene) assertSourceStateUnchanged(t *testing.T, o settlementOperation, boundary string) {
	t.Helper()

	state, _ := rigtest.RecordField(t, s.actor, s.id, "state")

	if got := fmt.Sprint(state); got != s.sourceState {
		t.Fatalf("%s/%s: the crash advanced durable state to %s, from %s",
			o.name, boundary, got, s.sourceState)
	}
}

// mineAndFinish produces the confirming block and re-runs the command with
// automatic mining off, so the confirming block is the test's and not the engine's.
func (s *settlementScene) mineAndFinish(t *testing.T, o settlementOperation, txid string) {
	t.Helper()

	chainName := "btc"

	if o.chain == chain.Blake {
		chainName = "blake"
	}

	// A restart that already saw a confirmed spend has advanced the record; the
	// reference splits those cases too, and asking for the settlement again once
	// it is final is correctly refused.
	state, _ := rigtest.RecordField(t, s.actor, s.id, "state")

	if fmt.Sprint(state) != s.sourceState {
		txidNow, blockHashNow, _ := s.durable(t, o)

		if txidNow != txid {
			t.Fatalf("%s: the recorded transaction changed: %s -> %s",
				o.name, txid, txidNow)
		}

		if blockHashNow == "" {
			t.Fatalf("%s: an advanced record carries no block hash", o.name)
		}

		if want := s.confirmedBlock(t, o, txid); blockHashNow != want {
			t.Fatalf("%s: recorded block %s is not the active-chain block %s",
				o.name, blockHashNow, want)
		}

		assertHTLCSpent(t, s, o, chainName)

		return
	}

	s.r.Mine(t, chainName, s.actor.Role, 1)

	output, exit, err := s.r.Exec(t, s.actor, s.messages,
		[]string{"BLAKE_SWAP_AUTO_MINE=0"}, o.command, s.id)

	if err != nil {
		t.Fatalf("%s: finishing: %v", o.name, err)
	}

	if exit != 0 {
		t.Fatalf("%s: finishing failed with exit %d:\n%s", o.name, exit, output)
	}

	after, blockHash, _ := s.durable(t, o)

	if after != txid {
		t.Fatalf("%s: finishing replaced the canonical txid %s with %s", o.name, txid, after)
	}

	if blockHash == "" {
		t.Fatalf("%s: finishing recorded no block hash", o.name)
	}

	if want := s.confirmedBlock(t, o, txid); blockHash != want {
		t.Fatalf("%s: recorded block %s is not the active-chain block %s",
			o.name, blockHash, want)
	}

	final, _ := rigtest.RecordField(t, s.actor, s.id, "state")

	if got := fmt.Sprint(final); got == s.sourceState {
		t.Fatalf("%s: the settlement completed without advancing the state", o.name)
	}

	assertHTLCSpent(t, s, o, chainName)
}

// assertHTLCSpent requires the HTLC output to be gone.
func assertHTLCSpent(t *testing.T, s *settlementScene, o settlementOperation, chainName string) {
	t.Helper()

	fundingTxID, fundingVout := s.funding(t, o)

	if utxo := s.r.Node(t, chainName, "gettxout", fundingTxID,
		fmt.Sprint(fundingVout)); strings.TrimSpace(utxo) != "" {
		t.Fatalf("%s: the HTLC is unspent after the settlement", o.name)
	}
}

// decodeJSON decodes one node answer.
func decodeJSON(raw string, target any) error {
	return json.Unmarshal([]byte(raw), target)
}
