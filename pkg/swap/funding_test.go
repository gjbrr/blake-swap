package swap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/decimal"
	"github.com/gjbrr/blake-swap/pkg/fund"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/state"
	"github.com/gjbrr/blake-swap/pkg/store"
)

// chainNode is one chain in a map: enough of a node for the funding commands.
//
// The funding path is what these tests exercise, so the fake implements exactly
// what it touches and embeds the rest of the interface: an unstubbed call is a nil
// method value, which panics, which is the right failure for "this command needed
// something the test did not expect".
type chainNode struct {
	Node

	height  int64
	wallets map[string]*testWallet
	txs     map[string]*chain.Tx
	utxos   map[string]*chain.TxOut
}

func newChainNode(height int64) *chainNode {
	return &chainNode{
		Node:    nil,
		height:  height,
		wallets: map[string]*testWallet{},
		txs:     map[string]*chain.Tx{},
		utxos:   map[string]*chain.TxOut{},
	}
}

func (c *chainNode) Height(context.Context) (int64, error) { return c.height, nil }

func (c *chainNode) Bind(name string) chain.Wallet {
	wallet, ok := c.wallets[name]

	if !ok {
		wallet = &testWallet{chain: c, name: name, balance: decimal.MustParse("5")}
		c.wallets[name] = wallet
	}

	return wallet
}

func (c *chainNode) ListWallets(context.Context) ([]string, error) {
	var names []string

	for name := range c.wallets {
		names = append(names, name)
	}

	return names, nil
}

func (c *chainNode) EnsureWallet(_ context.Context, wallet string, watchOnly bool) error {
	if _, ok := c.wallets[wallet]; !ok {
		c.wallets[wallet] = &testWallet{chain: c, name: wallet, watchOnly: watchOnly}
	}

	return nil
}

func (c *chainNode) DescriptorInfo(_ context.Context, body string) (*chain.DescriptorInfo, error) {
	sum := sha256.Sum256([]byte(body))

	return &chain.DescriptorInfo{
		Descriptor: body + "#" + hex.EncodeToString(sum[:4]),
		Checksum:   hex.EncodeToString(sum[:4]),
		IsSolvable: true,
	}, nil
}

func (c *chainNode) DeriveAddresses(_ context.Context, descriptor string) ([]string, error) {
	sum := sha256.Sum256([]byte(descriptor))

	return []string{"bcrt1q" + hex.EncodeToString(sum[:10])}, nil
}

func (c *chainNode) TxOut(_ context.Context, txid string, vout int64) (*chain.TxOut, error) {
	out, ok := c.utxos[fmt.Sprintf("%s:%d", txid, vout)]

	if !ok {
		return nil, nil
	}

	return out, nil
}

func (c *chainNode) RawTx(_ context.Context, txid, blockHash string) (*chain.Tx, error) {
	tx, ok := c.txs[txid]

	if !ok || (blockHash != "" && tx.BlockHash != blockHash) {
		return nil, fmt.Errorf("No such transaction")
	}

	return tx, nil
}

func (c *chainNode) DecodeRaw(_ context.Context, hexTx string) (*chain.Tx, error) {
	tx, ok := c.txs[hexTx]

	if !ok {
		return nil, fmt.Errorf("TX decode failed")
	}

	return tx, nil
}

func (c *chainNode) EstimateSmartFee(context.Context, int64) (*chain.FeeEstimate, error) {
	return &chain.FeeEstimate{Feerate: json.Number("0.00010000")}, nil
}

func (c *chainNode) MempoolInfo(context.Context) (*chain.MempoolInfo, error) {
	info := &chain.MempoolInfo{}
	info.MinRelayTxFee = json.Number("0.00001000")
	info.MempoolMinFee = json.Number("0.00001000")

	return info, nil
}

func (c *chainNode) MineToAddress(_ context.Context, blocks int64, _ string) ([]string, error) {
	c.height += blocks

	var hashes []string

	for i := int64(0); i < blocks; i++ {
		hashes = append(hashes, fmt.Sprintf("%064x", c.height))
	}

	// Confirming a block confirms everything in it, both views: the decoded
	// transaction and the UTXO set the verifiers read.
	for _, tx := range c.txs {
		if tx.BlockHash == "" {
			tx.BlockHash = fmt.Sprintf("%064x", c.height)
			tx.Confirmations = 1
		}
	}

	for _, out := range c.utxos {
		if out.Confirmations < 1 {
			out.Confirmations = c.height
		}
	}

	for _, wallet := range c.wallets {
		for _, tx := range wallet.txs {
			if tx.BlockHash == "" {
				tx.BlockHash = fmt.Sprintf("%064x", c.height)
				tx.Confirmations = 1
			}
		}
	}

	return hashes, nil
}

// testWallet is one wallet's view.
type testWallet struct {
	chain     *chainNode
	name      string
	watchOnly bool
	balance   decimal.Dec
	entries   []chain.WalletEntry
	txs       map[string]*chain.WalletTx
	imported  []string
}

func (w *testWallet) Name() string { return w.name }

func (w *testWallet) Balances(context.Context) (*chain.Balances, error) {
	if w.balance.IsZero() {
		w.balance = decimal.MustParse("5")
	}

	return chain.BalancesFromTrusted(w.balance.String()), nil
}

func (w *testWallet) Tx(_ context.Context, txid string) (*chain.WalletTx, error) {
	if tx, ok := w.txs[txid]; ok {
		return tx, nil
	}

	return nil, fmt.Errorf("Invalid or non-wallet transaction id")
}

func (w *testWallet) ListTransactions(context.Context, int64, int64) ([]chain.WalletEntry, error) {
	return w.entries, nil
}

func (w *testWallet) AddressInfo(_ context.Context, address string) (*chain.AddressInfo, error) {
	return &chain.AddressInfo{Address: address, Solvable: true}, nil
}

func (w *testWallet) ImportDescriptor(_ context.Context, descriptor string) (*chain.ImportResult, error) {
	w.imported = append(w.imported, descriptor)

	return &chain.ImportResult{Success: true}, nil
}

func (w *testWallet) SendToAddress(
	_ context.Context, address string, sats amount.Sats, feeRate *decimal.Dec,
) (string, error) {
	txid := strings.Repeat("aa", 32)

	tx := &chain.Tx{
		TxID: txid,
		Vout: []chain.Vout{{N: 0, Value: amount.Coin(sats.Coin().String())}},
	}
	tx.Vout[0].ScriptPubKey.Address = address

	w.chain.txs[txid] = tx

	out := &chain.TxOut{}
	out.Value = tx.Vout[0].Value
	out.ScriptPubKey.Address = address
	w.chain.utxos[fmt.Sprintf("%s:0", txid)] = out

	if w.txs == nil {
		w.txs = map[string]*chain.WalletTx{}
	}

	w.txs[txid] = &chain.WalletTx{TxID: txid, Hex: txid}

	w.entries = append(w.entries, chain.WalletEntry{
		Address:  address,
		Category: "send",
		TxID:     txid,
		Amount:   amount.Coin("-" + sats.Coin().String()),
	})

	return txid, nil
}

func (w *testWallet) NewAddress(context.Context) (string, error) {
	return "bcrt1qmining", nil
}

func (w *testWallet) Decode(ctx context.Context, hexTx string) (*chain.Tx, error) {
	return w.chain.DecodeRaw(ctx, hexTx)
}

func (w *testWallet) ProcessPSBT(context.Context, string, string) (*chain.ProcessPSBTResult, error) {
	return nil, fmt.Errorf("funding does not sign PSBTs")
}

// testIdentity is a local Nostr identity.
type testIdentity string

func (i testIdentity) PublicKey() string { return string(i) }

// aliceIdentity and bobIdentity are the two pinned participants.
const (
	aliceIdentity testIdentity = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	bobIdentity   testIdentity = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// pinnedSwap builds a stored swap with both Nostr identities pinned, which is
// what a swap that has been offered and accepted carries.
func pinnedSwap(t *testing.T, st store.Store, id string) *model.Swap {
	t.Helper()

	record := newRecord(t, id)

	alice := string(aliceIdentity)
	bob := string(bobIdentity)
	record.NostrAlicePub = &alice
	record.NostrBobPub = &bob

	// Contract keys, so the HTLC can be derived.
	record.BlakeAlicePub = strPtr("0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798")
	record.BlakeBobPub = strPtr("03c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5")
	record.BTCAlicePub = record.BlakeAlicePub
	record.BTCBobPub = record.BlakeBobPub

	if err := st.Save(context.Background(), store.Alice, record); err != nil {
		t.Fatal(err)
	}

	return record
}

func strPtr(value string) *string { return &value }

// fundingEngine builds an engine whose two chains are fakes.
func fundingEngine(t *testing.T, st store.Store) (*Engine, *chainNode, *chainNode) {
	t.Helper()

	blake := newChainNode(100)
	btc := newChainNode(100)

	engine := NewEngine(st, fund.NewMemStore(), blake, btc)
	engine.Identity = aliceIdentity
	engine.Funding.AutoMine = 1

	return engine, blake, btc
}

func TestFundBlakeHappyPath(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	engine, _, _ := fundingEngine(t, st)
	record := pinnedSwap(t, st, "aaaa00000101")

	result, err := engine.FundBlake(ctx, record.SwapID)

	if err != nil {
		t.Fatal(err)
	}

	if result.Pending || result.State != state.BlakeFunded {
		t.Fatalf("result = %+v", result)
	}

	if result.TxID == "" || result.Vout != 0 || result.BlockHash == "" {
		t.Fatalf("outpoint = %s:%d %s", result.TxID, result.Vout, result.BlockHash)
	}

	persisted, err := st.Load(ctx, record.SwapID)

	if err != nil {
		t.Fatal(err)
	}

	if persisted.State != state.BlakeFunded {
		t.Fatalf("state = %s", persisted.State)
	}

	if persisted.BlakeFundingTxID == nil || *persisted.BlakeFundingTxID != result.TxID {
		t.Fatal("the funding outpoint was not recorded")
	}

	// The intent is resolved once the outpoint is durable.
	if intents, found, _ := fund.ReadIntent(ctx, engine.Intents, record.SwapID, chain.Blake); found {
		t.Fatalf("a resolved funding left its intent behind: %+v", intents)
	}
}

func TestFundBlakeRefusesTheWrongRole(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	engine, _, _ := fundingEngine(t, st)
	engine.Identity = bobIdentity

	record := pinnedSwap(t, st, "aaaa00000102")

	_, err := engine.FundBlake(ctx, record.SwapID)

	if err == nil || !strings.Contains(err.Error(), "refusing BLAKE funding as protocol bob") {
		t.Fatalf("err = %v", err)
	}

	persisted, _ := st.Load(ctx, record.SwapID)

	if persisted.State != state.Offered || persisted.BlakeFundingTxID != nil {
		t.Fatal("a refused funding changed the record")
	}
}

func TestFundBlakeRefusesTheWrongState(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	engine, _, _ := fundingEngine(t, st)
	record := pinnedSwap(t, st, "aaaa00000103")

	if _, _, err := engine.Advance(ctx, record.SwapID, store.Alice, string(state.BlakeFunded)); err == nil {
		// Advance is gated; force the state through the store instead.
		_ = err
	}

	record.State = state.BlakeFunded

	if err := st.Save(ctx, store.Alice, record); err != nil {
		t.Fatal(err)
	}

	_, err := engine.FundBlake(ctx, record.SwapID)

	if err == nil || !strings.Contains(err.Error(), "requires swap state OFFERED") {
		t.Fatalf("err = %v", err)
	}
}

func TestPendingBlakeFundingLeavesTheStateAtOffered(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	engine, _, _ := fundingEngine(t, st)
	engine.Funding.AutoMine = 0

	record := pinnedSwap(t, st, "aaaa00000104")

	result, err := engine.FundBlake(ctx, record.SwapID)

	if err != nil {
		t.Fatal(err)
	}

	if !result.Pending || result.State != state.Offered {
		t.Fatalf("result = %+v", result)
	}

	persisted, _ := st.Load(ctx, record.SwapID)

	if persisted.State != state.Offered || persisted.BlakeFundingTxID != nil {
		t.Fatal("an unconfirmed funding advanced the record")
	}

	// The intent is what remembers the attempt while the chain decides.
	if _, found, _ := fund.ReadIntent(ctx, engine.Intents, record.SwapID, chain.Blake); !found {
		t.Fatal("a pending funding left no intent")
	}
}

func TestFundBTCRequiresAVerifiedBlakeContract(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	engine, _, _ := fundingEngine(t, st)
	engine.Identity = bobIdentity

	record := pinnedSwap(t, st, "aaaa00000105")

	if _, err := engine.FundBTC(ctx, record.SwapID); err == nil ||
		!strings.Contains(err.Error(), "requires state BLAKE_FUNDED") {
		t.Fatalf("err = %v", err)
	}

	// BLAKE_FUNDED, but the BLAKE output is not on the chain: Bob must not pay.
	record.State = state.BlakeFunded

	txid := strings.Repeat("bb", 32)
	vout := int32(0)
	record.BlakeFundingTxID = &txid
	record.BlakeFundingVout = &vout

	if err := st.Save(ctx, store.Alice, record); err != nil {
		t.Fatal(err)
	}

	_, err := engine.FundBTC(ctx, record.SwapID)

	if err == nil || !strings.Contains(err.Error(), "BLAKE contract is not acceptable") {
		t.Fatalf("err = %v", err)
	}
}

func TestFundBTCHappyPath(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	engine, _, _ := fundingEngine(t, st)

	// Alice funds BLAKE first, so Bob has a real contract to verify.
	record := pinnedSwap(t, st, "aaaa00000106")

	if _, err := engine.FundBlake(ctx, record.SwapID); err != nil {
		t.Fatal(err)
	}

	engine.Identity = bobIdentity

	result, err := engine.FundBTC(ctx, record.SwapID)

	if err != nil {
		t.Fatal(err)
	}

	if result.Pending || result.State != state.BTCFunded {
		t.Fatalf("result = %+v", result)
	}

	persisted, _ := st.Load(ctx, record.SwapID)

	if persisted.State != state.BTCFunded || persisted.BTCFundingTxID == nil {
		t.Fatalf("record = %+v", persisted)
	}
}

func TestPendingBTCFundingRecordsTheOutpoint(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	engine, _, _ := fundingEngine(t, st)
	record := pinnedSwap(t, st, "aaaa00000107")

	if _, err := engine.FundBlake(ctx, record.SwapID); err != nil {
		t.Fatal(err)
	}

	engine.Identity = bobIdentity
	engine.Funding.AutoMine = 0

	// A BLAKE funding that is confirmed but a BTC funding that is not: exactly
	// the state reconciliation has to be able to tell from "no BTC funding".
	result, err := engine.FundBTC(ctx, record.SwapID)

	if err != nil {
		t.Fatal(err)
	}

	if !result.Pending {
		t.Fatal("an unconfirmed funding was reported as confirmed")
	}

	persisted, _ := st.Load(ctx, record.SwapID)

	if persisted.State != state.BlakeFunded {
		t.Fatalf("state = %s", persisted.State)
	}

	if persisted.BTCFundingTxID == nil || *persisted.BTCFundingTxID != result.TxID {
		t.Fatal("the pending outpoint was not recorded")
	}

	if persisted.BTCFundingBlockHash != nil {
		t.Fatal("a pending funding recorded a block hash")
	}
}

func TestVerifyCommands(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	engine, _, _ := fundingEngine(t, st)
	record := pinnedSwap(t, st, "aaaa00000108")

	if _, err := engine.FundBlake(ctx, record.SwapID); err != nil {
		t.Fatal(err)
	}

	if err := engine.VerifyBlake(ctx, record.SwapID); err != nil {
		t.Fatalf("the funding this command just made does not verify: %v", err)
	}

	// A claim on a fabricated outpoint must fail: the check is against the chain,
	// not against the record.
	bogus := strings.Repeat("ff", 32)
	persisted, _ := st.Load(ctx, record.SwapID)
	persisted.BlakeFundingTxID = &bogus

	if err := st.Save(ctx, store.Alice, persisted); err != nil {
		t.Fatal(err)
	}

	if err := engine.VerifyBlake(ctx, record.SwapID); err == nil {
		t.Fatal("a fabricated funding outpoint verified")
	}
}

func TestLocalRole(t *testing.T) {
	record := newRecord(t, "aaaa00000109")

	if _, _, err := LocalRole(record, string(aliceIdentity)); err == nil {
		t.Fatal("an unpinned identity was given a role")
	}

	alice := string(aliceIdentity)
	bob := string(bobIdentity)

	record.NostrAlicePub = &alice

	role, peer, err := LocalRole(record, alice)

	if err != nil || role != "alice" || peer != "" {
		t.Fatalf("%s %s %v", role, peer, err)
	}

	record.NostrBobPub = &bob

	if role, peer, err = LocalRole(record, bob); err != nil || role != "bob" || peer != alice {
		t.Fatalf("%s %s %v", role, peer, err)
	}

	// Pinned as both is a record that cannot say which side it is.
	same := string(aliceIdentity)
	record.NostrBobPub = &same

	if _, _, err := LocalRole(record, alice); err == nil {
		t.Fatal("a swap that pins both roles to one identity was resolved")
	}

	if _, _, err := LocalRole(record, ""); err == nil {
		t.Fatal("an empty local identity was accepted")
	}
}
