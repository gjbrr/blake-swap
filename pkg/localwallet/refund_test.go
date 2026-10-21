package localwallet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"

	swapchain "github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/contract"
	"github.com/gjbrr/blake-swap/pkg/decimal"
	"github.com/gjbrr/blake-swap/pkg/fund"
	"github.com/gjbrr/blake-swap/pkg/htlc"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/settle"
)

// TestRefundThroughTheEngine is the claim test's other half: the same funded contract, spent on
// the timelock branch by the wallet whose key is the refunder, with no preimage and no node
// wallet RPC.
//
// A refund is the branch a participant falls back to when the secret never arrives, so it must
// work without the peer — which is also why it is the branch that proves the wallet can build,
// sign, finalise and broadcast a contract spend on its own.
func TestRefundThroughTheEngine(t *testing.T) {
	if os.Getenv("NOSTRINO_RIG") != "1" {
		t.Skip("NOSTRINO_RIG is not set")
	}

	rig := newRig(t)
	ctx := context.Background()
	direct := NewDirect(rig.dom)

	// The refund branch has no hashlock, but the descriptor still renders one and the
	// contract it compiles carries it, so the hash is a real 32-byte value.
	preimageSum := sha256.Sum256([]byte("the engine's refund preimage"))
	preimage := preimageSum[:]
	sum := sha256.Sum256(preimage)

	rate, _ := decimal.Parse("2")

	otherKey, _ := btcec.PrivKeyFromBytes([]byte{31: 0x66})
	otherPub := hex.EncodeToString(otherKey.PubKey().SerializeCompressed())

	for _, name := range []string{"btc", "blake"} {
		ensureCoins(t, rig, name)

		addresses, _ := rig.dom.Addresses(name, false, 0, 1)
		pub := hex.EncodeToString(addresses[0].Pubkey)
		status, _ := rig.dom.Status(name)

		// The contract must already be refundable when it is spent: it is funded in the
		// next block, so its locktime is that height and the spend needs two blocks.
		timeout := int64(status.Position.Tip) + 1
		ptr := func(v string) *string { return &v }

		// The wallet's key is the refunder on both chains: Alice refunds BLAKE, Bob refunds
		// Bitcoin. The counterparty's key is the claimer, which is what makes this a refund.
		swap := &model.Swap{
			SwapID: "diagrefund", BlakeAmountBaseUnits: 20_000, BTCSats: 25_000,
			SecretHex: ptr(hex.EncodeToString(preimage)), SecretHash: hex.EncodeToString(sum[:]),
			BlakeTimeout: timeout, BTCTimeout: timeout,
			BlakeAlicePub: ptr(pub), BlakeBobPub: ptr(otherPub),
			BTCAlicePub: ptr(otherPub), BTCBobPub: ptr(pub),
		}

		ch := swapchain.Chain(name)
		adapter := New(rpcNode(t, name), direct)

		terms := contract.Terms{
			Claimer: otherKey.PubKey().SerializeCompressed(), Refunder: addresses[0].Pubkey,
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
		rpc(t, name, "generatetoaddress", "2", rpc(t, name, "getnewaddress"))
		syncChain(t, rig, name)

		coin := contractCoin(t, rig.dom, name, p2wsh(derived.Script))

		bond, _ := contract.Bond(swap)
		descriptor, err := bond.Descriptor(htlc.Chain(name))

		if err != nil {
			t.Fatal(err)
		}

		destination, _ := rig.dom.Addresses(name, false, 1, 1)

		plan, err := settle.Refund(ctx, adapter, settle.Request{
			Chain: ch, Funding: swapchain.Outpoint{TxID: displayTxID(coin.TxID), Vout: int64(coin.Vout)},
			Destination: destination[0].Text, Descriptor: descriptor, InputSats: int64(coin.Value),
			LockTime: timeout, Wallet: name,
		}, settle.Options{FeeRate: &rate, MaxPercent: decimal.FromInt64(50)})
		if err != nil {
			t.Fatal(err)
		}

		t.Logf("fee %d sats, plan %s", plan.FeeSats, plan.Hex)

		refundTxID := broadcast(t, name, plan.Hex)

		rpc(t, name, "generatetoaddress", "1", rpc(t, name, "getnewaddress"))
		syncChain(t, rig, name)

		paid := contractCoin(t, rig.dom, name, destination[0].Script())

		if paid.Value != coin.Value-uint64(plan.FeeSats) {
			t.Fatalf("%s: payout %d, fee %d", name, paid.Value, plan.FeeSats)
		}

		if got := rpc(t, name, "gettxout", displayTxID(coin.TxID), fmt.Sprintf("%d", coin.Vout)); strings.TrimSpace(got) != "" {
			t.Fatalf("%s: the contract is still unspent", name)
		}

		say(t, "%s: settle.Refund paid %d to %s in %s (fee %d), payout coin at height %d",
			name, paid.Value, destination[0].Text, refundTxID, plan.FeeSats, paid.Height)
	}
}
