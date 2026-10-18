package swap

import (
	"context"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/fund"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/state"
	"github.com/gjbrr/blake-swap/pkg/store"
)

// aliceOfferSwap stores an offer that Alice has created but Bob has not accepted.
func aliceOfferSwap(t *testing.T, st store.Store, id string) *model.Swap {
	t.Helper()

	record := newRecord(t, id)

	alice := string(aliceIdentity)
	record.NostrAlicePub = &alice
	record.BlakeAlicePub = ptr("0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798")
	record.BTCAlicePub = record.BlakeAlicePub

	if err := st.Save(context.Background(), store.Alice, record); err != nil {
		t.Fatal(err)
	}

	return record
}

// refreshEngine builds an engine whose chains are at the given heights.
func refreshEngine(t *testing.T, blakeHeight, btcHeight int64) (*Engine, *mempoolNode, *mempoolNode, store.Store) {
	t.Helper()

	st := store.NewMemory()

	blake := newMempoolNode()
	blake.height = blakeHeight

	btc := newMempoolNode()
	btc.height = btcHeight

	engine := NewEngine(st, fund.NewMemStore(), blake, btc)
	engine.Identity = aliceIdentity

	return engine, blake, btc, st
}

// TestRefreshTimeoutsRecomputesThePlan: the deadlines move with the chain, and nothing
// else does.
func TestRefreshTimeoutsRecomputesThePlan(t *testing.T) {
	ctx := context.Background()

	engine, _, _, st := refreshEngine(t, 1_000, 500)

	record := aliceOfferSwap(t, st, "aaaa00000701")

	// An offer created earlier has deadlines computed from older tips.
	record.BTCTimeout = 400
	record.BlakeTimeout = 500
	record.BTCScanStartHeight = ptr(int64(100))
	record.BlakeScanStartHeight = ptr(int64(200))

	if err := st.Save(ctx, store.Alice, record); err != nil {
		t.Fatal(err)
	}

	result, err := engine.RefreshTimeouts(ctx, record.SwapID, false)

	if err != nil {
		t.Fatal(err)
	}

	if result.NewBTCTimeout <= 500 || result.NewBlakeTimeout <= 1_000 {
		t.Fatalf("the plan was not recomputed: %+v", result)
	}

	persisted, err := st.Load(ctx, record.SwapID)

	if err != nil {
		t.Fatal(err)
	}

	if persisted.BTCTimeout != result.NewBTCTimeout || persisted.BlakeTimeout != result.NewBlakeTimeout {
		t.Fatalf("thresholds = %d/%d", persisted.BTCTimeout, persisted.BlakeTimeout)
	}

	// The anchors move with the plan.
	if persisted.BTCScanStartHeight == nil || *persisted.BTCScanStartHeight != 500 {
		t.Fatalf("btc anchor = %v", persisted.BTCScanStartHeight)
	}

	// Identity, secret and economics are untouched.
	if persisted.SwapID != record.SwapID ||
		persisted.SecretHash != record.SecretHash ||
		persisted.BlakeAmountBaseUnits != record.BlakeAmountBaseUnits ||
		persisted.BTCSats != record.BTCSats {
		t.Fatal("a timeout refresh changed something it must not")
	}

	if persisted.SecretHex == nil || *persisted.SecretHex != *record.SecretHex {
		t.Fatal("the secret changed")
	}
}

// TestRefreshTimeoutsRefusesAfterAcceptance: the advertised deadlines are part of the
// contract once the counterparty has accepted.
func TestRefreshTimeoutsRefusesAfterAcceptance(t *testing.T) {
	ctx := context.Background()

	engine, _, _, st := refreshEngine(t, 100, 100)

	record := aliceOfferSwap(t, st, "aaaa00000702")

	// Acceptance binds Bob's contract key.
	record.BlakeBobPub = ptr("03c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5")

	if err := st.Save(ctx, store.Alice, record); err != nil {
		t.Fatal(err)
	}

	if _, err := engine.RefreshTimeouts(ctx, record.SwapID, false); err == nil ||
		!strings.Contains(err.Error(), "after counterparty acceptance") {
		t.Fatalf("err = %v", err)
	}
}

// TestRefreshTimeoutsRefusesFundingEvidence covers both the canonical evidence and the
// intent that can exist before any of it is written.
func TestRefreshTimeoutsRefusesFundingEvidence(t *testing.T) {
	ctx := context.Background()

	t.Run("recorded funding", func(t *testing.T) {
		engine, _, _, st := refreshEngine(t, 100, 100)

		record := aliceOfferSwap(t, st, "aaaa00000703")
		record.BlakeFundingTxID = ptr(strings.Repeat("aa", 32))

		if err := st.Save(ctx, store.Alice, record); err != nil {
			t.Fatal(err)
		}

		if _, err := engine.RefreshTimeouts(ctx, record.SwapID, false); err == nil ||
			!strings.Contains(err.Error(), "funding evidence") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("leftover intent", func(t *testing.T) {
		engine, _, _, st := refreshEngine(t, 100, 100)

		record := aliceOfferSwap(t, st, "aaaa00000704")

		if _, err := fund.WriteIntent(ctx, engine.Intents, record, chain.Blake,
			"bcrt1qsomething"); err != nil {
			t.Fatal(err)
		}

		if _, err := engine.RefreshTimeouts(ctx, record.SwapID, false); err == nil ||
			!strings.Contains(err.Error(), "funding intent exists") {
			t.Fatalf("err = %v", err)
		}

		// Without the explicit flag the intent survives, so a later attempt still
		// refuses.
		if _, found, _ := fund.ReadIntent(ctx, engine.Intents, record.SwapID, chain.Blake); !found {
			t.Fatal("a refused refresh cleared the intent")
		}
	})

	t.Run("pending intent is never cleared", func(t *testing.T) {
		engine, blake, _, st := refreshEngine(t, 100, 100)

		record := aliceOfferSwap(t, st, "aaaa00000705")

		//
		// A pre-acceptance swap has no contract address yet, so the intent is written
		// against an address directly. The case exists for a damaged or legacy record: it
		// is still a funding attempt this command must refuse to clear.
		//
		intent, err := fund.WriteIntent(ctx, engine.Intents, record, chain.Blake, "bcrt1qpending")

		if err != nil {
			t.Fatal(err)
		}

		//
		// The wallet knows an unconfirmed payment to that address, which is exactly what
		// a funding broadcast in flight looks like.
		//
		txid := strings.Repeat("d1", 32)

		tx := &chain.Tx{
			TxID: txid,
			Vout: []chain.Vout{{
				N:     0,
				Value: amount.Coin(amount.Sats(record.BlakeAmountBaseUnits).Coin().String()),
			}},
		}
		tx.Vout[0].ScriptPubKey.Address = intent.Address

		blake.txs[txid] = tx
		blake.walletEntries = []chain.WalletEntry{{
			Address:  intent.Address,
			Category: "send",
			TxID:     txid,
			Amount:   amount.Coin("-" + amount.Sats(record.BlakeAmountBaseUnits).Coin().String()),
		}}
		blake.walletTxs[txid] = &chain.WalletTx{TxID: txid, Hex: txid}

		if _, err := engine.RefreshTimeouts(ctx, record.SwapID, true); err == nil ||
			!strings.Contains(err.Error(), "still pending") {
			t.Fatalf("err = %v", err)
		}

		if _, found, _ := fund.ReadIntent(ctx, engine.Intents, record.SwapID, chain.Blake); !found {
			t.Fatal("a pending intent was cleared")
		}
	})

	t.Run("stale intent is cleared on request", func(t *testing.T) {
		engine, _, _, st := refreshEngine(t, 100, 100)

		record := aliceOfferSwap(t, st, "aaaa00000706")

		if _, err := fund.WriteIntent(ctx, engine.Intents, record, chain.Blake,
			"bcrt1qnothing"); err != nil {
			t.Fatal(err)
		}

		result, err := engine.RefreshTimeouts(ctx, record.SwapID, true)

		if err != nil {
			t.Fatalf("an explicitly cleared stale intent did not allow the refresh: %v", err)
		}

		if len(result.ClearedIntents) != 1 || result.ClearedIntents[0] != "BLAKE" {
			t.Fatalf("cleared = %v", result.ClearedIntents)
		}

		if _, found, _ := fund.ReadIntent(ctx, engine.Intents, record.SwapID, chain.Blake); found {
			t.Fatal("the stale intent was not cleared")
		}
	})
}

func TestRefreshTimeoutsRequiresTheOfferState(t *testing.T) {
	ctx := context.Background()

	engine, _, _, st := refreshEngine(t, 100, 100)

	record := aliceOfferSwap(t, st, "aaaa00000707")
	record.State = state.BlakeFunded

	if err := st.Save(ctx, store.Alice, record); err != nil {
		t.Fatal(err)
	}

	if _, err := engine.RefreshTimeouts(ctx, record.SwapID, false); err == nil ||
		!strings.Contains(err.Error(), "requires state OFFERED") {
		t.Fatalf("err = %v", err)
	}
}
