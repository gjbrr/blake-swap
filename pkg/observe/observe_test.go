package observe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/contract"
	"github.com/gjbrr/blake-swap/pkg/htlc"
	"github.com/gjbrr/blake-swap/pkg/model"
)

// fakeNode is a chain in a map.
//
// Observation is entirely about what the node reports, so the tests are
// written against a node whose answers the test controls. That is the only way
// to reach the interesting cases at all: a spent HTLC whose spender cannot be
// proven, a wallet-only transaction on a node without txindex, two settlements
// recorded at once. None of those are convenient to produce on a real chain,
// and all of them are exactly where this layer must refuse to act.
type fakeNode struct {
	chain.Chain

	height  int64
	blocks  map[int64]string
	txs     map[string]*chain.Tx
	utxos   map[string]*chain.TxOut
	wallet  map[string]*chain.WalletTx
	wallets map[string]bool
	// failRaw makes RawTx fail for these txids, standing in for a node
	// without txindex that no longer has the transaction.
	failRaw map[string]bool
	// failDerive makes getdescriptorinfo fail, to prove observation reports an
	// invalid rather than a funded HTLC when it cannot prove the contract.
	failDerive bool
	// txOutMisses makes the next N gettxout reads report an absent output
	// even though one exists. It stands in for the race the Python engine
	// reports as "unknown": the output vanished between two reads.
	txOutMisses int
}

func key(txid string, vout int64) string { return fmt.Sprintf("%s:%d", txid, vout) }

func newFakeNode() *fakeNode {
	return &fakeNode{
		height:  100,
		blocks:  map[int64]string{},
		txs:     map[string]*chain.Tx{},
		utxos:   map[string]*chain.TxOut{},
		wallet:  map[string]*chain.WalletTx{},
		wallets: map[string]bool{},
		failRaw: map[string]bool{},
	}
}

func (f *fakeNode) Height(context.Context) (int64, error) { return f.height, nil }

// hashFor is the fake's block hash for a height: the node has every block up
// to its tip, and most of them carry nothing this swap cares about.
func hashFor(height int64) string { return fmt.Sprintf("%064x", height) }

func (f *fakeNode) BlockHash(_ context.Context, height int64) (string, error) {
	if height < 0 || height > f.height {
		return "", fmt.Errorf("Block height out of range: %d", height)
	}

	if hash, ok := f.blocks[height]; ok {
		return hash, nil
	}

	return hashFor(height), nil
}

func (f *fakeNode) BlockTxs(_ context.Context, hash string) (*chain.VerboseBlock, error) {
	height := int64(-1)

	for candidate := int64(0); candidate <= f.height; candidate++ {
		if hashFor(candidate) == hash || f.blocks[candidate] == hash {
			height = candidate

			break
		}
	}

	if height < 0 {
		return nil, fmt.Errorf("Block not found: %s", hash)
	}

	block := &chain.VerboseBlock{
		Hash: hash, Height: height, Confirmations: f.height - height + 1,
	}

	// Deterministic order, because map iteration is not.
	var ids []string

	for txid, tx := range f.txs {
		if tx.BlockHash == hash {
			ids = append(ids, txid)
		}
	}

	sort.Strings(ids)

	for _, txid := range ids {
		block.Tx = append(block.Tx, *f.txs[txid])
	}

	return block, nil
}

func (f *fakeNode) RawTx(_ context.Context, txid, _ string) (*chain.Tx, error) {
	if f.failRaw[txid] {
		return nil, fmt.Errorf("No such mempool or blockchain transaction")
	}

	tx, ok := f.txs[txid]

	if !ok {
		return nil, fmt.Errorf("No such mempool or blockchain transaction")
	}

	return tx, nil
}

func (f *fakeNode) TxOut(_ context.Context, txid string, vout int64) (*chain.TxOut, error) {
	if f.txOutMisses > 0 {
		f.txOutMisses--

		return nil, nil
	}

	out, ok := f.utxos[key(txid, vout)]

	if !ok {
		return nil, nil
	}

	return out, nil
}

func (f *fakeNode) WalletTx(_ context.Context, txid string) (*chain.WalletTx, error) {
	tx, ok := f.wallet[txid]

	if !ok {
		return nil, fmt.Errorf("Invalid or non-wallet transaction id")
	}

	return tx, nil
}

func (f *fakeNode) WalletTxIn(ctx context.Context, wallet, txid string) (*chain.WalletTx, error) {
	if _, ok := f.wallets[wallet]; !ok {
		return nil, fmt.Errorf("wallet %q is not loaded", wallet)
	}

	return f.WalletTx(ctx, txid)
}

func (f *fakeNode) DecodeRaw(_ context.Context, hexTx string) (*chain.Tx, error) {
	tx, ok := f.txs[hexTx]

	if !ok {
		return nil, fmt.Errorf("TX decode failed")
	}

	copied := *tx
	copied.Confirmations = 0
	copied.BlockHash = ""

	return &copied, nil
}

func (f *fakeNode) DescriptorInfo(_ context.Context, body string) (*chain.DescriptorInfo, error) {
	if f.failDerive {
		return nil, fmt.Errorf("Cannot derive scriptPubKey")
	}

	sum := sha256.Sum256([]byte(body))

	return &chain.DescriptorInfo{
		Descriptor: body + "#" + hex.EncodeToString(sum[:4]),
		Checksum:   hex.EncodeToString(sum[:4]),
		IsSolvable: true,
	}, nil
}

func (f *fakeNode) DeriveAddresses(_ context.Context, descriptor string) ([]string, error) {
	sum := sha256.Sum256([]byte(descriptor))

	return []string{"bcrt1q" + hex.EncodeToString(sum[:10])}, nil
}

// swap builds a valid swap whose HTLC the fake node can serve.
func testSwap() *model.Swap {
	str := func(s string) *string { return &s }
	num := func(n int64) *int64 { return &n }
	vout := func(n int32) *int32 { return &n }

	alice := "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"
	bob := "03c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5"
	preimage := strings.Repeat("11", 32)
	sum := sha256.Sum256(mustHex(preimage))

	return &model.Swap{
		SwapID:               "aaaa00000001",
		BlakeAmountBaseUnits: 1_000_000,
		BTCSats:              25_000,
		SecretHex:            str(preimage),
		SecretHash:           hex.EncodeToString(sum[:]),
		BlakeTimeout:         500,
		BTCTimeout:           400,
		BlakeScanStartHeight: num(10),
		BTCScanStartHeight:   num(10),
		BlakeFundingTxID:     str(strings.Repeat("b1", 32)),
		BlakeFundingVout:     vout(0),
		BTCFundingTxID:       str(strings.Repeat("b2", 32)),
		BTCFundingVout:       vout(1),
		BlakeAlicePub:        str(alice),
		BlakeBobPub:          str(bob),
		BTCAlicePub:          str(alice),
		BTCBobPub:            str(bob),
	}
}

func mustHex(s string) []byte {
	decoded, err := hex.DecodeString(s)

	if err != nil {
		panic(err)
	}

	return decoded
}

// fund adds a confirmed funding transaction for one chain to the fake node.
func (f *fakeNode) fund(t *testing.T, swap *model.Swap, ch chain.Chain, confirmations int64) *chain.Tx {
	t.Helper()

	derived, err := contract.Derive(context.Background(), f, swap, ch)

	if err != nil {
		t.Fatalf("derive: %v", err)
	}

	txid := strings.Repeat("f1", 32)
	vout := int64(0)

	if ch == chain.BTC {
		txid = strings.Repeat("f2", 32)
		vout = 1
	}

	f.height = 100

	tx := &chain.Tx{
		TxID:          txid,
		Confirmations: confirmations,
		Vout: []chain.Vout{{
			N:     vout,
			Value: amount.Coin(fmt.Sprintf("%.8f", float64(derived.Amount)/1e8)),
		}},
	}
	tx.Vout[0].ScriptPubKey.Address = derived.Address

	if confirmations > 0 {
		tx.BlockHash = hashFor(f.height)
	}

	f.txs[txid] = tx

	if confirmations > 0 {
		out := &chain.TxOut{Confirmations: confirmations}
		out.Value = tx.Vout[0].Value
		out.ScriptPubKey.Address = derived.Address
		f.utxos[key(txid, vout)] = out
	}

	// The swap must point at the outpoint that was actually funded; the whole
	// point of these tests is that the record and the chain are compared.
	if ch == chain.BTC {
		swap.BTCFundingTxID = &txid
		swap.BTCFundingVout = &[]int32{int32(vout)}[0]

		if confirmations > 0 {
			swap.BTCFundingBlockHash = &tx.BlockHash
		}
	} else {
		swap.BlakeFundingTxID = &txid
		swap.BlakeFundingVout = &[]int32{int32(vout)}[0]

		if confirmations > 0 {
			swap.BlakeFundingBlockHash = &tx.BlockHash
		}
	}

	return tx
}

// spend adds a spending transaction and removes the funded output.
func (f *fakeNode) spend(
	t *testing.T,
	swap *model.Swap,
	funding *chain.Tx,
	branch htlc.Branch,
	confirmations int64,
) *chain.Tx {
	t.Helper()

	witness := []string{strings.Repeat("22", 32), "30", ""}

	if branch == htlc.Claim {
		witness = []string{*swap.SecretHex, "30", "01"}
	}

	witness = append(witness, "63")

	txid := strings.Repeat("cc", 32)

	f.height++

	blockHash := hashFor(f.height)

	tx := &chain.Tx{
		TxID:          txid,
		Confirmations: confirmations,
		Vin: []chain.Vin{{
			TxID:    funding.TxID,
			Vout:    funding.Vout[0].N,
			Witness: witness,
		}},
	}

	if confirmations > 0 {
		tx.BlockHash = blockHash
	}

	f.txs[txid] = tx
	delete(f.utxos, key(funding.TxID, funding.Vout[0].N))

	return tx
}

func fundedSwap(t *testing.T, ch chain.Chain) (*fakeNode, *model.Swap, *chain.Tx) {
	t.Helper()

	node := newFakeNode()
	swap := testSwap()
	funding := node.fund(t, swap, ch, 3)

	return node, swap, funding
}

func observeFake(t *testing.T, node *fakeNode, swap *model.Swap, ch chain.Chain) *Observation {
	t.Helper()

	observation, err := Observe(context.Background(), node, swap, ch, Config{})

	if err != nil {
		t.Fatalf("observe: %v", err)
	}

	return observation
}

// TestObserveReportsEachState walks the vocabulary. Every status is what the
// engine branches on, so every one of them has to be reachable, and reachable
// for the stated reason.
// Parity: tests/swap_bounded_scan_tests.py
func TestObserveReportsEachState(t *testing.T) {
	t.Run("funded", func(t *testing.T) {
		node, swap, _ := fundedSwap(t, chain.BTC)

		got := observeFake(t, node, swap, chain.BTC)

		if got.Status != Funded || got.Confirmations != 3 {
			t.Fatalf("got %s/%d", got.Status, got.Confirmations)
		}
	})

	t.Run("pending", func(t *testing.T) {
		node := newFakeNode()
		swap := testSwap()
		node.fund(t, swap, chain.BTC, 0)

		got := observeFake(t, node, swap, chain.BTC)

		if got.Status != Pending || got.Confirmations != 0 {
			t.Fatalf("got %s/%d", got.Status, got.Confirmations)
		}
	})

	t.Run("spent claim", func(t *testing.T) {
		node, swap, funding := fundedSwap(t, chain.BTC)
		node.spend(t, swap, funding, htlc.Claim, 2)

		got := observeFake(t, node, swap, chain.BTC)

		if got.Status != Spent || got.Branch != htlc.Claim {
			t.Fatalf("got %s/%s", got.Status, got.Branch)
		}

		if got.SpendTxID == "" {
			t.Fatal("a spent HTLC must name its spender")
		}
	})

	t.Run("spent refund", func(t *testing.T) {
		node, swap, funding := fundedSwap(t, chain.Blake)
		node.spend(t, swap, funding, htlc.Refund, 1)

		got := observeFake(t, node, swap, chain.Blake)

		if got.Status != Spent || got.Branch != htlc.Refund {
			t.Fatalf("got %s/%s", got.Status, got.Branch)
		}
	})

	t.Run("absent", func(t *testing.T) {
		node := newFakeNode()
		swap := testSwap()
		// No funding recorded and no matching output in any block.
		swap.BTCFundingTxID = nil
		swap.BTCFundingVout = nil

		got := observeFake(t, node, swap, chain.BTC)

		if got.Status != Absent {
			t.Fatalf("got %s: %s", got.Status, got.Reason)
		}
	})

	t.Run("invalid when the amount is wrong", func(t *testing.T) {
		node, swap, _ := fundedSwap(t, chain.BTC)

		swap.BTCSats = 25_001

		got := observeFake(t, node, swap, chain.BTC)

		if got.Status != Invalid {
			t.Fatalf("got %s: %s", got.Status, got.Reason)
		}

		if !strings.Contains(got.Reason, "Wrong Bitcoin amount") {
			t.Fatalf("reason: %s", got.Reason)
		}
	})

	t.Run("invalid when the contract cannot be derived", func(t *testing.T) {
		node, swap, _ := fundedSwap(t, chain.BTC)
		node.failDerive = true

		got := observeFake(t, node, swap, chain.BTC)

		if got.Status != Invalid {
			t.Fatalf("got %s: %s", got.Status, got.Reason)
		}
	})

	t.Run("pending-or-ambiguous when the outpoint is gone", func(t *testing.T) {
		node, swap, funding := fundedSwap(t, chain.BTC)

		// The output is gone and no block contains a spender: something may
		// already be spending it, which is a refusal, not an absence.
		delete(node.utxos, key(funding.TxID, funding.Vout[0].N))

		got := observeFake(t, node, swap, chain.BTC)

		if got.Status != PendingOrAmbiguous {
			t.Fatalf("got %s: %s", got.Status, got.Reason)
		}

		if !strings.Contains(got.Reason, "Refusing to rebroadcast") {
			t.Fatalf("reason: %s", got.Reason)
		}
	})

	t.Run("unknown when the two reads disagree", func(t *testing.T) {
		node, swap, _ := fundedSwap(t, chain.BTC)

		// The funding read says the output is gone, but the later read that
		// guards against rebroadcast finds it still unspent. That race is the
		// one case the Python engine reports as unknown.
		node.txOutMisses = 1

		got := observeFake(t, node, swap, chain.BTC)

		if got.Status != Unknown {
			t.Fatalf("got %s: %s", got.Status, got.Reason)
		}

		if !strings.Contains(got.Reason, "no confirmed spender was found") {
			t.Fatalf("reason: %s", got.Reason)
		}
	})
}

// TestObserveDiscoversUnrecordedFunding is the recovery path: a participant
// that never recorded the funding outpoint still finds its HTLC, and the
// discovery is bounded by the recorded anchor.
func TestObserveDiscoversUnrecordedFunding(t *testing.T) {
	node, swap, funding := fundedSwap(t, chain.BTC)
	swap.BTCFundingTxID = nil
	swap.BTCFundingVout = nil
	swap.BTCFundingBlockHash = nil

	got := observeFake(t, node, swap, chain.BTC)

	if got.Status != Funded {
		t.Fatalf("got %s: %s", got.Status, got.Reason)
	}

	if !got.Discovered {
		t.Fatal("a scanned funding is reported as discovered")
	}

	if got.TxID != funding.TxID || got.Vout != funding.Vout[0].N {
		t.Fatalf("discovered %s:%d", got.TxID, got.Vout)
	}
}

// TestObserveRefusesHalfAnOutpoint: a txid without a vout could name any
// output of that transaction, and funding against the wrong output is
// unrecoverable.
func TestObserveRefusesHalfAnOutpoint(t *testing.T) {
	node, swap, _ := fundedSwap(t, chain.BTC)
	swap.BTCFundingVout = nil

	if _, err := Observe(context.Background(), node, swap, chain.BTC, Config{}); err == nil {
		t.Fatal("an incomplete outpoint was accepted")
	}
}

// TestObservePendingSettlementIsAWait proves the mempool case: the recorded
// settlement transaction is not confirmed, so observation says "wait" rather
// than "nothing there".
func TestObservePendingSettlementIsAWait(t *testing.T) {
	node, swap, funding := fundedSwap(t, chain.BTC)

	preimage := *swap.SecretHex
	txid := strings.Repeat("ee", 32)
	settlement := &chain.Tx{
		TxID: txid,
		Vin: []chain.Vin{{
			TxID: funding.TxID, Vout: funding.Vout[0].N,
			Witness: []string{preimage, "30", "01", "63"},
		}},
	}
	node.txs[txid] = settlement
	delete(node.utxos, key(funding.TxID, funding.Vout[0].N))
	swap.BTCCLaimTxID = &txid

	got := observeFake(t, node, swap, chain.BTC)

	if got.Status != PendingSpend {
		t.Fatalf("got %s: %s", got.Status, got.Reason)
	}

	if got.SpendTxID != txid || got.Branch != htlc.Claim {
		t.Fatalf("got spender %s branch %s", got.SpendTxID, got.Branch)
	}
}

// TestObservePendingSettlementWrongBranch: a recorded settlement that used the
// refund branch while the record says claim is a contradiction, and the caller
// must not act on it.
func TestObservePendingSettlementWrongBranch(t *testing.T) {
	node, swap, funding := fundedSwap(t, chain.BTC)

	txid := strings.Repeat("ef", 32)
	node.txs[txid] = &chain.Tx{
		TxID: txid,
		Vin: []chain.Vin{{
			TxID: funding.TxID, Vout: funding.Vout[0].N,
			Witness: []string{"30", "", "63"},
		}},
	}
	delete(node.utxos, key(funding.TxID, funding.Vout[0].N))
	swap.BTCCLaimTxID = &txid

	got := observeFake(t, node, swap, chain.BTC)

	if got.Status != PendingOrAmbiguous {
		t.Fatalf("got %s: %s", got.Status, got.Reason)
	}

	if !strings.Contains(got.Reason, "branch mismatch") {
		t.Fatalf("reason: %s", got.Reason)
	}
}

// TestScanFundingIgnoresTheWrongAmount: the scan matches address *and* amount,
// so a decoy paying the HTLC address a different amount is not a match.
func TestScanFundingIgnoresTheWrongAmount(t *testing.T) {
	node, swap, _ := fundedSwap(t, chain.BTC)

	swap.BTCFundingTxID = nil
	swap.BTCFundingVout = nil

	got := observeFake(t, node, swap, chain.BTC)

	if got.Status != Funded {
		t.Fatalf("got %s: %s", got.Status, got.Reason)
	}

	// Now change the expected amount: the decoy no longer matches, and an
	// absent HTLC is what remains.
	swap.BTCSats = 25_001

	got = observeFake(t, node, swap, chain.BTC)

	if got.Status != Absent {
		t.Fatalf("got %s: %s", got.Status, got.Reason)
	}
}

// TestScanFundingRefusesAnUnboundedScan keeps the anchor rule honest.
func TestScanFundingRefusesAnUnboundedScan(t *testing.T) {
	node, swap, _ := fundedSwap(t, chain.BTC)
	swap.BTCFundingTxID = nil
	swap.BTCFundingVout = nil
	swap.BTCScanStartHeight = nil

	got := observeFake(t, node, swap, chain.BTC)

	if got.Status != Unknown || !strings.Contains(got.Reason, "no recorded Bitcoin funding scan anchor") {
		t.Fatalf("got %s: %s", got.Status, got.Reason)
	}

	// The override is the documented way out for a legacy record.
	observation, err := Observe(context.Background(), node, swap, chain.BTC, Config{
		AllowUnanchoredScan: true,
	})

	if err != nil {
		t.Fatal(err)
	}

	if observation.Status != Funded {
		t.Fatalf("with the override: %s: %s", observation.Status, observation.Reason)
	}
}

// TestScanFundingRefusesAFutureAnchor: an anchor above the tip means the chain
// was reset or the record is inconsistent, and guessing is exactly what must
// not happen.
func TestScanFundingRefusesAFutureAnchor(t *testing.T) {
	node, swap, _ := fundedSwap(t, chain.BTC)
	swap.BTCFundingTxID = nil
	swap.BTCFundingVout = nil
	anchor := int64(500)
	swap.BTCScanStartHeight = &anchor

	got := observeFake(t, node, swap, chain.BTC)

	if got.Status != Unknown || !strings.Contains(got.Reason, "ahead of local tip") {
		t.Fatalf("got %s: %s", got.Status, got.Reason)
	}
}

// TestScanFundingBoundsTheWork: a scan wider than the limit is refused rather
// than run for hours.
func TestScanFundingBoundsTheWork(t *testing.T) {
	node, swap, _ := fundedSwap(t, chain.BTC)

	anchor := int64(1)
	swap.BTCScanStartHeight = &anchor

	_, err := ScanFunding(context.Background(), node, swap, chain.BTC, Config{
		MaxScanBlocks: 10,
	})

	// The funding outpoint is populated here, so the scan does not run; clear
	// it to exercise the bound.
	swap.BTCFundingTxID = nil
	swap.BTCFundingVout = nil

	_, err = ScanFunding(context.Background(), node, swap, chain.BTC, Config{
		MaxScanBlocks: 10,
	})

	if err == nil || !strings.Contains(err.Error(), "above the 10-block limit") {
		t.Fatalf("err = %v", err)
	}
}

// TestScanFundingRefusesAmbiguity: two outputs with the exact address and
// amount cannot be told apart, so neither is adopted.
func TestScanFundingRefusesAmbiguity(t *testing.T) {
	node, swap, funding := fundedSwap(t, chain.BTC)
	swap.BTCFundingTxID = nil
	swap.BTCFundingVout = nil

	// A second, identical output in a different block.
	twin := *funding
	twin.TxID = strings.Repeat("f3", 32)
	node.txs[twin.TxID] = &twin

	got := observeFake(t, node, swap, chain.BTC)

	if got.Status != Unknown || !strings.Contains(got.Reason, "ambiguous") {
		t.Fatalf("got %s: %s", got.Status, got.Reason)
	}
}

// TestFindSpendVerifiesARecordedSettlement covers the fast path: a recorded
// txid that is confirmed and proven is returned without a block scan.
func TestFindSpendVerifiesARecordedSettlement(t *testing.T) {
	node, swap, funding := fundedSwap(t, chain.BTC)
	spend := node.spend(t, swap, funding, htlc.Claim, 1)
	swap.BTCCLaimTxID = &spend.TxID
	swap.BTCCLaimBlockHash = &spend.BlockHash

	got, err := FindSpend(context.Background(), node, swap, chain.BTC, Config{})

	if err != nil {
		t.Fatal(err)
	}

	if got == nil || got.TxID != spend.TxID || got.Branch != htlc.Claim {
		t.Fatalf("got %+v", got)
	}
}

// TestFindSpendRefusesAContradictedRecord: if the recorded transaction does not
// spend this HTLC, the record is wrong and the caller must not rebroadcast.
func TestFindSpendRefusesAContradictedRecord(t *testing.T) {
	node, swap, funding := fundedSwap(t, chain.BTC)

	other := strings.Repeat("f4", 32)
	node.txs[other] = &chain.Tx{
		TxID:          other,
		Confirmations: 2,
		BlockHash:     strings.Repeat("ce", 32),
		Vin:           []chain.Vin{{TxID: strings.Repeat("99", 32), Vout: 0}},
	}
	swap.BTCCLaimTxID = &other

	_, err := FindSpend(context.Background(), node, swap, chain.BTC, Config{})

	if err == nil {
		t.Fatal("a settlement that does not spend the HTLC was accepted")
	}

	if class, ok := ClassOf(err); !ok || class != Rejected {
		t.Fatalf("err = %v (class %v/%v)", err, class, ok)
	}

	_ = funding
}

// TestFindSpendRefusesTwoRecordedSettlements: two claims cannot both be the
// settlement, and choosing one would restore funds that may never have moved.
func TestFindSpendRefusesTwoRecordedSettlements(t *testing.T) {
	node, swap, funding := fundedSwap(t, chain.BTC)
	spend := node.spend(t, swap, funding, htlc.Claim, 1)

	twin := strings.Repeat("f5", 32)
	swap.BTCCLaimTxID = &spend.TxID
	swap.BTCRefundTxID = &twin

	_, err := FindSpend(context.Background(), node, swap, chain.BTC, Config{})

	if err == nil || !strings.Contains(err.Error(), "multiple") {
		t.Fatalf("err = %v", err)
	}
}

// TestFindSpendRefusesRebroadcastWhenTheOutpointIsGone is the dangerous case:
// no confirmed spender, no output. Something may already be spending it.
func TestFindSpendRefusesRebroadcastWhenTheOutpointIsGone(t *testing.T) {
	node, swap, funding := fundedSwap(t, chain.BTC)
	delete(node.utxos, key(funding.TxID, funding.Vout[0].N))

	_, err := FindSpend(context.Background(), node, swap, chain.BTC, Config{})

	if err == nil {
		t.Fatal("an unavailable outpoint with no spender was accepted")
	}

	if class, ok := ClassOf(err); !ok || class != Rejected {
		t.Fatalf("err = %v", err)
	}

	if !strings.Contains(err.Error(), "Refusing to rebroadcast") {
		t.Fatalf("err = %v", err)
	}
}

// TestFindSpendRecoversThroughTheWallet: on a node without txindex, a
// wallet-created settlement is unreadable by txid but recoverable from the
// wallet's hex. That path must produce the same verified result.
func TestFindSpendRecoversThroughTheWallet(t *testing.T) {
	node, swap, funding := fundedSwap(t, chain.BTC)
	spend := node.spend(t, swap, funding, htlc.Claim, 1)

	txid := spend.TxID
	swap.BTCCLaimTxID = &txid
	node.failRaw[txid] = true
	node.wallet[txid] = &chain.WalletTx{
		TxID:          txid,
		Hex:           txid,
		Confirmations: 1,
		BlockHash:     spend.BlockHash,
	}

	node.wallets["bob"] = true

	got, err := FindSpend(context.Background(), node, swap, chain.BTC, Config{Wallet: "bob"})

	if err != nil {
		t.Fatal(err)
	}

	if got == nil || got.TxID != txid || got.Branch != htlc.Claim {
		t.Fatalf("got %+v", got)
	}
}

// TestFindSpendRefusesWithoutARecoveryWallet: the wallet fallback is the only way
// to read a wallet-created transaction on a node without txindex, so a caller that
// configured no wallet must be told rather than silently failing to recover.
func TestFindSpendRefusesWithoutARecoveryWallet(t *testing.T) {
	node, swap, funding := fundedSwap(t, chain.BTC)
	spend := node.spend(t, swap, funding, htlc.Claim, 1)

	txid := spend.TxID
	swap.BTCCLaimTxID = &txid
	node.failRaw[txid] = true

	_, err := FindSpend(context.Background(), node, swap, chain.BTC, Config{})

	if err == nil || !strings.Contains(err.Error(), "no recovery wallet") {
		t.Fatalf("err = %v", err)
	}
}

// TestFindSpendUnconfirmedRecordedSettlementIsPending: an unconfirmed recorded
// settlement must be a wait, and it must be classified as such rather than as
// a rejection the operator would investigate as corruption.
func TestFindSpendUnconfirmedRecordedSettlementIsPending(t *testing.T) {
	node, swap, funding := fundedSwap(t, chain.BTC)
	spend := node.spend(t, swap, funding, htlc.Claim, 0)
	txid := spend.TxID
	node.txs[txid] = &chain.Tx{
		TxID:          txid,
		Confirmations: 0,
		Vin: []chain.Vin{{
			TxID: funding.TxID, Vout: funding.Vout[0].N,
			Witness: []string{*swap.SecretHex, "30", "01", "63"},
		}},
	}
	swap.BTCCLaimTxID = &txid

	_, err := FindSpend(context.Background(), node, swap, chain.BTC, Config{})

	if err == nil {
		t.Fatal("an unconfirmed settlement was reported as confirmed")
	}

	if class, ok := ClassOf(err); !ok || class != Wait {
		t.Fatalf("err = %v (class %v/%v)", err, class, ok)
	}
}

// TestFindSpendRejectsAnInactiveTransaction: a confirmed settlement that a
// reorg rolled back is not a settlement.
func TestFindSpendRejectsAnInactiveTransaction(t *testing.T) {
	node, swap, funding := fundedSwap(t, chain.BTC)
	spend := node.spend(t, swap, funding, htlc.Claim, 1)
	inactive := false
	spend.InActiveChain = &inactive
	swap.BTCCLaimTxID = &spend.TxID
	swap.BTCCLaimBlockHash = &spend.BlockHash

	_, err := FindSpend(context.Background(), node, swap, chain.BTC, Config{})

	if err == nil || !strings.Contains(err.Error(), "no longer on the active chain") {
		t.Fatalf("err = %v", err)
	}
}
