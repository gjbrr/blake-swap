package swap

import (
	"context"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/coord"
	"github.com/gjbrr/blake-swap/pkg/fund"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/state"
	"github.com/gjbrr/blake-swap/pkg/store"
)

// claimedSwap is a stored swap whose Bitcoin claim is confirmed and recorded.
func claimedSwap(t *testing.T, st store.Store, id string) (*model.Swap, *claimFixture) {
	t.Helper()

	record := settlementSwap(t, st, id, state.BTCClaimed)

	claimID := strings.Repeat("c3", 32)
	blockHash := strings.Repeat("c4", 32)

	record.BTCCLaimTxID = &claimID
	record.BTCCLaimBlockHash = &blockHash

	if err := st.Save(context.Background(), store.Alice, record); err != nil {
		t.Fatal(err)
	}

	return record, &claimFixture{txid: claimID, blockHash: blockHash}
}

type claimFixture struct {
	txid      string
	blockHash string
}

// secretClaimNode is a node holding a confirmed Bitcoin claim whose witness
// carries the preimage.
func secretClaimNode(record *model.Swap, fixture *claimFixture) *mempoolNode {
	node := newMempoolNode()

	tx := claimTx(fixture.txid, *record.BTCFundingTxID, 0, *record.SecretHex, 2)
	tx.BlockHash = fixture.blockHash

	node.txs[fixture.txid] = tx

	return node
}

// TestExtractSecretReadsThePreimageFromTheChain is the only way Bob learns the
// secret: it is published on Bitcoin, not in a message.
// Parity: tests/production_settlement_pending_tests.py
func TestExtractSecretReadsThePreimageFromTheChain(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	record, fixture := claimedSwap(t, st, "aaaa00000401")
	node := secretClaimNode(record, fixture)

	engine := NewEngine(st, fund.NewMemStore(), node, node)
	engine.Identity = bobIdentity

	secret, err := engine.ExtractSecret(ctx, record.SwapID)

	if err != nil {
		t.Fatal(err)
	}

	if secret != *record.SecretHex {
		t.Fatalf("secret = %s", secret)
	}

	persisted, _ := st.Load(ctx, record.SwapID)

	if persisted.RevealedSecretHex == nil || *persisted.RevealedSecretHex != secret {
		t.Fatal("the revealed secret was not recorded")
	}
}

// TestExtractSecretRefusesAWitnessThatDoesNotHash keeps the extraction honest: a
// 32-byte witness item is only the secret if it hashes to the swap's hash.
func TestExtractSecretRefusesAWitnessThatDoesNotHash(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	record, fixture := claimedSwap(t, st, "aaaa00000402")

	node := newMempoolNode()
	tx := claimTx(fixture.txid, *record.BTCFundingTxID, 0, strings.Repeat("99", 32), 2)
	tx.BlockHash = fixture.blockHash
	node.txs[fixture.txid] = tx

	engine := NewEngine(st, fund.NewMemStore(), node, node)
	engine.Identity = bobIdentity

	_, err := engine.ExtractSecret(ctx, record.SwapID)

	if err == nil || !strings.Contains(err.Error(), "No witness item matched") {
		t.Fatalf("err = %v", err)
	}
}

// TestReceiveBTCCLaimedVerifiesAndAdvances: the announcement is verified against
// Bitcoin before Bob's record moves.
func TestReceiveBTCCLaimedVerifiesAndAdvances(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	node := newMempoolNode()
	transport := newRecordingTransport()

	engine := NewEngine(st, fund.NewMemStore(), node, node)
	engine.Identity = bobIdentity
	engine.Transport = transport

	record := settlementSwap(t, st, "aaaa00000403", state.BTCFunded)

	claimID := strings.Repeat("c5", 32)
	blockHash := strings.Repeat("c6", 32)

	tx := claimTx(claimID, *record.BTCFundingTxID, 0, *record.SecretHex, 2)
	tx.BlockHash = blockHash
	node.txs[claimID] = tx

	transport.incoming["btc-claimed"] = &coord.Message{
		Kind:   "btc-claimed",
		Sender: string(aliceIdentity),
		Body: map[string]any{
			"type":                "btc-claimed",
			"swap_id":             record.SwapID,
			"secret_hash":         record.SecretHash,
			"btc_claim_txid":      claimID,
			"btc_claim_blockhash": blockHash,
		},
		Location: "event-claimed",
	}

	result, err := engine.ReceiveBTCCLaimed(ctx, record.SwapID)

	if err != nil {
		t.Fatal(err)
	}

	if result.State != state.BTCClaimed {
		t.Fatalf("state = %s", result.State)
	}

	persisted, _ := st.Load(ctx, record.SwapID)

	if persisted.BTCCLaimTxID == nil || *persisted.BTCCLaimTxID != claimID {
		t.Fatal("the claim was not recorded")
	}

	// The message did not carry the secret, and receiving it did not create one.
	if persisted.RevealedSecretHex != nil {
		t.Fatal("the secret arrived in a message")
	}
}

// TestReceiveRefusesAnUnconfirmedClaim: an announced settlement that has not
// confirmed is not a settlement.
func TestReceiveRefusesAnUnconfirmedClaim(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	node := newMempoolNode()
	transport := newRecordingTransport()

	engine := NewEngine(st, fund.NewMemStore(), node, node)
	engine.Identity = bobIdentity
	engine.Transport = transport

	record := settlementSwap(t, st, "aaaa00000404", state.BTCFunded)

	claimID := strings.Repeat("c7", 32)
	blockHash := strings.Repeat("c8", 32)

	tx := claimTx(claimID, *record.BTCFundingTxID, 0, *record.SecretHex, 0)
	tx.BlockHash = blockHash
	node.txs[claimID] = tx

	transport.incoming["btc-claimed"] = &coord.Message{
		Kind:   "btc-claimed",
		Sender: string(aliceIdentity),
		Body: map[string]any{
			"type":                "btc-claimed",
			"swap_id":             record.SwapID,
			"secret_hash":         record.SecretHash,
			"btc_claim_txid":      claimID,
			"btc_claim_blockhash": blockHash,
		},
	}

	_, err := engine.ReceiveBTCCLaimed(ctx, record.SwapID)

	if err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Fatalf("err = %v", err)
	}

	persisted, _ := st.Load(ctx, record.SwapID)

	if persisted.State != state.BTCFunded {
		t.Fatalf("a refused claim changed the state to %s", persisted.State)
	}
}

// TestReceiveRefusesAConflictingClaim: two different claims cannot both be the
// settlement of one HTLC.
func TestReceiveRefusesAConflictingClaim(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	node := newMempoolNode()
	transport := newRecordingTransport()

	engine := NewEngine(st, fund.NewMemStore(), node, node)
	engine.Identity = bobIdentity
	engine.Transport = transport

	record := settlementSwap(t, st, "aaaa00000405", state.BTCFunded)

	recordedID := strings.Repeat("c9", 32)
	recordedBlock := strings.Repeat("ca", 32)
	record.BTCCLaimTxID = &recordedID
	record.BTCCLaimBlockHash = &recordedBlock

	if err := st.Save(ctx, store.Alice, record); err != nil {
		t.Fatal(err)
	}

	announcedID := strings.Repeat("cb", 32)
	announcedBlock := strings.Repeat("cc", 32)

	tx := claimTx(announcedID, *record.BTCFundingTxID, 0, *record.SecretHex, 2)
	tx.BlockHash = announcedBlock
	node.txs[announcedID] = tx

	transport.incoming["btc-claimed"] = &coord.Message{
		Kind:   "btc-claimed",
		Sender: string(aliceIdentity),
		Body: map[string]any{
			"type":                "btc-claimed",
			"swap_id":             record.SwapID,
			"secret_hash":         record.SecretHash,
			"btc_claim_txid":      announcedID,
			"btc_claim_blockhash": announcedBlock,
		},
	}

	_, err := engine.ReceiveBTCCLaimed(ctx, record.SwapID)

	if err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("err = %v", err)
	}
}

// TestReceiveBlakeClaimedAndBTCRefunded cover the two other announcements.
func TestReceiveBlakeClaimedAndBTCRefunded(t *testing.T) {
	ctx := context.Background()

	t.Run("blake-claimed", func(t *testing.T) {
		st := store.NewMemory()
		node := newMempoolNode()
		transport := newRecordingTransport()

		engine := NewEngine(st, fund.NewMemStore(), node, node)
		engine.Identity = aliceIdentity
		engine.Transport = transport

		record := settlementSwap(t, st, "aaaa00000406", state.BTCClaimed)

		claimID := strings.Repeat("d1", 32)
		blockHash := strings.Repeat("d2", 32)

		tx := claimTx(claimID, *record.BlakeFundingTxID, 0, *record.SecretHex, 3)
		tx.BlockHash = blockHash
		node.txs[claimID] = tx

		transport.incoming["blake-claimed"] = &coord.Message{
			Kind:   "blake-claimed",
			Sender: string(bobIdentity),
			Body: map[string]any{
				"type":                  "blake-claimed",
				"swap_id":               record.SwapID,
				"secret_hash":           record.SecretHash,
				"blake_claim_txid":      claimID,
				"blake_claim_blockhash": blockHash,
			},
		}

		result, err := engine.ReceiveBlakeClaimed(ctx, record.SwapID)

		if err != nil {
			t.Fatal(err)
		}

		if result.State != state.BlakeClaimed {
			t.Fatalf("state = %s", result.State)
		}
	})

	t.Run("btc-refunded", func(t *testing.T) {
		st := store.NewMemory()
		node := newMempoolNode()
		transport := newRecordingTransport()

		engine := NewEngine(st, fund.NewMemStore(), node, node)
		engine.Identity = aliceIdentity
		engine.Transport = transport

		record := settlementSwap(t, st, "aaaa00000407", state.BTCFunded)

		refundID := strings.Repeat("d3", 32)
		blockHash := strings.Repeat("d4", 32)

		tx := refundTx(refundID, *record.BTCFundingTxID, 0, 3)
		tx.BlockHash = blockHash
		node.txs[refundID] = tx

		transport.incoming["btc-refunded"] = &coord.Message{
			Kind:   "btc-refunded",
			Sender: string(bobIdentity),
			Body: map[string]any{
				"type":                 "btc-refunded",
				"swap_id":              record.SwapID,
				"secret_hash":          record.SecretHash,
				"btc_refund_txid":      refundID,
				"btc_refund_blockhash": blockHash,
			},
		}

		result, err := engine.ReceiveBTCRefunded(ctx, record.SwapID)

		if err != nil {
			t.Fatal(err)
		}

		if result.State != state.BTCRefunded {
			t.Fatalf("state = %s", result.State)
		}
	})
}

// TestAnnounceSettlementCarriesOnlyPublicData: the settlement announcements
// describe transactions that are already public; the secret is never in them.
func TestAnnounceSettlementCarriesOnlyPublicData(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	node := newMempoolNode()
	transport := newRecordingTransport()

	engine := NewEngine(st, fund.NewMemStore(), node, node)
	engine.Identity = aliceIdentity
	engine.Transport = transport

	record := settlementSwap(t, st, "aaaa00000408", state.BTCClaimed)

	claimID := strings.Repeat("d5", 32)
	blockHash := strings.Repeat("d6", 32)
	record.BTCCLaimTxID = &claimID
	record.BTCCLaimBlockHash = &blockHash

	if err := st.Save(ctx, store.Alice, record); err != nil {
		t.Fatal(err)
	}

	if _, err := engine.AnnounceBTCCLaimed(ctx, record.SwapID); err != nil {
		t.Fatal(err)
	}

	if len(transport.sent) != 1 {
		t.Fatalf("sent %d messages", len(transport.sent))
	}

	body := transport.sent[0].body

	if _, present := body["secret_hex"]; present {
		t.Fatal("the claim announcement carries the secret")
	}

	if body["btc_claim_txid"] != claimID || body["btc_claim_blockhash"] != blockHash {
		t.Fatalf("body = %v", body)
	}
}
