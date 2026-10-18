package swap

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/contract"
	"github.com/gjbrr/blake-swap/pkg/fund"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/state"
	"github.com/gjbrr/blake-swap/pkg/store"
)

// fundedBothLegs stores a swap whose two funding outputs are actually on the fake
// chain, which is what the decision table's "funded" answer requires.
func fundedBothLegs(
	t *testing.T, st store.Store, node *mempoolNode, id string, target state.State,
) *model.Swap {
	t.Helper()

	record := pinnedSwap(t, st, id)

	for _, ch := range []chain.Chain{chain.Blake, chain.BTC} {
		txid, vout := fundContract(t, node, record, ch)
		index := int32(vout)
		blockHash := fmtHash(node.height)

		if ch == chain.Blake {
			record.BlakeFundingTxID = &txid
			record.BlakeFundingVout = &index
			record.BlakeFundingBlockHash = &blockHash
		} else {
			record.BTCFundingTxID = &txid
			record.BTCFundingVout = &index
			record.BTCFundingBlockHash = &blockHash
		}
	}

	record.State = target

	if err := st.Save(context.Background(), store.Alice, record); err != nil {
		t.Fatal(err)
	}

	return record
}

// TestRecoveryPlanNamesTheBranches checks the plan reports, per chain, the exact
// descriptor, the outpoint and which key belongs to this participant.
//
// The sides are mirrored between the chains — on BLAKE the taker claims and the maker
// refunds, on Bitcoin the reverse — so a plan that named the same key on both chains
// would send an operator to the wrong branch.
func TestRecoveryPlanNamesTheBranches(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	node := newMempoolNode()
	engine := NewEngine(st, fund.NewMemStore(), node, node)
	engine.Identity = bobIdentity

	record := fundedBothLegs(t, st, node, "aaaa00000501", state.BTCFunded)

	plan, err := engine.RecoveryPlan(ctx, record.SwapID)

	if err != nil {
		t.Fatal(err)
	}

	if plan.Role != "bob" {
		t.Fatalf("role = %q", plan.Role)
	}

	if len(plan.Legs) != 2 {
		t.Fatalf("legs = %d", len(plan.Legs))
	}

	var blake, btc *RecoveryLeg

	for i := range plan.Legs {
		switch plan.Legs[i].Chain {
		case chain.Blake:
			blake = &plan.Legs[i]
		case chain.BTC:
			btc = &plan.Legs[i]
		}
	}

	if blake == nil || btc == nil {
		t.Fatal("a chain is missing from the plan")
	}

	// The descriptor and address must be the node's own compilation.
	expected, err := contract.Derive(ctx, node, record, chain.Blake)

	if err != nil {
		t.Fatal(err)
	}

	if blake.Descriptor != expected.Descriptor || blake.Address != expected.Address {
		t.Fatalf("blake leg = %+v", blake)
	}

	if blake.Amount != record.BlakeAmountBaseUnits {
		t.Fatalf("blake amount = %d", blake.Amount)
	}

	// Bob claims on BLAKE and refunds on Bitcoin; the keys are mirrored.
	if blake.OurClaimKey != *record.BlakeBobPub {
		t.Fatalf("blake claim key = %q", blake.OurClaimKey)
	}

	if blake.OurRefundKey != *record.BlakeAlicePub {
		t.Fatalf("blake refund key = %q", blake.OurRefundKey)
	}

	if btc.OurClaimKey != *record.BTCAlicePub {
		t.Fatalf("btc claim key = %q", btc.OurClaimKey)
	}

	if btc.OurRefundKey != *record.BTCBobPub {
		t.Fatalf("btc refund key = %q", btc.OurRefundKey)
	}

	if btc.FundingTxID != *record.BTCFundingTxID {
		t.Fatalf("btc funding = %q", btc.FundingTxID)
	}

	if blake.Solvable == nil || !*blake.Solvable {
		t.Fatalf("solvable = %v", blake.Solvable)
	}
}

// TestRecoveryPlanIsReadOnly: the plan exists to be printed, so it must not leave a
// mark on the record.
func TestRecoveryPlanIsReadOnly(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	node := newMempoolNode()
	engine := NewEngine(st, fund.NewMemStore(), node, node)
	engine.Identity = aliceIdentity

	record := fundedBothLegs(t, st, node, "aaaa00000502", state.BTCFunded)

	before, err := st.Load(ctx, record.SwapID)

	if err != nil {
		t.Fatal(err)
	}

	if _, err := engine.RecoveryPlan(ctx, record.SwapID); err != nil {
		t.Fatal(err)
	}

	after, err := st.Load(ctx, record.SwapID)

	if err != nil {
		t.Fatal(err)
	}

	if before.State != after.State || before.BlakeFundingTxID == nil || after.BlakeFundingTxID == nil ||
		*before.BlakeFundingTxID != *after.BlakeFundingTxID {
		t.Fatal("the recovery plan changed the record")
	}
}

// TestDashboardUsesTheDecisionTable: the dashboard's summary must be the decision
// table's answer, not a second opinion.
func TestDashboardUsesTheDecisionTable(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	node := newMempoolNode()
	engine := NewEngine(st, fund.NewMemStore(), node, node)
	engine.Identity = aliceIdentity

	// One swap ready to claim, one that is complete.
	ready := fundedBothLegs(t, st, node, "aaaa00000503", state.BTCFunded)

	done := fundedBothLegs(t, st, node, "aaaa00000504", state.Complete)

	// The complete swap needs both claims visible on chain to read as settled.
	blakeClaim := strings.Repeat("f1", 32)
	btcClaim := strings.Repeat("f2", 32)

	done.BlakeClaimTxID = &blakeClaim
	done.BTCCLaimTxID = &btcClaim

	if err := st.Save(ctx, store.Alice, done); err != nil {
		t.Fatal(err)
	}

	blakeSpend := claimTx(blakeClaim, *done.BlakeFundingTxID, 0, *done.SecretHex, 2)
	btcSpend := claimTx(btcClaim, *done.BTCFundingTxID, 0, *done.SecretHex, 2)

	// A claim spends its funding output, so the UTXO set no longer holds it.
	delete(node.utxos, fmt.Sprintf("%s:0", *done.BlakeFundingTxID))
	delete(node.utxos, fmt.Sprintf("%s:0", *done.BTCFundingTxID))

	for _, tx := range []*chain.Tx{blakeSpend, btcSpend} {
		tx.BlockHash = strings.Repeat("aa", 32)
		node.txs[tx.TxID] = tx
		node.blocks[tx.BlockHash] = &chain.VerboseBlock{
			Hash: tx.BlockHash, Height: 100, Tx: []chain.Tx{*tx},
		}
	}

	entries, err := engine.Dashboard(ctx)

	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 2 {
		t.Fatalf("entries = %d", len(entries))
	}

	if entries[0].SwapID != ready.SwapID || entries[1].SwapID != done.SwapID {
		t.Fatalf("order = %v/%v", entries[0].SwapID, entries[1].SwapID)
	}

	if entries[0].Outcome != "READY TO CLAIM BTC" || entries[0].Action != "claim-btc" {
		t.Fatalf("ready entry = %+v", entries[0])
	}

	if entries[1].Outcome != "COMPLETED" {
		t.Fatalf("done entry = %+v", entries[1])
	}
}

// TestResumeAllLeavesWaitingSwapsAlone: resuming a whole state tree must not act on a
// swap whose correct answer is "wait".
func TestResumeAllLeavesWaitingSwapsAlone(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	node := newMempoolNode()
	engine := NewEngine(st, fund.NewMemStore(), node, node)
	engine.Identity = bobIdentity

	// Bob, at BTC_FUNDED, waits for Alice's claim.
	record := fundedBothLegs(t, st, node, "aaaa00000505", state.BTCFunded)

	results, err := engine.ResumeAll(ctx)

	if err != nil {
		t.Fatal(err)
	}

	if len(results) != 1 {
		t.Fatalf("results = %d", len(results))
	}

	if results[0].Performed {
		t.Fatal("resume-all acted on a swap that should wait")
	}

	if results[0].Action != "wait" || results[0].Class != "waiting" {
		t.Fatalf("result = %+v", results[0])
	}

	persisted, _ := st.Load(ctx, record.SwapID)

	if persisted.State != state.BTCFunded {
		t.Fatalf("state = %s", persisted.State)
	}
}
