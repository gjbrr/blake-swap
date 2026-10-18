package fund

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
	"github.com/gjbrr/blake-swap/pkg/contract"
	"github.com/gjbrr/blake-swap/pkg/decimal"
	"github.com/gjbrr/blake-swap/pkg/model"
)

// fakeChain is one node with two wallets.
//
// Funding is mostly about the moments where the node has already done something
// the swap record does not know about yet, so the tests need a node whose
// answers can be arranged into exactly those moments: a payment that exists
// before the record does, an import the node refuses, a broadcast that is still
// unconfirmed.
type fakeChain struct {
	height    int64
	txs       map[string]*chain.Tx
	utxos     map[string]*chain.TxOut
	wallets   map[string]*fakeWallet
	feeRate   string
	mined     int64
	failSend  bool
	sendError string
	// events records the order of the two side effects funding has to keep in
	// the right order: the watch-only import must precede the broadcast.
	events []string
}

func newFakeChain() *fakeChain {
	return &fakeChain{
		height:  100,
		txs:     map[string]*chain.Tx{},
		utxos:   map[string]*chain.TxOut{},
		wallets: map[string]*fakeWallet{},
		feeRate: "0.00010000",
	}
}

func (f *fakeChain) Bind(name string) chain.Wallet {
	wallet, ok := f.wallets[name]

	if !ok {
		wallet = &fakeWallet{chain: f, name: name, balance: decimal.Zero()}
		f.wallets[name] = wallet
	}

	return wallet
}

func (f *fakeChain) Height(context.Context) (int64, error) { return f.height, nil }

func (f *fakeChain) ListWallets(context.Context) ([]string, error) {
	var names []string

	for name := range f.wallets {
		names = append(names, name)
	}

	return names, nil
}

func (f *fakeChain) EnsureWallet(_ context.Context, wallet string, watchOnly bool) error {
	if _, ok := f.wallets[wallet]; !ok {
		f.wallets[wallet] = &fakeWallet{
			chain: f, name: wallet, watchOnly: watchOnly, balance: decimal.Zero(),
		}
	}

	return nil
}

func (f *fakeChain) DescriptorInfo(_ context.Context, body string) (*chain.DescriptorInfo, error) {
	sum := sha256.Sum256([]byte(body))

	return &chain.DescriptorInfo{
		Descriptor: body + "#" + hex.EncodeToString(sum[:4]),
		Checksum:   hex.EncodeToString(sum[:4]),
		IsSolvable: true,
	}, nil
}

func (f *fakeChain) DeriveAddresses(_ context.Context, descriptor string) ([]string, error) {
	sum := sha256.Sum256([]byte(descriptor))

	return []string{"bcrt1q" + hex.EncodeToString(sum[:10])}, nil
}

func (f *fakeChain) TxOut(_ context.Context, txid string, vout int64) (*chain.TxOut, error) {
	out, ok := f.utxos[fmt.Sprintf("%s:%d", txid, vout)]

	if !ok {
		return nil, nil
	}

	return out, nil
}

func (f *fakeChain) RawTx(_ context.Context, txid, blockHash string) (*chain.Tx, error) {
	tx, ok := f.txs[txid]

	if !ok || (blockHash != "" && tx.BlockHash != blockHash) {
		return nil, fmt.Errorf("No such transaction")
	}

	return tx, nil
}

func (f *fakeChain) DecodeRaw(_ context.Context, hexTx string) (*chain.Tx, error) {
	tx, ok := f.txs[hexTx]

	if !ok {
		return nil, fmt.Errorf("TX decode failed")
	}

	copied := *tx

	return &copied, nil
}

func (f *fakeChain) EstimateSmartFee(context.Context, int64) (*chain.FeeEstimate, error) {
	return &chain.FeeEstimate{Feerate: json.Number(f.feeRate)}, nil
}

func (f *fakeChain) MempoolInfo(context.Context) (*chain.MempoolInfo, error) {
	info := &chain.MempoolInfo{}
	info.MinRelayTxFee = json.Number("0.00001000")
	info.MempoolMinFee = json.Number("0.00001000")

	return info, nil
}

func (f *fakeChain) MineToAddress(context.Context, int64, string) ([]string, error) {
	f.height++
	f.mined++

	// Confirm anything still pending, the way a mined block would. Both views
	// move: the node's decoded transactions and the wallets' own records.
	for _, tx := range f.txs {
		if tx.BlockHash == "" {
			tx.BlockHash = fmt.Sprintf("%064x", f.height)
			tx.Confirmations = 1
		}
	}

	for _, wallet := range f.wallets {
		for _, tx := range wallet.txs {
			if tx.BlockHash == "" {
				tx.BlockHash = fmt.Sprintf("%064x", f.height)
				tx.Confirmations = 1
			}
		}
	}

	return []string{fmt.Sprintf("%064x", f.height)}, nil
}

// fakeWallet is one wallet's view.
type fakeWallet struct {
	chain     *fakeChain
	name      string
	watchOnly bool
	balance   decimal.Dec
	entries   []chain.WalletEntry
	txs       map[string]*chain.WalletTx
	imported  []string
	failImprt bool
	solvable  bool
	sent      []sent
}

type sent struct {
	address string
	sats    amount.Sats
	feeRate string
}

func (w *fakeWallet) Name() string { return w.name }

func (w *fakeWallet) Balances(context.Context) (*chain.Balances, error) {
	return chain.BalancesFromTrusted(w.balance.String()), nil
}

func (w *fakeWallet) Tx(_ context.Context, txid string) (*chain.WalletTx, error) {
	if tx, ok := w.txs[txid]; ok {
		return tx, nil
	}

	return nil, fmt.Errorf("Invalid or non-wallet transaction id")
}

func (w *fakeWallet) ListTransactions(context.Context, int64, int64) ([]chain.WalletEntry, error) {
	return w.entries, nil
}

func (w *fakeWallet) AddressInfo(_ context.Context, address string) (*chain.AddressInfo, error) {
	return &chain.AddressInfo{Address: address, Solvable: w.solvable}, nil
}

func (w *fakeWallet) ImportDescriptor(_ context.Context, descriptor string) (*chain.ImportResult, error) {
	if w.failImprt {
		return &chain.ImportResult{
			Success: false,
			Error: &struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}{Code: -4, Message: "Cannot import descriptor without private keys to a wallet with private keys enabled"},
		}, nil
	}

	w.imported = append(w.imported, descriptor)
	w.chain.events = append(w.chain.events, "import")

	return &chain.ImportResult{Success: true}, nil
}

func (w *fakeWallet) SendToAddress(
	_ context.Context, address string, sats amount.Sats, feeRate *decimal.Dec,
) (string, error) {
	if w.chain.failSend {
		return "", fmt.Errorf("%s", w.chain.sendError)
	}

	rate := ""
	if feeRate != nil {
		rate = feeRate.String()
	}

	w.chain.events = append(w.chain.events, "send")

	w.sent = append(w.sent, sent{address: address, sats: sats, feeRate: rate})

	txid := strings.Repeat("aa", 32)
	tx := &chain.Tx{
		TxID: txid,
		Vout: []chain.Vout{{N: 0, Value: amount.Coin(sats.Coin().String())}},
	}
	tx.Vout[0].ScriptPubKey.Address = address

	w.chain.txs[txid] = tx
	w.chain.utxos[fmt.Sprintf("%s:0", txid)] = &chain.TxOut{Confirmations: 0}

	if w.txs == nil {
		w.txs = map[string]*chain.WalletTx{}
	}

	w.txs[txid] = &chain.WalletTx{TxID: txid, Hex: txid}
	w.entries = append(w.entries, chain.WalletEntry{
		Address: address, Category: "send", TxID: txid,
		Amount: amount.Coin("-" + sats.Coin().String()),
	})

	return txid, nil
}

func (w *fakeWallet) NewAddress(context.Context) (string, error) {
	return "bcrt1qminingaddress", nil
}

func (w *fakeWallet) Decode(ctx context.Context, hexTx string) (*chain.Tx, error) {
	return w.chain.DecodeRaw(ctx, hexTx)
}

// ProcessPSBT is part of the wallet surface; funding never signs a PSBT.
func (w *fakeWallet) ProcessPSBT(context.Context, string, string) (*chain.ProcessPSBTResult, error) {
	return nil, fmt.Errorf("funding does not sign PSBTs")
}

// testSwap is a swap whose HTLC the fake node can derive.
func testSwap() *model.Swap {
	str := func(s string) *string { return &s }
	num := func(n int64) *int64 { return &n }

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

func fundedOptions(chain0 chain.Chain) Options {
	return Options{
		Wallet:  "alice",
		Intents: NewMemStore(),
	}
}

// TestFundBroadcastsAndVerifies walks the happy path for both chains.
func TestFundBroadcastsAndVerifies(t *testing.T) {
	for _, ch := range []chain.Chain{chain.BTC, chain.Blake} {
		t.Run(string(ch), func(t *testing.T) {
			node := newFakeChain()
			swap := testSwap()
			store := NewMemStore()

			wallet := node.Bind("alice").(*fakeWallet)
			wallet.balance = decimal.MustParse("5")

			receipt, err := Fund(context.Background(), node, swap, ch, Options{
				Wallet: "alice", Intents: store, AutoMine: 1,
			})

			if err != nil {
				t.Fatal(err)
			}

			if receipt.Pending || receipt.Recovered {
				t.Fatalf("pending=%v recovered=%v", receipt.Pending, receipt.Recovered)
			}

			if receipt.TxID == "" || receipt.BlockHash == "" {
				t.Fatalf("receipt = %+v", receipt)
			}

			if receipt.FeeSource != "dynamic" {
				t.Fatalf("fee source = %q", receipt.FeeSource)
			}

			// The intent survives with the broadcast identity, because the
			// caller has not yet recorded the outpoint.
			intent, found, err := ReadIntent(context.Background(), store, swap.SwapID, ch)

			if err != nil || !found {
				t.Fatalf("intent: found=%v err=%v", found, err)
			}

			if intent.TxID != receipt.TxID || intent.Vout == nil {
				t.Fatalf("intent = %+v", intent)
			}
		})
	}
}

// TestFundRecordsTheIntentWithBroadcastIdentity is the crash-window contract:
// after Fund returns, the intent names the transaction it sent.
func TestFundRecordsTheIntentWithBroadcastIdentity(t *testing.T) {
	node := newFakeChain()
	swap := testSwap()
	store := NewMemStore()

	node.Bind("alice").(*fakeWallet).balance = decimal.MustParse("5")

	opts := Options{Wallet: "alice", Intents: store}

	receipt, err := Fund(context.Background(), node, swap, chain.Blake, opts)

	if err != nil {
		t.Fatal(err)
	}

	intent, found, err := ReadIntent(context.Background(), store, swap.SwapID, chain.Blake)

	if err != nil || !found {
		t.Fatalf("intent: found=%v err=%v", found, err)
	}

	if intent.TxID != receipt.TxID {
		t.Fatalf("intent txid = %q, receipt = %q", intent.TxID, receipt.TxID)
	}

	if intent.Vout == nil || *intent.Vout != int32(receipt.Vout) {
		t.Fatalf("intent vout = %v, receipt = %d", intent.Vout, receipt.Vout)
	}

	if intent.Address != receipt.Address {
		t.Fatalf("intent address = %q, receipt = %q", intent.Address, receipt.Address)
	}

	if intent.BaseUnits != swap.BlakeAmountBaseUnits {
		t.Fatalf("intent base units = %d", intent.BaseUnits)
	}
}

// TestFundRecoversInsteadOfPayingTwice is the reason FindPayment runs before
// the balance preflight: the wallet may already have paid, and the record may
// not know it.
func TestFundRecoversInsteadOfPayingTwice(t *testing.T) {
	node := newFakeChain()
	swap := testSwap()
	store := NewMemStore()

	derived, err := contract.Derive(context.Background(), node, swap, chain.Blake)

	if err != nil {
		t.Fatal(err)
	}

	// A crashed run: the intent and the payment exist, the swap record does
	// not. The wallet is now empty, which is exactly the state the preflight
	// would refuse.
	wallet := node.Bind("alice").(*fakeWallet)
	payInto(t, node, wallet, derived.Address, swap.BlakeAmountBaseUnits)

	if _, err := WriteIntent(context.Background(), store, swap, chain.Blake, derived.Address); err != nil {
		t.Fatal(err)
	}

	receipt, err := Fund(context.Background(), node, swap, chain.Blake, Options{
		Wallet: "alice", Intents: store,
	})

	if err != nil {
		t.Fatal(err)
	}

	if !receipt.Recovered {
		t.Fatal("an existing payment was not recovered")
	}

	if len(wallet.sent) != 0 {
		t.Fatalf("funding sent %d extra payments", len(wallet.sent))
	}

	intent, found, _ := ReadIntent(context.Background(), store, swap.SwapID, chain.Blake)

	if !found || intent.TxID != receipt.TxID {
		t.Fatalf("intent not extended: %+v", intent)
	}
}

// TestFundRefusesRecoveryWithoutAnIntent: a payment this wallet made but no
// intent for is not proof that *this* process started the funding — a restored
// or replaced wallet looks the same — so it is refused rather than adopted.
func TestFundRefusesRecoveryWithoutAnIntent(t *testing.T) {
	node := newFakeChain()
	swap := testSwap()

	derived, err := contract.Derive(context.Background(), node, swap, chain.Blake)

	if err != nil {
		t.Fatal(err)
	}

	wallet := node.Bind("alice").(*fakeWallet)
	payInto(t, node, wallet, derived.Address, swap.BlakeAmountBaseUnits)

	_, err = Fund(context.Background(), node, swap, chain.Blake, Options{
		Wallet: "alice", Intents: NewMemStore(),
	})

	if err == nil || !strings.Contains(err.Error(), "intent disappeared") {
		t.Fatalf("err = %v", err)
	}
}

// TestFundRefusesInsufficientBalance keeps the friendly preflight honest, and
// refuses before writing an intent: a refused funding must leave no marker that
// later looks like a spend may have started.
func TestFundRefusesInsufficientBalance(t *testing.T) {
	node := newFakeChain()
	swap := testSwap()
	store := NewMemStore()

	node.Bind("alice").(*fakeWallet).balance = decimal.MustParse("0.005")

	_, err := Fund(context.Background(), node, swap, chain.Blake, Options{
		Wallet: "alice", Intents: store,
	})

	if err == nil || !strings.Contains(err.Error(), "Insufficient trusted") {
		t.Fatalf("err = %v", err)
	}

	if keys := store.Keys(); len(keys) != 0 {
		t.Fatalf("a refused funding left an intent: %v", keys)
	}
}

// TestFundRefusesAfterTheTimeout: funding a contract whose timelock has passed
// pays for an HTLC the counterparty can already take back.
func TestFundRefusesAfterTheTimeout(t *testing.T) {
	node := newFakeChain()
	swap := testSwap()
	node.height = swap.BlakeTimeout

	_, err := Fund(context.Background(), node, swap, chain.Blake, fundedOptions(chain.Blake))

	if err == nil || !strings.Contains(err.Error(), "already been reached") {
		t.Fatalf("err = %v", err)
	}
}

// TestFundPendingIsNotAdvanced: an unconfirmed funding is a wait, and the
// receipt says so rather than claiming the outpoint is confirmed.
func TestFundPendingIsNotAdvanced(t *testing.T) {
	node := newFakeChain()
	swap := testSwap()
	store := NewMemStore()

	node.Bind("alice").(*fakeWallet).balance = decimal.MustParse("5")

	receipt, err := Fund(context.Background(), node, swap, chain.Blake, Options{
		Wallet: "alice", Intents: store,
	})

	if err != nil {
		t.Fatal(err)
	}

	if !receipt.Pending {
		t.Fatal("an unconfirmed funding was reported as confirmed")
	}

	if receipt.BlockHash != "" || receipt.Confirmations != 0 {
		t.Fatalf("receipt = %+v", receipt)
	}
}

// TestFundAutoMineConfirms covers the rig path: mine, re-read the wallet, then
// prove the anchor.
func TestFundAutoMineConfirms(t *testing.T) {
	node := newFakeChain()
	swap := testSwap()

	node.Bind("alice").(*fakeWallet).balance = decimal.MustParse("5")

	receipt, err := Fund(context.Background(), node, swap, chain.Blake, Options{
		Wallet: "alice", Intents: NewMemStore(), AutoMine: 1,
	})

	if err != nil {
		t.Fatal(err)
	}

	if receipt.Pending || receipt.Mined != 1 {
		t.Fatalf("receipt = %+v", receipt)
	}

	if receipt.Confirmations < 1 {
		t.Fatalf("confirmations = %d", receipt.Confirmations)
	}
}

// TestFundWatchOnlyFailureIsReportedNotFatal: the keys are present right now,
// so a node refusing the watch-only copy must not stop the funding — but it
// must be visible.
func TestFundWatchOnlyFailureIsReportedNotFatal(t *testing.T) {
	node := newFakeChain()
	swap := testSwap()

	node.Bind("alice").(*fakeWallet).balance = decimal.MustParse("5")
	node.EnsureWallet(context.Background(), "alice-watch", true)
	node.Bind("alice-watch").(*fakeWallet).failImprt = true

	receipt, err := Fund(context.Background(), node, swap, chain.Blake, fundedOptions(chain.Blake))

	if err != nil {
		t.Fatal(err)
	}

	if receipt.Watch.Imported {
		t.Fatal("a failed import was reported as imported")
	}

	if !strings.Contains(receipt.Watch.Reason, "private keys") {
		t.Fatalf("reason = %q", receipt.Watch.Reason)
	}

	if receipt.TxID == "" {
		t.Fatal("the funding did not continue after the import failure")
	}
}

// TestFundImportHappensBeforeTheBroadcast: the order is the point. If the
// import were after the broadcast, a crash between them would leave a funded
// HTLC nothing on this machine can see.
func TestFundImportHappensBeforeTheBroadcast(t *testing.T) {
	node := newFakeChain()
	swap := testSwap()

	watch := node.Bind("alice-watch").(*fakeWallet)
	watch.failImprt = false

	node.Bind("alice").(*fakeWallet).balance = decimal.MustParse("5")

	receipt, err := Fund(context.Background(), node, swap, chain.Blake, fundedOptions(chain.Blake))

	if err != nil {
		t.Fatal(err)
	}

	if !receipt.Watch.Imported || len(watch.imported) != 1 {
		t.Fatalf("watch status = %+v", receipt.Watch)
	}

	if watch.imported[0] != receipt.Descriptor {
		t.Fatalf("imported %q, expected %q", watch.imported[0], receipt.Descriptor)
	}

	// Order, not just presence: an import after the broadcast would leave a
	// crash window in which the HTLC is funded and nothing local can see it.
	order := node.events

	if len(order) != 2 || order[0] != "import" || order[1] != "send" {
		t.Fatalf("side effects happened in the order %v", order)
	}
}

// TestFundExplicitFeeRateOutranksTheEstimator: the client driving the swap
// decides what it pays, and the node's estimate is only a default.
func TestFundExplicitFeeRateOutranksTheEstimator(t *testing.T) {
	node := newFakeChain()
	swap := testSwap()

	wallet := node.Bind("alice").(*fakeWallet)
	wallet.balance = decimal.MustParse("5")

	rate := decimal.MustParse("7.5")

	receipt, err := Fund(context.Background(), node, swap, chain.Blake, Options{
		Wallet: "alice", Intents: NewMemStore(), FeeRate: &rate,
	})

	if err != nil {
		t.Fatal(err)
	}

	if receipt.FeeSource != "explicit" || receipt.FeeRate.String() != "7.5" {
		t.Fatalf("rate = %s from %s", receipt.FeeRate, receipt.FeeSource)
	}

	if wallet.sent[0].feeRate != "7.5" {
		t.Fatalf("the node was asked for %s sat/vB", wallet.sent[0].feeRate)
	}
}

// TestFundAppliesTheBitcoinWalletFloor: Core's wallet refuses to build a
// transaction below 1 sat/vB, so the floor is named rather than left to a
// broadcast failure.
func TestFundAppliesTheBitcoinWalletFloor(t *testing.T) {
	node := newFakeChain()
	swap := testSwap()
	node.feeRate = "0.00001000" // 1 sat/vB

	node.Bind("bob").(*fakeWallet).balance = decimal.MustParse("5")

	floor := decimal.MustParse("2")

	receipt, err := Fund(context.Background(), node, swap, chain.BTC, Options{
		Wallet: "bob", Intents: NewMemStore(), MinFeeRate: &floor,
	})

	if err != nil {
		t.Fatal(err)
	}

	if receipt.FeeSource != "wallet-minimum" || receipt.FeeRate.String() != "2" {
		t.Fatalf("rate = %s from %s", receipt.FeeRate, receipt.FeeSource)
	}
}

// TestFindPaymentIgnoresDecoys: recovery compares the exact derived address and
// the exact amount in base units, so a payment that is one satoshi off is not
// this contract.
func TestFindPaymentIgnoresDecoys(t *testing.T) {
	node := newFakeChain()
	swap := testSwap()
	wallet := node.Bind("alice").(*fakeWallet)

	derived, err := contract.Derive(context.Background(), node, swap, chain.Blake)

	if err != nil {
		t.Fatal(err)
	}

	payInto(t, node, wallet, derived.Address, swap.BlakeAmountBaseUnits+1)

	payment, err := FindPayment(context.Background(), wallet, derived.Address, swap.BlakeAmountBaseUnits)

	if err != nil {
		t.Fatal(err)
	}

	if payment != nil {
		t.Fatalf("a decoy was adopted: %+v", payment)
	}
}

// TestVerifyAnchorRejectsAnInactiveTransaction: a reorged funding is not
// funding, and the state must not advance on it.
func TestVerifyAnchorRejectsAnInactiveTransaction(t *testing.T) {
	node := newFakeChain()
	swap := testSwap()

	derived, err := contract.Derive(context.Background(), node, swap, chain.Blake)

	if err != nil {
		t.Fatal(err)
	}

	txid := strings.Repeat("bb", 32)
	inactive := false

	tx := &chain.Tx{
		TxID:          txid,
		BlockHash:     strings.Repeat("cb", 32),
		Confirmations: 1,
		InActiveChain: &inactive,
		Vout:          []chain.Vout{{N: 0, Value: amount.Coin("0.01000000")}},
	}
	tx.Vout[0].ScriptPubKey.Address = derived.Address
	node.txs[txid] = tx

	_, err = VerifyAnchor(context.Background(), node, swap, chain.Blake, txid, 0, tx.BlockHash)

	if err == nil || !strings.Contains(err.Error(), "not on the active chain") {
		t.Fatalf("err = %v", err)
	}
}

// TestResolveIntentWalksTheStates covers the leftover-intent decision.
func TestResolveIntentWalksTheStates(t *testing.T) {
	node := newFakeChain()
	swap := testSwap()
	store := NewMemStore()

	resolution, err := ResolveIntent(context.Background(), node, swap, chain.Blake, "alice", store)

	if err != nil || resolution != ResolutionNone {
		t.Fatalf("none: %v %v", resolution, err)
	}

	derived, err := contract.Derive(context.Background(), node, swap, chain.Blake)

	if err != nil {
		t.Fatal(err)
	}

	if _, err := WriteIntent(context.Background(), store, swap, chain.Blake, derived.Address); err != nil {
		t.Fatal(err)
	}

	resolution, _ = ResolveIntent(context.Background(), node, swap, chain.Blake, "alice", store)

	if resolution != ResolutionAbsent {
		t.Fatalf("absent: %v", resolution)
	}

	wallet := node.Bind("alice").(*fakeWallet)
	payInto(t, node, wallet, derived.Address, swap.BlakeAmountBaseUnits)

	resolution, _ = ResolveIntent(context.Background(), node, swap, chain.Blake, "alice", store)

	if resolution != ResolutionPending {
		t.Fatalf("pending: %v", resolution)
	}

	// Confirming the payment resolves the intent and clears it.
	if _, err := node.MineToAddress(context.Background(), 1, "bcrt1qminingaddress"); err != nil {
		t.Fatal(err)
	}

	// The wallet entry has to learn the block hash, which a real node reports
	// through gettransaction.
	for _, tx := range wallet.txs {
		tx.BlockHash = fmt.Sprintf("%064x", node.height)
		tx.Confirmations = 1
	}

	resolution, _ = ResolveIntent(context.Background(), node, swap, chain.Blake, "alice", store)

	if resolution != ResolutionConfirmed {
		t.Fatalf("confirmed: %v", resolution)
	}

	if keys := store.Keys(); len(keys) != 0 {
		t.Fatalf("a confirmed intent was not cleared: %v", keys)
	}
}

// TestResolveIntentRefusesAForeignIntent: an intent whose secret hash is not
// this swap's belongs to a different swap or a corrupted record, and guessing
// which would be the same as trusting it.
func TestResolveIntentRefusesAForeignIntent(t *testing.T) {
	node := newFakeChain()
	swap := testSwap()
	store := NewMemStore()

	foreign, err := json.Marshal(Intent{
		SwapID: swap.SwapID, Chain: chain.Blake, Address: "bcrt1qforeign",
		Amount: "0.01", BaseUnits: 1_000_000, SecretHash: strings.Repeat("ff", 32),
	})

	if err != nil {
		t.Fatal(err)
	}

	if err := store.Write(context.Background(), swap.SwapID, chain.Blake, foreign); err != nil {
		t.Fatal(err)
	}

	resolution, _ := ResolveIntent(context.Background(), node, swap, chain.Blake, "alice", store)

	if resolution != ResolutionUnknown {
		t.Fatalf("resolution = %v", resolution)
	}
}

// payInto makes the wallet look as if a previous run paid the contract.
func payInto(t *testing.T, node *fakeChain, wallet *fakeWallet, address string, baseUnits int64) {
	t.Helper()

	txid := strings.Repeat("cc", 32)

	tx := &chain.Tx{
		TxID: txid,
		Vout: []chain.Vout{{N: 0, Value: amount.Coin(amount.Sats(baseUnits).Coin().String())}},
	}
	tx.Vout[0].ScriptPubKey.Address = address

	node.txs[txid] = tx

	if wallet.txs == nil {
		wallet.txs = map[string]*chain.WalletTx{}
	}

	wallet.txs[txid] = &chain.WalletTx{TxID: txid, Hex: txid}
	wallet.entries = append(wallet.entries, chain.WalletEntry{
		Address:  address,
		Category: "send",
		TxID:     txid,
		Amount:   amount.Coin("-" + amount.Sats(baseUnits).Coin().String()),
	})
}
