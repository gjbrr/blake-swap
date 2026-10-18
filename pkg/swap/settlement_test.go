package swap

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"crypto/sha256"
	"encoding/hex"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/contract"
	"github.com/gjbrr/blake-swap/pkg/fund"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/state"
	"github.com/gjbrr/blake-swap/pkg/store"
)

// mempoolNode serves exactly the reads recovery needs, and panics on anything
// else — so a test that expects "no second transaction" fails loudly if the
// command decides to build one.
type mempoolNode struct {
	Node

	height  int64
	mempool map[string]bool
	txs     map[string]*chain.Tx
	blocks  map[string]*chain.VerboseBlock
	utxos   map[string]*chain.TxOut

	// walletEntries and walletTxs are what any bound wallet reports.
	walletEntries []chain.WalletEntry
	walletTxs     map[string]*chain.WalletTx
}

func newMempoolNode() *mempoolNode {
	return &mempoolNode{
		Node:      nil,
		height:    100,
		mempool:   map[string]bool{},
		txs:       map[string]*chain.Tx{},
		blocks:    map[string]*chain.VerboseBlock{},
		utxos:     map[string]*chain.TxOut{},
		walletTxs: map[string]*chain.WalletTx{},
	}
}

func (m *mempoolNode) Height(context.Context) (int64, error) { return m.height, nil }

// Bind answers with a wallet that can see the HTLC, which is all the recovery plan
// asks about.
func (m *mempoolNode) Bind(name string) chain.Wallet {
	if m.walletEntries == nil {
		m.walletEntries = nil
	}

	if m.walletTxs == nil {
		m.walletTxs = map[string]*chain.WalletTx{}
	}

	return &stubWallet{name: name, node: m}
}

// ListWallets reports the two participants.
func (m *mempoolNode) ListWallets(context.Context) ([]string, error) {
	return []string{"alice", "bob"}, nil
}

// stubWallet is a wallet with no signing ability: it knows addresses and the
// transactions the node recorded, which is all the recovery paths read from it.
type stubWallet struct {
	chain.Wallet

	name string
	node *mempoolNode
}

func (w *stubWallet) Name() string { return w.name }

func (w *stubWallet) AddressInfo(_ context.Context, address string) (*chain.AddressInfo, error) {
	return &chain.AddressInfo{Address: address, Solvable: true}, nil
}

func (w *stubWallet) ListTransactions(context.Context, int64, int64) ([]chain.WalletEntry, error) {
	return w.node.walletEntries, nil
}

func (w *stubWallet) Decode(ctx context.Context, hexTx string) (*chain.Tx, error) {
	return w.node.DecodeRaw(ctx, hexTx)
}

func (w *stubWallet) Tx(_ context.Context, txid string) (*chain.WalletTx, error) {
	tx, ok := w.node.walletTxs[txid]

	if !ok {
		return nil, fmt.Errorf("Invalid or non-wallet transaction id")
	}

	return tx, nil
}

func (m *mempoolNode) BlockHash(_ context.Context, height int64) (string, error) {
	return fmt.Sprintf("%064x", height), nil
}

func (m *mempoolNode) InMempool(_ context.Context, txid string) (bool, error) {
	return m.mempool[txid], nil
}

func (m *mempoolNode) RawTx(_ context.Context, txid, _ string) (*chain.Tx, error) {
	tx, ok := m.txs[txid]

	if !ok {
		return nil, fmt.Errorf("No such transaction")
	}

	return tx, nil
}

func (m *mempoolNode) TxOut(_ context.Context, txid string, vout int64) (*chain.TxOut, error) {
	out, ok := m.utxos[fmt.Sprintf("%s:%d", txid, vout)]

	if !ok {
		return nil, nil
	}

	return out, nil
}

func (m *mempoolNode) BlockTxs(_ context.Context, hash string) (*chain.VerboseBlock, error) {
	if block, ok := m.blocks[hash]; ok {
		return block, nil
	}

	return &chain.VerboseBlock{Hash: hash}, nil
}

func (m *mempoolNode) DescriptorInfo(_ context.Context, body string) (*chain.DescriptorInfo, error) {
	sum := sha256.Sum256([]byte(body))

	return &chain.DescriptorInfo{
		Descriptor: body + "#" + hex.EncodeToString(sum[:4]),
		Checksum:   hex.EncodeToString(sum[:4]),
		IsSolvable: true,
	}, nil
}

func (m *mempoolNode) DeriveAddresses(_ context.Context, descriptor string) ([]string, error) {
	sum := sha256.Sum256([]byte(descriptor))

	return []string{"bcrt1q" + hex.EncodeToString(sum[:10])}, nil
}

func (m *mempoolNode) DecodeRaw(_ context.Context, hexTx string) (*chain.Tx, error) {
	tx, ok := m.txs[hexTx]

	if !ok {
		return nil, fmt.Errorf("TX decode failed")
	}

	return tx, nil
}

// settlementSwap builds a stored swap that is funded on both chains.
func settlementSwap(t *testing.T, st store.Store, id string, swapState state.State) *model.Swap {
	t.Helper()

	record := pinnedSwap(t, st, id)

	blakeTxID := strings.Repeat("b1", 32)
	btcTxID := strings.Repeat("b2", 32)
	vout := int32(0)
	blockHash := strings.Repeat("c1", 32)

	record.State = swapState
	record.BlakeFundingTxID = &blakeTxID
	record.BlakeFundingVout = &vout
	record.BlakeFundingBlockHash = &blockHash
	record.BTCFundingTxID = &btcTxID
	record.BTCFundingVout = &vout
	record.BTCFundingBlockHash = &blockHash

	if err := st.Save(context.Background(), store.Alice, record); err != nil {
		t.Fatal(err)
	}

	return record
}

// amountCoin renders a base-unit amount as the node would report it.
func amountCoin(baseUnits int64) amount.Coin {
	return amount.Coin(amount.Sats(baseUnits).Coin().String())
}

// claimTx builds a transaction that spends the given HTLC with a claim witness.
func claimTx(txid, fundingTxID string, vout int64, preimage string, confirmations int64) *chain.Tx {
	return &chain.Tx{
		TxID:          txid,
		Confirmations: confirmations,
		Vin: []chain.Vin{{
			TxID:    fundingTxID,
			Vout:    vout,
			Witness: []string{preimage, "30", "01", "63"},
		}},
	}
}

// refundTx builds a transaction that spends the given HTLC with a refund witness.
func refundTx(txid, fundingTxID string, vout int64, confirmations int64) *chain.Tx {
	return &chain.Tx{
		TxID:          txid,
		Confirmations: confirmations,
		Vin: []chain.Vin{{
			TxID:    fundingTxID,
			Vout:    vout,
			Witness: []string{"30", "", "63"},
		}},
	}
}

// TestClaimBTCPendingIsAdoptedNotRebuilt is the recovery rule: a recorded
// settlement in the mempool is this participant's own work, and rebuilding it
// would race itself.
func TestClaimBTCPendingIsAdoptedNotRebuilt(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	node := newMempoolNode()
	engine := NewEngine(st, fund.NewMemStore(), nil, node)
	engine.Identity = aliceIdentity

	record := settlementSwap(t, st, "aaaa00000201", state.BTCFunded)

	claimID := strings.Repeat("cc", 32)

	node.mempool[claimID] = true
	node.txs[claimID] = claimTx(claimID, *record.BTCFundingTxID, 0, *record.SecretHex, 0)

	record.BTCCLaimTxID = &claimID

	if err := st.Save(ctx, store.Alice, record); err != nil {
		t.Fatal(err)
	}

	result, err := engine.ClaimBTC(ctx, record.SwapID)

	if err != nil {
		t.Fatal(err)
	}

	if !result.Pending || result.TxID != claimID {
		t.Fatalf("result = %+v", result)
	}

	// An unconfirmed settlement must not advance durable protocol state.
	persisted, _ := st.Load(ctx, record.SwapID)

	if persisted.State != state.BTCFunded {
		t.Fatalf("state = %s", persisted.State)
	}
}

// TestClaimBTCRefusesARecordedSettlementOnTheWrongBranch: a recorded claim that
// used the refund branch is a contradiction, not a recovery.
func TestClaimBTCRefusesARecordedSettlementOnTheWrongBranch(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	node := newMempoolNode()
	engine := NewEngine(st, fund.NewMemStore(), nil, node)
	engine.Identity = aliceIdentity

	record := settlementSwap(t, st, "aaaa00000202", state.BTCFunded)

	wrongID := strings.Repeat("dd", 32)

	node.mempool[wrongID] = true
	node.txs[wrongID] = refundTx(wrongID, *record.BTCFundingTxID, 0, 0)

	record.BTCCLaimTxID = &wrongID

	if err := st.Save(ctx, store.Alice, record); err != nil {
		t.Fatal(err)
	}

	_, err := engine.ClaimBTC(ctx, record.SwapID)

	if err == nil || !strings.Contains(err.Error(), "uses refund branch") {
		t.Fatalf("err = %v", err)
	}
}

// TestSettlementCommandsGateOnRoleAndState is the cheap half of the protocol:
// the wrong side or the wrong stage never reaches a chain.
func TestSettlementCommandsGateOnRoleAndState(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	node := newMempoolNode()
	engine := NewEngine(st, fund.NewMemStore(), node, node)
	engine.Identity = aliceIdentity

	record := settlementSwap(t, st, "aaaa00000203", state.Offered)

	if _, err := engine.ClaimBTC(ctx, record.SwapID); err == nil ||
		!strings.Contains(err.Error(), "requires state BTC_FUNDED") {
		t.Fatalf("err = %v", err)
	}

	// Alice may not claim BLAKE; that is Bob's side of the swap.
	record.State = state.BTCClaimed

	if err := st.Save(ctx, store.Alice, record); err != nil {
		t.Fatal(err)
	}

	if _, err := engine.ClaimBlake(ctx, record.SwapID); err == nil ||
		!strings.Contains(err.Error(), "refusing BLAKE claim as protocol alice") {
		t.Fatalf("err = %v", err)
	}

	// Bob may not refund BLAKE either.
	engine.Identity = bobIdentity

	if _, err := engine.RefundBlake(ctx, record.SwapID); err == nil ||
		!strings.Contains(err.Error(), "refusing BLAKE refund as protocol bob") {
		t.Fatalf("err = %v", err)
	}
}

// TestRefundBlakeReprovesTheBTCRefund: local BTC_REFUNDED state is not authority
// to move BLAKE, because a reorg can put the Bitcoin HTLC back.
func TestRefundBlakeReprovesTheBTCRefund(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	node := newMempoolNode()
	engine := NewEngine(st, fund.NewMemStore(), node, node)
	engine.Identity = aliceIdentity

	record := settlementSwap(t, st, "aaaa00000204", state.BTCRefunded)

	// No confirmed refund anywhere: the BLAKE refund must be refused.
	_, err := engine.RefundBlake(ctx, record.SwapID)

	if err == nil || !strings.Contains(err.Error(), "refusing to refund BLAKE") {
		t.Fatalf("err = %v", err)
	}

	// Now put a confirmed refund on the chain and record it.
	refundID := strings.Repeat("ee", 32)

	block := &chain.VerboseBlock{Hash: *record.BTCFundingBlockHash, Height: 100}
	refund := refundTx(refundID, *record.BTCFundingTxID, 0, 2)
	refund.BlockHash = fmt.Sprintf("%064x", 100)
	node.txs[refundID] = refund
	node.blocks[refund.BlockHash] = block

	record.BTCRefundTxID = &refundID
	record.BTCRefundBlockHash = &refund.BlockHash

	if err := st.Save(ctx, store.Alice, record); err != nil {
		t.Fatal(err)
	}

	// The re-proof now succeeds, and the command proceeds to build a BLAKE
	// refund — which this node cannot do, so the error must be about building,
	// not about the re-proof.
	_, err = engine.RefundBlake(ctx, record.SwapID)

	if err == nil {
		t.Fatal("a BLAKE refund was built by a node that cannot build one")
	}

	if strings.Contains(err.Error(), "refusing to refund BLAKE") {
		t.Fatalf("the re-proof still refused a confirmed refund: %v", err)
	}
}

// TestCompleteReprovesTheActiveClaim: a swap is complete because the chain says
// so, not because the record does.
func TestCompleteReprovesTheActiveClaim(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	node := newMempoolNode()
	engine := NewEngine(st, fund.NewMemStore(), node, node)
	engine.Identity = bobIdentity

	record := settlementSwap(t, st, "aaaa00000205", state.BlakeClaimed)

	if err := engine.Complete(ctx, record.SwapID); err == nil ||
		!strings.Contains(err.Error(), "refusing to complete") {
		t.Fatalf("err = %v", err)
	}

	// A confirmed claim on the BLAKE chain, matching the record.
	claimID := strings.Repeat("f1", 32)

	claim := claimTx(claimID, *record.BlakeFundingTxID, 0, *record.SecretHex, 1)
	claim.BlockHash = fmt.Sprintf("%064x", 100)

	node.txs[claimID] = claim
	node.blocks[claim.BlockHash] = &chain.VerboseBlock{
		Hash: claim.BlockHash, Height: 100, Tx: []chain.Tx{*claim},
	}

	record.BlakeClaimTxID = &claimID
	record.BlakeClaimBlockHash = &claim.BlockHash

	if err := st.Save(ctx, store.Bob, record); err != nil {
		t.Fatal(err)
	}

	if err := engine.Complete(ctx, record.SwapID); err != nil {
		t.Fatalf("a confirmed claim did not complete the swap: %v", err)
	}

	persisted, _ := st.Load(ctx, record.SwapID)

	if persisted.State != state.Complete {
		t.Fatalf("state = %s", persisted.State)
	}

	// The recorded claim must be the one the chain holds.
	other := strings.Repeat("f2", 32)
	persisted.BlakeClaimTxID = &other

	if err := st.Save(ctx, store.Bob, persisted); err != nil {
		t.Fatal(err)
	}

	if err := engine.Complete(ctx, record.SwapID); err == nil {
		t.Fatal("a completed swap accepted a claim the chain does not hold")
	}
}

// TestClaimBlakeNeedsTheRevealedSecret: Bob must claim with the preimage he read
// off Bitcoin, never with a secret he made up.
func TestClaimBlakeNeedsTheRevealedSecret(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	node := newMempoolNode()
	engine := NewEngine(st, fund.NewMemStore(), node, node)
	engine.Identity = bobIdentity

	record := settlementSwap(t, st, "aaaa00000206", state.BTCClaimed)

	// The funding output as the chain would report it, so that the missing
	// secret is the check that fails rather than the contract.
	contract, err := contract.Derive(ctx, node, record, chain.Blake)

	if err != nil {
		t.Fatal(err)
	}

	out := &chain.TxOut{Confirmations: 2}
	out.Value = amountCoin(record.BlakeAmountBaseUnits)
	out.ScriptPubKey.Address = contract.Address
	node.utxos[fmt.Sprintf("%s:0", *record.BlakeFundingTxID)] = out

	if _, err := engine.ClaimBlake(ctx, record.SwapID); err == nil ||
		!strings.Contains(err.Error(), "revealed secret") {
		t.Fatalf("err = %v", err)
	}
}
