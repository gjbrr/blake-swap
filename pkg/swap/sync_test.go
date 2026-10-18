package swap

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/contract"
	"github.com/gjbrr/blake-swap/pkg/fund"
	"github.com/gjbrr/blake-swap/pkg/state"
	"github.com/gjbrr/blake-swap/pkg/store"
)

// TestSyncFundingRecoversBothStages: one invocation may recover more than one missed
// event, which is exactly the state an operator reaches for this command with.
func TestSyncFundingRecoversBothStages(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	node := newMempoolNode()
	engine := NewEngine(st, fund.NewMemStore(), node, node)
	engine.Identity = aliceIdentity

	// Alice's record still says OFFERED, but both HTLCs are on the chain.
	record := pinnedSwap(t, st, "aaaa00000601")

	fundContract(t, node, record, chain.Blake)
	fundContract(t, node, record, chain.BTC)

	result, err := engine.SyncFunding(ctx, record.SwapID)

	if err != nil {
		t.Fatal(err)
	}

	if result.Start != state.Offered || result.End != state.BTCFunded {
		t.Fatalf("start %s end %s", result.Start, result.End)
	}

	if len(result.Recovered) != 2 {
		t.Fatalf("recovered = %v", result.Recovered)
	}

	persisted, _ := st.Load(ctx, record.SwapID)

	if persisted.State != state.BTCFunded {
		t.Fatalf("state = %s", persisted.State)
	}

	if persisted.BlakeFundingBlockHash == nil || persisted.BTCFundingBlockHash == nil {
		t.Fatal("a recovered funding has no block anchor")
	}
}

// TestSyncFundingReturnsWhenNothingIsOnChain: a swap with no confirmed funding is not an
// error, it is a swap that has not been funded yet.
func TestSyncFundingReturnsWhenNothingIsOnChain(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	node := newMempoolNode()
	engine := NewEngine(st, fund.NewMemStore(), node, node)
	engine.Identity = aliceIdentity

	record := pinnedSwap(t, st, "aaaa00000602")

	result, err := engine.SyncFunding(ctx, record.SwapID)

	if err != nil {
		t.Fatal(err)
	}

	if result.Start != state.Offered || result.End != state.Offered {
		t.Fatalf("start %s end %s", result.Start, result.End)
	}

	if len(result.Recovered) != 0 {
		t.Fatalf("recovered = %v", result.Recovered)
	}
}

// TestSyncFundingFromTheWatchWallet covers the lost-announcement case: the record never
// learned the outpoint, but this participant's watch-only wallet did.
func TestSyncFundingFromTheWatchWallet(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	node := newMempoolNode()
	engine := NewEngine(st, fund.NewMemStore(), node, node)
	engine.Identity = bobIdentity

	record := pinnedSwap(t, st, "aaaa00000603")

	// The output is on the chain, and the watch wallet knows the payment.
	txid, _ := fundContract(t, node, record, chain.Blake)

	derived, err := contract.Derive(ctx, node, record, chain.Blake)

	if err != nil {
		t.Fatal(err)
	}

	// The wallet records the payment it made, which is what a lost announcement leaves
	// behind: the chain has the output and the wallet knows the transaction.
	node.walletEntries = []chain.WalletEntry{{
		Address:  derived.Address,
		Category: "send",
		TxID:     txid,
		Amount:   amount.Coin("-" + amount.Sats(record.BlakeAmountBaseUnits).Coin().String()),
	}}
	node.walletTxs[txid] = &chain.WalletTx{
		TxID: txid, Hex: txid, BlockHash: fmtHash(node.height),
	}

	result, err := engine.SyncFunding(ctx, record.SwapID)

	if err != nil {
		t.Fatal(err)
	}

	if result.End != state.BlakeFunded {
		t.Fatalf("end = %s", result.End)
	}

	persisted, _ := st.Load(ctx, record.SwapID)

	if persisted.BlakeFundingTxID == nil || *persisted.BlakeFundingTxID != txid {
		t.Fatal("the watch wallet's payment was not adopted")
	}
}

// TestSyncSettlementRecordsAConfirmedClaim: the chain proves the branch, and the record
// follows it.
func TestSyncSettlementRecordsAConfirmedClaim(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	node := newMempoolNode()
	engine := NewEngine(st, fund.NewMemStore(), node, node)
	engine.Identity = bobIdentity

	record := fundedBothLegs(t, st, node, "aaaa00000604", state.BTCFunded)

	// Alice's Bitcoin claim is confirmed on chain.
	claimID := strings.Repeat("a1", 32)
	claim := claimTx(claimID, *record.BTCFundingTxID, 0, *record.SecretHex, 2)
	// The spender is mined after the funding, which is what the scan range assumes.
	node.height++

	claim.BlockHash = fmtHash(node.height)

	node.txs[claimID] = claim
	node.blocks[claim.BlockHash] = &chain.VerboseBlock{
		Hash: claim.BlockHash, Height: node.height, Tx: []chain.Tx{*claim},
	}

	delete(node.utxos, outpointKey(record.BTCFundingTxID, 0))

	result, err := engine.SyncSettlement(ctx, record.SwapID)

	if err != nil {
		t.Fatal(err)
	}

	if result.End != state.BTCClaimed {
		t.Fatalf("end = %s", result.End)
	}

	persisted, _ := st.Load(ctx, record.SwapID)

	if persisted.BTCCLaimTxID == nil || *persisted.BTCCLaimTxID != claimID {
		t.Fatal("the claim was not recorded")
	}
}

// TestSyncSettlementRefusesAnInconsistentBranch: state says one thing, the chain proves
// another, and the record must not follow the chain into an impossible state.
func TestSyncSettlementRefusesAnInconsistentBranch(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	node := newMempoolNode()
	engine := NewEngine(st, fund.NewMemStore(), node, node)
	engine.Identity = aliceIdentity

	// Alice's record is BLAKE_FUNDED, which permits only a BLAKE refund.
	record := fundedBothLegs(t, st, node, "aaaa00000605", state.BlakeFunded)

	// The chain shows a claim of the BLAKE HTLC instead.
	claimID := strings.Repeat("a2", 32)
	claim := claimTx(claimID, *record.BlakeFundingTxID, 0, *record.SecretHex, 2)
	node.height++

	claim.BlockHash = fmtHash(node.height)

	node.txs[claimID] = claim
	node.blocks[claim.BlockHash] = &chain.VerboseBlock{
		Hash: claim.BlockHash, Height: node.height, Tx: []chain.Tx{*claim},
	}

	delete(node.utxos, outpointKey(record.BlakeFundingTxID, 0))

	_, err := engine.SyncSettlement(ctx, record.SwapID)

	if err == nil || !strings.Contains(err.Error(), "inconsistent with local protocol state") {
		t.Fatalf("err = %v", err)
	}

	persisted, _ := st.Load(ctx, record.SwapID)

	if persisted.State != state.BlakeFunded {
		t.Fatalf("a refused reconciliation changed the state to %s", persisted.State)
	}
}

// TestSyncSettlementRefusesTheIntermediateState: SECRET_REVEALED must not survive on
// disk, because it is a state only a crash inside a transition can produce.
func TestSyncSettlementRefusesTheIntermediateState(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	node := newMempoolNode()
	engine := NewEngine(st, fund.NewMemStore(), node, node)
	engine.Identity = aliceIdentity

	record := pinnedSwap(t, st, "aaaa00000606")
	record.State = state.SecretRevealed

	if err := st.Save(ctx, store.Alice, record); err != nil {
		t.Fatal(err)
	}

	if _, err := engine.SyncSettlement(ctx, record.SwapID); err == nil ||
		!strings.Contains(err.Error(), "SECRET_REVEALED") {
		t.Fatalf("err = %v", err)
	}
}

// outpointKey renders the fake node's UTXO map key.
func outpointKey(txid *string, vout int64) string {
	return fmt.Sprintf("%s:%d", *txid, vout)
}
