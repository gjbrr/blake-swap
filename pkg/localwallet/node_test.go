package localwallet

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/walletdep"

	"git.smesh.lol/tinywallet/pkg/psbt"
	"git.smesh.lol/tinywallet/pkg/tx"
)

// fakeWallet records what the adapter asks of the local wallet.
type fakeWallet struct {
	height   int64
	coins    []Coin
	address  string
	pubkey   []byte
	sent     string
	sendArgs struct {
		chain, address string
		sats, fee      uint64
	}
	signed       []byte
	signedN      int
	signedPolicy Policy
	raw          string
	deriveErr    error
}

func (f *fakeWallet) Height(context.Context, string) (int64, error) { return f.height, nil }

func (f *fakeWallet) Coins(context.Context, string) ([]Coin, error) { return f.coins, nil }

func (f *fakeWallet) Address(context.Context, string) (string, []byte, error) {
	return f.address, f.pubkey, nil
}

func (f *fakeWallet) Send(_ context.Context, chainName, address string, sats, fee uint64, _ Policy) (string, error) {
	f.sendArgs.chain, f.sendArgs.address = chainName, address
	f.sendArgs.sats, f.sendArgs.fee = sats, fee

	return f.sent, nil
}

func (f *fakeWallet) Derive(context.Context, string, HTLCTerms) (Contract, error) {
	return Contract{}, f.deriveErr
}

func (f *fakeWallet) Watch(context.Context, string, []byte) error { return nil }

func (f *fakeWallet) SignPSBT(_ context.Context, _ string, _ []byte, policy Policy) ([]byte, int, error) {
	f.signedPolicy = policy

	return f.signed, f.signedN, nil
}

func (f *fakeWallet) RawTx(context.Context, string, string) (string, error) { return f.raw, nil }

// adapter builds the node surface over a fake wallet and a node with no client: only the
// wallet-backed calls are exercised, and every chain-backed call is the node's own.
func adapter(t *testing.T) (*Node, *fakeWallet) {
	t.Helper()

	wallet := &fakeWallet{
		height:  24115,
		address: "bcrt1qwallet",
		pubkey:  []byte{2, 1, 2, 3},
		sent:    "aa11",
		coins: []Coin{
			{Chain: "btc", TxID: "aa11", Vout: 0, Value: 60_000, Height: 24000, Script: []byte{0, 20, 1}},
			{Chain: "btc", TxID: "bb22", Vout: 1, Value: 40_000, Height: 0, Script: []byte{0, 20, 2}},
		},
		signed:  []byte("signed-psbt"),
		signedN: 1,
		raw:     "0200000001deadbeef",
	}

	return New(&chain.Node{Chain: chain.BTC}, wallet), wallet
}

// TestLocalWalletAnswersFromTheWallet: the wallet-backed half of the surface is answered
// locally, including the balance, a send, a signed PSBT and a transaction's bytes.
func TestLocalWalletAnswersFromTheWallet(t *testing.T) {
	ctx := context.Background()
	node, wallet := adapter(t)

	height, err := node.Height(ctx)

	if err != nil || height != 24115 {
		t.Fatalf("height %d, %v", height, err)
	}

	bound := node.Bind("btc")

	balances, err := bound.Balances(ctx)

	if err != nil {
		t.Fatal(err)
	}

	trusted, err := balances.TrustedAmount()

	if err != nil {
		t.Fatal(err)
	}

	if fixed, err := trusted.Fixed(8); err != nil || fixed != "0.00100000" {
		t.Fatalf("balance %s, %v", fixed, err)
	}

	txid, err := bound.SendToAddress(ctx, "bcrt1qcontract", amount.Sats(50_000), nil)

	if err != nil || txid != "aa11" {
		t.Fatalf("send %q, %v", txid, err)
	}

	if wallet.sendArgs.chain != "btc" || wallet.sendArgs.address != "bcrt1qcontract" ||
		wallet.sendArgs.sats != 50_000 || wallet.sendArgs.fee != 1_000 {
		t.Fatalf("send args %+v", wallet.sendArgs)
	}

	psbtRaw := witnessPSBT(t, 70_000)

	result, err := bound.ProcessPSBT(ctx, string(psbtRaw), "ALL")

	if err != nil || result.PSBT != "signed-psbt" {
		t.Fatalf("process psbt %+v, %v", result, err)
	}

	// The policy is the wallet's rule about what it will sign: at most the PSBT's own inputs,
	// at most a tenth of them as fee.
	if wallet.signedPolicy.MaxSpend != 70_000 || wallet.signedPolicy.MaxFee != 7_000 {
		t.Fatalf("the signing policy is %+v", wallet.signedPolicy)
	}

	walletTx, err := bound.Tx(ctx, "aa11")

	if err != nil || walletTx.Hex != "0200000001deadbeef" {
		t.Fatalf("wallet tx %+v, %v", walletTx, err)
	}

	entries, err := bound.ListTransactions(ctx, 100, 0)

	if err != nil || len(entries) != 2 {
		t.Fatalf("%d entries, %v", len(entries), err)
	}

	if got := entries[0]; got.Category != "receive" || got.TxID != "aa11" || got.Vout != 0 {
		t.Fatalf("entry %+v", got)
	}

	info, err := bound.AddressInfo(ctx, "bcrt1qwallet")

	if err != nil || !info.IsMine || info.Pubkey != "02010203" {
		t.Fatalf("address info %+v, %v", info, err)
	}
}

// TestWhatHasNoLocalEquivalentRefusesByName: every node-wallet operation this build cannot do
// says so, with the operation's name and the walletdep sentinel — not a connection error that
// reads as a broken node.
func TestWhatHasNoLocalEquivalentRefusesByName(t *testing.T) {
	ctx := context.Background()
	node, _ := adapter(t)
	bound := node.Bind("btc")

	refusals := []struct {
		operation string
		call      func() error
	}{
		{"listwallets", func() error { _, err := node.ListWallets(ctx); return err }},
		{"ensurewallet", func() error { return node.EnsureWallet(ctx, "btc", true) }},
		{"getwalletinfo", func() error { _, err := node.GetWalletInfo(ctx, "btc"); return err }},
		{"getblockchaininfo-for-import", func() error { _, err := node.ChainInfo(ctx); return err }},
		{"getdescriptorinfo", func() error { _, err := node.DescriptorInfo(ctx, "wsh(x)"); return err }},
		{"deriveaddresses", func() error { _, err := node.DeriveAddresses(ctx, "wsh(x)"); return err }},
		{"importdescriptors", func() error { _, err := bound.ImportDescriptor(ctx, "wsh(x)", chain.ImportEverything()); return err }},
		{"gettransaction", func() error { _, err := node.WalletTxIn(ctx, "btc", "aa11"); return err }},
	}

	for _, refusal := range refusals {
		err := refusal.call()

		if !errors.Is(err, walletdep.ErrNoWalletService) {
			t.Fatalf("%s: %v", refusal.operation, err)
		}

		if !strings.Contains(err.Error(), refusal.operation) {
			t.Fatalf("%s: the refusal does not name the operation: %v", refusal.operation, err)
		}
	}
}

// TestProcessPSBTRefusesToPretend: a PSBT the wallet signed nothing in is an error, not a
// successful signing with an unchanged file.
func TestProcessPSBTRefusesToPretend(t *testing.T) {
	ctx := context.Background()
	node, wallet := adapter(t)
	wallet.signedN = 0

	if _, err := node.Bind("btc").ProcessPSBT(ctx, string(witnessPSBT(t, 70_000)), "ALL"); err == nil {
		t.Fatal("a PSBT with no wallet input was reported as signed")
	}

	// A PSBT the wallet cannot bound — no inputs, or no amounts — is refused rather than
	// signed blind.
	if _, err := node.Bind("btc").ProcessPSBT(ctx, "cHNidP8=", "ALL"); err == nil {
		t.Fatal("a PSBT that is not a PSBT was signed")
	}
}

// keep fmt imported for the adapter's error formatting.
var _ = fmt.Sprintf

// witnessPSBT builds a PSBT with one input whose value is known, which is what a node-driven
// settlement hands the wallet after utxoupdatepsbt.
func witnessPSBT(t *testing.T, amount uint64) []byte {
	t.Helper()

	transaction := tx.New()
	transaction.AddInput(tx.Outpoint{Hash: [32]byte{9}, Index: 0}, 0xfffffffe)
	transaction.AddOutput(amount-1_000, []byte{0x00, 0x14, 1, 2, 3})

	raw, err := psbt.Write(transaction, []psbt.SegwitInput{{
		Amount:       amount,
		ScriptPubKey: []byte{0x00, 0x14, 1, 2, 3, 4},
	}})

	if err != nil {
		t.Fatal(err)
	}

	return raw
}
