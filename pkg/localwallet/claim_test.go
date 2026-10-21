package localwallet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"

	"fmt"
	swapchain "github.com/gjbrr/blake-swap/pkg/chain"
	"strings"

	"github.com/gjbrr/blake-swap/pkg/contract"
	"github.com/gjbrr/blake-swap/pkg/decimal"
	"github.com/gjbrr/blake-swap/pkg/fund"
	"github.com/gjbrr/blake-swap/pkg/htlc"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/settle"
)

// TestSettleAClaimThroughTheEngine runs settle.Claim itself against the local wallet: fund a
// contract through fund.Fund, then claim it with the wallet's own key and the preimage.
//
// The contract's claim branch checks OP_SIZE 32, so the preimage is 32 bytes. A shorter one is
// a valid signature over an unspendable witness — which is exactly how this test found the
// difference between "the wallet signs" and "the chain accepts".
func TestSettleAClaimThroughTheEngine(t *testing.T) {
	if os.Getenv("NOSTRINO_RIG") != "1" {
		t.Skip("NOSTRINO_RIG is not set")
	}

	rig := newRig(t)
	ctx := context.Background()
	direct := NewDirect(rig.dom)

	// The contract checks OP_SIZE 32: the preimage must be exactly 32 bytes.
	preimageSum := sha256.Sum256([]byte("the engine's claim preimage"))
	preimage := preimageSum[:]
	sum := sha256.Sum256(preimage)
	rate, _ := decimal.Parse("2")

	otherKey, _ := btcec.PrivKeyFromBytes([]byte{31: 0x55})
	otherPub := hex.EncodeToString(otherKey.PubKey().SerializeCompressed())

	for _, name := range []string{"btc", "blake"} {
		ensureCoins(t, rig, name)

		addresses, _ := rig.dom.Addresses(name, false, 0, 1)
		pub := hex.EncodeToString(addresses[0].Pubkey)
		status, _ := rig.dom.Status(name)
		timeout := int64(status.Position.Tip) + 200
		ptr := func(v string) *string { return &v }

		swap := &model.Swap{
			SwapID: "diagclaim", BlakeAmountBaseUnits: 20_000, BTCSats: 25_000,
			SecretHex: ptr(hex.EncodeToString(preimage)), SecretHash: hex.EncodeToString(sum[:]),
			BlakeTimeout: timeout, BTCTimeout: timeout,
			BlakeAlicePub: ptr(otherPub), BlakeBobPub: ptr(pub),
			BTCAlicePub: ptr(pub), BTCBobPub: ptr(otherPub),
		}

		ch := swapchain.Chain(name)
		adapter := New(rpcNode(t, name), direct)

		terms := contract.Terms{
			Claimer: addresses[0].Pubkey, Refunder: otherKey.PubKey().SerializeCompressed(),
			SecretHash: sum, Locktime: uint32(timeout),
		}

		derived, err := adapter.DeriveHTLC(ctx, ch, terms)
		if err != nil {
			t.Fatal(err)
		}

		if err := direct.Watch(ctx, name, p2wsh(derived.Script)); err != nil {
			t.Fatal(err)
		}

		receipt, err := fund.Fund(ctx, adapter, swap, ch, fund.Options{
			Wallet: name, SkipWatchOnly: true, FeeRate: &rate, Intents: fund.NewMemStore(),
		})
		if err != nil {
			t.Fatal(err)
		}

		waitForMempool(t, name, receipt.TxID)
		rpc(t, name, "generatetoaddress", "1", rpc(t, name, "getnewaddress"))
		syncChain(t, rig, name)

		coin := contractCoin(t, rig.dom, name, p2wsh(derived.Script))

		bond, _ := contract.Bond(swap)
		descriptor, _ := bond.Descriptor(htlc.Chain(name))
		destination, _ := rig.dom.Addresses(name, false, 1, 1)

		plan, err := settle.Claim(ctx, adapter, settle.Request{
			Chain: ch, Funding: swapchain.Outpoint{TxID: displayTxID(coin.TxID), Vout: int64(coin.Vout)},
			Destination: destination[0].Text, Descriptor: descriptor, InputSats: int64(coin.Value),
			SecretHash: hex.EncodeToString(sum[:]), Preimage: hex.EncodeToString(preimage), Wallet: name,
		}, settle.Options{FeeRate: &rate, MaxPercent: decimal.FromInt64(50)})
		if err != nil {
			t.Fatal(err)
		}

		t.Logf("fee %d sats, plan %s", plan.FeeSats, plan.Hex)

		n := nodes[name]
		out, err := exec.Command(n.cli, "-datadir="+n.datadir, "sendrawtransaction", plan.Hex).CombinedOutput()
		if err != nil {
			t.Fatalf("sendrawtransaction: %v: %s", err, out)
		}

		claimTxID := strings.TrimSpace(string(out))

		rpc(t, name, "generatetoaddress", "1", rpc(t, name, "getnewaddress"))
		syncChain(t, rig, name)

		paid := contractCoin(t, rig.dom, name, destination[0].Script())

		if paid.Value != coin.Value-uint64(plan.FeeSats) {
			t.Fatalf("%s: payout %d, fee %d", name, paid.Value, plan.FeeSats)
		}

		if got := rpc(t, name, "gettxout", displayTxID(coin.TxID), fmt.Sprintf("%d", coin.Vout)); strings.TrimSpace(got) != "" {
			t.Fatalf("%s: the contract is still unspent", name)
		}

		say(t, "%s: settle.Claim paid %d to %s in %s (fee %d), payout coin at height %d",
			name, paid.Value, destination[0].Text, claimTxID, plan.FeeSats, paid.Height)
	}
}
