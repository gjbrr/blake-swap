//go:build !js

package settle

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/decimal"
	"github.com/gjbrr/blake-swap/pkg/fees"
)

// rigNode builds a node against the local regtest rig, or skips.
//
// It is duplicated from pkg/chain's test rather than exported from the package:
// a test helper in a shipped package would be API that exists only for tests.
func rigNode(t *testing.T, name chain.Chain) *chain.Node {
	t.Helper()

	root := os.Getenv("BLAKE_SWAP_TEST_ROOT")

	if root == "" {
		home, err := os.UserHomeDir()

		if err != nil {
			t.Skip("no home directory")
		}

		root = filepath.Join(home, ".blake-swap-test")
	}

	envPath := filepath.Join(root, "rig-env.json")

	data, err := os.ReadFile(envPath)

	if err != nil {
		t.Skipf("no test rig at %s", envPath)
	}

	var env struct {
		BTCRPCPort   int    `json:"btc_rpcport"`
		BlakeRPCPort int    `json:"blake_rpcport"`
		BTCDatadir   string `json:"btc_datadir"`
		BlakeDatadir string `json:"blake_datadir"`
	}

	if err := json.Unmarshal(data, &env); err != nil {
		t.Skipf("rig-env.json is unreadable: %v", err)
	}

	port := env.BTCRPCPort
	datadir := env.BTCDatadir

	if name == chain.Blake {
		port = env.BlakeRPCPort
		datadir = env.BlakeDatadir
	}

	if port == 0 || datadir == "" {
		t.Skipf("the rig does not describe the %s node", name)
	}

	cookiePath := filepath.Join(datadir, "regtest", ".cookie")

	cookie, err := os.ReadFile(cookiePath)

	if err != nil {
		t.Skipf("no RPC cookie at %s", cookiePath)
	}

	user, password, ok := strings.Cut(strings.TrimSpace(string(cookie)), ":")

	if !ok {
		t.Skip("the RPC cookie is malformed")
	}

	node, err := chain.New(name, chain.Config{
		URL:      "http://127.0.0.1:" + strconv.Itoa(port) + "/",
		User:     user,
		Password: password,
	})

	if err != nil {
		t.Skip(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := node.Height(ctx); err != nil {
		t.Skipf("the %s node is not answering: %v", name, err)
	}

	return node
}

// TestRigSettlementPipeline builds, signs, finalizes and broadcasts a real
// settlement through the real node.
//
// The fee loop, the PSBT hand-off and the finalizer are only proven by a node
// that actually signs: a test double can agree with the engine about a
// transaction that no chain would accept.
func TestRigSettlementPipeline(t *testing.T) {
	node := rigNode(t, chain.BTC)
	ctx := context.Background()

	wallets, err := node.ListWallets(ctx)

	if err != nil {
		t.Fatal(err)
	}

	if len(wallets) == 0 {
		t.Skip("the rig has no wallet loaded")
	}

	walletName := wallets[0]
	wallet := node.Bind(walletName)

	unspent, err := node.WithWallet(walletName).ListUnspent(ctx)

	if err != nil {
		t.Fatal(err)
	}

	var spendable *chain.Unspent

	for i := range unspent {
		entry := unspent[i]

		if entry.Confirmations < 1 || !entry.Spendable {
			continue
		}

		sats, err := entry.Sats()

		// A realistic settlement-sized input: large enough to pay a fee,
		// small enough to stay inside the fee cap.
		if err == nil && sats > 20_000 {
			spendable = &entry

			break
		}
	}

	if spendable == nil {
		t.Skip("the rig has no confirmed spendable output")
	}

	inputSats, err := spendable.Sats()

	if err != nil {
		t.Fatal(err)
	}

	destination, err := wallet.NewAddress(ctx)

	if err != nil {
		t.Fatal(err)
	}

	// The wallet's own descriptor for the input address stands in for an HTLC
	// descriptor: utxoupdatepsbt and walletprocesspsbt take the same path, and
	// this wallet can actually sign for the output it selected.
	info, err := wallet.AddressInfo(ctx, spendable.Address)

	if err != nil {
		t.Fatal(err)
	}

	if info.Desc == "" {
		t.Skip("the rig wallet reports no descriptor for its own output")
	}

	rate := decimal.MustParse("2")

	plan, err := Refund(ctx, node, Request{
		Chain:       chain.BTC,
		Funding:     chain.Outpoint{TxID: spendable.TxID, Vout: spendable.Vout},
		Destination: destination,
		Descriptor:  info.Desc,
		InputSats:   int64(inputSats),
		Wallet:      walletName,
	}, Options{FeeRate: &rate})

	if err != nil {
		t.Fatalf("the settlement did not build: %v", err)
	}

	required, err := fees.FeeForVsize(int(plan.Vsize), rate)

	if err != nil {
		t.Fatal(err)
	}

	if plan.FeeSats < required {
		t.Fatalf("fee %d does not cover %d for %d vB", plan.FeeSats, required, plan.Vsize)
	}

	if plan.OutputSats != int64(inputSats)-plan.FeeSats {
		t.Fatalf("the output pays %d, input %d, fee %d", plan.OutputSats, inputSats, plan.FeeSats)
	}

	txid, fee, err := Broadcast(ctx, node, chain.BTC, plan, int64(inputSats))

	if err != nil {
		t.Fatalf("the settlement did not broadcast: %v", err)
	}

	if fee != plan.FeeSats {
		t.Fatalf("the chain charges %d, the plan expected %d", fee, plan.FeeSats)
	}

	if _, err := node.MempoolEntry(ctx, txid); err != nil {
		t.Fatalf("the broadcast transaction is not in the mempool: %v", err)
	}

	// Confirm it, so the rig is left as the Python suites expect: a mempool
	// entry they did not create could change their timing assumptions.
	hashes, err := node.MineToAddress(ctx, 1, destination)

	if err != nil {
		t.Fatalf("mining the confirmation failed: %v", err)
	}

	if len(hashes) != 1 {
		t.Fatalf("mining returned %d hashes", len(hashes))
	}

	// The rig runs without txindex, so a confirmed transaction is only
	// reachable through the block that holds it.
	confirmed, err := node.RawTx(ctx, txid, hashes[0])

	if err != nil {
		t.Fatal(err)
	}

	if confirmed.Confirmations < 1 {
		t.Fatalf("confirmations = %d", confirmed.Confirmations)
	}

	if confirmed.VSize != plan.Vsize {
		t.Fatalf("the chain measures %d vB, the plan said %d", confirmed.VSize, plan.Vsize)
	}

	if confirmed.TxID != txid {
		t.Fatalf("mine returned %s, the chain has %s", txid, confirmed.TxID)
	}

	// A settlement output smaller than its input by exactly the fee is the
	// property that makes the fee loop meaningful.
	if got := int64(inputSats) - plan.OutputSats; got != fee {
		t.Fatalf("input minus output is %d, measured fee %d", got, fee)
	}

}
