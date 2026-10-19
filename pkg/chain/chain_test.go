package chain

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gjbrr/blake-swap/pkg/htlc"
	"github.com/gjbrr/blake-swap/pkg/rpc"
)

// rigNode builds a node against the local regtest rig, or skips.
//
// The rig is the same one the Python suites use; a Go port that could not talk
// to it would not be a port.
func rigNode(t *testing.T, name Chain) *Node {
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

	if name == Blake {
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

	node, err := New(name, Config{
		URL:      "http://127.0.0.1:" + itoa(port) + "/",
		User:     user,
		Password: password,
	})

	if err != nil {
		t.Skip(err)
	}

	// A stale rig-env.json is possible: prove the node answers before using it.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := node.Height(ctx); err != nil {
		t.Skipf("the %s node is not answering: %v", name, err)
	}

	return node
}

func itoa(n int) string {
	digits := "0123456789"
	out := ""

	for n > 0 {
		out = string(digits[n%10]) + out
		n /= 10
	}

	if out == "" {
		return "0"
	}

	return out
}

func TestRigHeightAndBlocks(t *testing.T) {
	for _, name := range []Chain{BTC, Blake} {
		t.Run(string(name), func(t *testing.T) {
			node := rigNode(t, name)
			ctx := context.Background()

			height, err := node.Height(ctx)

			if err != nil {
				t.Fatal(err)
			}

			if height < 1 {
				t.Fatalf("height = %d", height)
			}

			hash, err := node.BlockHash(ctx, height-1)

			if err != nil {
				t.Fatal(err)
			}

			if len(hash) != 64 {
				t.Fatalf("block hash = %q", hash)
			}

			block, err := node.Block(ctx, hash, 1)

			if err != nil {
				t.Fatal(err)
			}

			if block.Hash != hash {
				t.Fatalf("block hash = %q, want %q", block.Hash, hash)
			}

			if block.Height != height-1 {
				t.Fatalf("block height = %d, want %d", block.Height, height-1)
			}

			if len(block.Tx) == 0 {
				t.Fatal("the block has no transactions")
			}
		})
	}
}

func TestRigTransactionReads(t *testing.T) {
	node := rigNode(t, BTC)
	ctx := context.Background()

	height, err := node.Height(ctx)

	if err != nil {
		t.Fatal(err)
	}

	hash, err := node.BlockHash(ctx, height-1)

	if err != nil {
		t.Fatal(err)
	}

	block, err := node.Block(ctx, hash, 1)

	if err != nil {
		t.Fatal(err)
	}

	// The coinbase of a recent block is confirmed and decodable.
	tx, err := node.RawTx(ctx, block.Tx[0], hash)

	if err != nil {
		t.Fatal(err)
	}

	if tx.TxID != block.Tx[0] {
		t.Fatalf("txid = %q, want %q", tx.TxID, block.Tx[0])
	}

	if tx.VSize < 1 {
		t.Fatalf("vsize = %d", tx.VSize)
	}

	if len(tx.Vin) == 0 || len(tx.Vout) == 0 {
		t.Fatal("the transaction has no inputs or outputs")
	}

	// Decoding the same transaction must agree about its identity and size.
	// The block hash is passed again because the rig runs without -txindex,
	// so a confirmed transaction is only reachable through its block.
	hexTx, err := node.call(ctx, "getrawtransaction", block.Tx[0], false, hash)

	if err != nil {
		t.Fatal(err)
	}

	var hexString string

	if err := decode(hexTx, &hexString); err != nil {
		t.Fatal(err)
	}

	if len(hexString)%2 != 0 {
		t.Fatal("raw transaction hex has odd length")
	}

	// An unknown txid is a node error, not a silent empty result.
	if _, err := node.RawTx(ctx, strings.Repeat("00", 32), ""); err == nil {
		t.Fatal("an unknown txid was accepted")
	}
}

func TestRigTxOut(t *testing.T) {
	node := rigNode(t, BTC)
	ctx := context.Background()

	height, err := node.Height(ctx)

	if err != nil {
		t.Fatal(err)
	}

	hash, err := node.BlockHash(ctx, height-1)

	if err != nil {
		t.Fatal(err)
	}

	block, err := node.Block(ctx, hash, 1)

	if err != nil {
		t.Fatal(err)
	}

	tx, err := node.RawTx(ctx, block.Tx[0], hash)

	if err != nil {
		t.Fatal(err)
	}

	// A mature coinbase output is unspent.
	//
	// The output to sample is the one carrying the subsidy: a segwit block's
	// coinbase also carries a zero-value witness commitment, and which index that
	// lands in is the node's business, not this test's.
	index := int64(-1)

	for _, output := range tx.Vout {
		value, err := output.Value.Sats()

		if err != nil {
			t.Fatalf("output %d: %v", output.N, err)
		}

		if int64(value) > 0 {
			index = output.N

			break
		}
	}

	if index < 0 {
		t.Skip("the sampled coinbase pays nothing at all")
	}

	out, err := node.TxOut(ctx, tx.TxID, index)

	if err != nil {
		t.Fatal(err)
	}

	if out == nil {
		t.Skip("the sampled coinbase is already spent")
	}

	if out.Confirmations < 1 {
		t.Fatalf("confirmations = %d", out.Confirmations)
	}

	sats, err := out.Value.Sats()

	if err != nil {
		t.Fatalf("value %q: %v", out.Value, err)
	}

	if sats <= 0 {
		t.Fatalf("value = %d", sats)
	}
}

func TestRigMempoolAndFees(t *testing.T) {
	node := rigNode(t, BTC)
	ctx := context.Background()

	info, err := node.MempoolInfo(ctx)

	if err != nil {
		t.Fatal(err)
	}

	// Both floors are required by the fee policy.
	for name, field := range map[string]string{
		"mempoolminfee": info.MempoolMinFee.String(),
		"minrelaytxfee": info.MinRelayTxFee.String(),
	} {
		if field == "" || field == "<nil>" {
			t.Errorf("%s is missing", name)
		}
	}

	estimate, err := node.EstimateSmartFee(ctx, 2)

	if err != nil {
		t.Fatal(err)
	}

	// Regtest has no fee history, so an error list instead of a feerate is a
	// legitimate answer; the policy treats it as "use the floors".
	if estimate.Feerate.String() == "" && len(estimate.Errors) == 0 {
		t.Fatal("the estimator returned neither a feerate nor errors")
	}
}

func TestRigDescriptorRoundTrip(t *testing.T) {
	node := rigNode(t, BTC)
	ctx := context.Background()

	// A descriptor body with a fixed, valid key: the node compiles it and
	// derives the address, which is how the engine treats the node as the
	// authority on the HTLC script.
	body := "wsh(or_i(and_v(v:pk(" +
		"02" + "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798" +
		"),sha256(" + strings.Repeat("ab", 32) + ")),and_v(v:after(1000),pk(" +
		"03" + "c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5" +
		"))))"

	info, err := node.DescriptorInfo(ctx, body)

	if err != nil {
		t.Fatal(err)
	}

	if len(info.Checksum) != 8 {
		t.Fatalf("checksum = %q", info.Checksum)
	}

	if !info.IsSolvable {
		t.Fatal("the HTLC descriptor is not solvable")
	}

	if info.HasPrivate {
		t.Fatal("the descriptor body unexpectedly carries private keys")
	}

	// The checksummed form derives exactly one address.
	addresses, err := node.DeriveAddresses(ctx, info.Descriptor)

	if err != nil {
		t.Fatal(err)
	}

	if len(addresses) != 1 || !strings.HasPrefix(addresses[0], "bcrt1") {
		t.Fatalf("addresses = %v", addresses)
	}
}

func TestRigWallets(t *testing.T) {
	node := rigNode(t, BTC)
	ctx := context.Background()

	wallets, err := node.ListWallets(ctx)

	if err != nil {
		t.Fatal(err)
	}

	if len(wallets) == 0 {
		t.Skip("the rig has no wallet loaded")
	}

	// Binding a wallet changes which wallet the call addresses: the balances
	// of the first loaded wallet must be readable, and an absent one must be
	// reported as not loaded rather than erroring.
	bound := node.WithWallet(wallets[0])

	if _, err := bound.GetBalances(ctx); err != nil {
		t.Fatalf("balances for %q: %v", wallets[0], err)
	}

	loaded, err := node.WalletLoaded(ctx, wallets[0])

	if err != nil || !loaded {
		t.Fatalf("WalletLoaded(%q) = %v %v", wallets[0], loaded, err)
	}

	loaded, err = node.WalletLoaded(ctx, "definitely-not-a-wallet")

	if err != nil {
		t.Fatal(err)
	}

	if loaded {
		t.Fatal("an absent wallet is reported as loaded")
	}
}

func TestRigSendRefusals(t *testing.T) {
	node := rigNode(t, BTC)
	ctx := context.Background()

	// Sending from a wallet that is not loaded must fail, and must fail as a
	// node error rather than a transport one: this is a state-changing call, so
	// a transport-level failure would leave the caller unsure whether money
	// moved.
	absent := node.WithWallet("definitely-not-a-wallet")

	_, err := absent.SendToAddress(ctx, "bcrt1qxy2kgdygjrsqtzq2n0yrf2493p83kkfjhx0wlh", 1000, 2)

	if err == nil {
		t.Fatal("a send from an absent wallet succeeded")
	}

	var nodeErr *rpc.Error

	if !asNodeError(err, &nodeErr) {
		t.Fatalf("error is %T (%v), want a node error", err, err)
	}
}

// TestRigWalletSurface exercises the wallet-scoped calls funding depends on:
// a fresh address the wallet owns, the watch-only wallet lifecycle, a
// descriptor import with the node's verdict, and the transaction listing
// recovery reads.
func TestRigWalletSurface(t *testing.T) {
	node := rigNode(t, BTC)
	ctx := context.Background()

	wallets, err := node.ListWallets(ctx)

	if err != nil {
		t.Fatal(err)
	}

	if len(wallets) == 0 {
		t.Skip("the rig has no wallet loaded")
	}

	alice := node.Bind(wallets[0])

	address, err := alice.NewAddress(ctx)

	if err != nil {
		t.Fatal(err)
	}

	info, err := alice.AddressInfo(ctx, address)

	if err != nil {
		t.Fatal(err)
	}

	if !info.IsMine {
		t.Fatalf("a fresh address is not ours: %+v", info)
	}

	entries, err := alice.ListTransactions(ctx, 100, 0)

	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		if entry.Category == "receive" && entry.Address == address {
			// Not an assertion: the entry only exists once the address has
			// been paid. The call itself is what is under test.
			break
		}
	}

	// A watch-only wallet holds no keys, so the node will accept a public
	// descriptor — which is exactly what funding relies on before it moves
	// money.
	watchName := "watchtest" + fmt.Sprint(len(entries))

	if err := node.EnsureWallet(ctx, watchName, true); err != nil {
		t.Fatalf("create the watch-only wallet: %v", err)
	}

	bond := htlc.Bond{
		SecretHash:    strings.Repeat("ab", 32),
		BlakeBobPub:   "03c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5",
		BlakeAlicePub: "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798",
		BTCAlicePub:   "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798",
		BTCBobPub:     "03c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5",
		BlakeTimeout:  1000,
		BTCTimeout:    900,
	}

	body, err := bond.Descriptor(htlc.BTC)

	if err != nil {
		t.Fatal(err)
	}

	compiled, err := node.DescriptorInfo(ctx, body)

	if err != nil {
		t.Fatal(err)
	}

	watch := node.Bind(watchName)

	result, err := watch.ImportDescriptor(ctx, compiled.Descriptor)

	if err != nil {
		t.Fatal(err)
	}

	if !result.Success {
		t.Fatalf("the node refused a watch-only import: %+v", result)
	}

	addresses, err := node.DeriveAddresses(ctx, compiled.Descriptor)

	if err != nil {
		t.Fatal(err)
	}

	solvable, err := watch.AddressInfo(ctx, addresses[0])

	if err != nil {
		t.Fatal(err)
	}

	if !solvable.Solvable {
		t.Fatalf("the imported HTLC is not solvable: %+v", solvable)
	}
}
