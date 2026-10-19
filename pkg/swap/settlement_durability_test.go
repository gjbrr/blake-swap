//go:build !js

package swap_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/internal/rigtest"
	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/htlc"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// TestSettlementCandidateIsDurableBeforeBroadcast is the regression test for the
// ordering fix in settleAndRecord, and the smallest honest version of what the
// crash matrix asserts.
//
// It arms the boundary that sits between "the signed transaction is durable" and
// "the node has been told about it", kills the process there, and then asks the
// only question that matters: can the participant come back and recognise the
// transaction it made?
//
// With the old ordering the answer was no — the record was written after the
// broadcast, so a death in this window left a record with no txid and no hex, and
// the retry built a second, different claim.
//
// Parity: tests/swap_settlement_crash_matrix_tests.py
func TestSettlementCandidateIsDurableBeforeBroadcast(t *testing.T) {
	r := rigtest.Load(t)

	alice, bob, messages, id := r.SetupGoSwap(t)

	r.FundBothLegs(t, alice, bob, messages, id)

	point := swap.FailpointName(chain.BTC, htlc.Claim, swap.BoundaryCandidateSave)

	output, exit, err := r.Exec(t, alice, messages,
		[]string{swap.FailpointEnv + "=" + point}, "claim-btc", id)

	if err != nil {
		t.Fatalf("running the armed claim: %v", err)
	}

	if exit != swap.FailpointExit {
		t.Fatalf("exit = %d, want %d at %s\n%s", exit, swap.FailpointExit, point, output)
	}

	if !strings.Contains(output, "TEST FAILPOINT TRIGGERED: "+point) {
		t.Fatalf("the failpoint did not report itself:\n%s", output)
	}

	// The transaction is recorded, with the identity and the signed bytes a
	// restart needs.
	record := rigtest.Record(t, alice, id)

	txid, _ := record["btc_claim_txid"].(string)
	if len(txid) != 64 {
		t.Fatalf("btc_claim_txid = %q, want a 64-character txid", txid)
	}

	hexTx, _ := record["btc_claim_hex"].(string)
	if hexTx == "" {
		t.Fatal("btc_claim_hex is empty; a restart could not rebroadcast")
	}

	// But it was never broadcast: this is the window, not a completed settlement.
	if _, err := r.NodeTry(t, "btc", "getmempoolentry", txid); err == nil {
		t.Fatal("the claim reached the mempool before the broadcast boundary")
	}

	fundingTxID, _ := record["btc_funding_txid"].(string)

	vout, ok := record["btc_funding_vout"].(float64)
	if !ok {
		t.Fatalf("btc_funding_vout is not a number: %v", record["btc_funding_vout"])
	}

	utxo := r.Node(t, "btc", "gettxout", fundingTxID, strconv.FormatInt(int64(vout), 10))

	if strings.TrimSpace(utxo) == "" {
		t.Fatal("the HTLC output was spent before the broadcast boundary")
	}

	// The retry must reuse the exact transaction the durable record names. That is
	// the property the save-before-broadcast ordering exists to provide: one
	// settlement, one transaction, however many times the command is retried.
	r.Run(t, alice, messages, "claim-btc", id)

	retried := rigtest.Record(t, alice, id)

	if retried["btc_claim_txid"] != txid {
		t.Fatalf("the retry built a different transaction: %v, want %s",
			retried["btc_claim_txid"], txid)
	}

	// A rebroadcast is pending, not confirmed, so the protocol state has not moved.
	if retried["state"] != "BTC_FUNDED" {
		t.Fatalf("state = %v after a rebroadcast, want BTC_FUNDED", retried["state"])
	}

	if _, err := r.NodeTry(t, "btc", "getmempoolentry", txid); err != nil {
		t.Fatalf("the retry did not rebroadcast the recorded transaction: %v", err)
	}

	// Once it confirms, the same transaction advances the state; nothing new is
	// signed.
	r.Mine(t, "btc", alice.Role, 1)
	r.Run(t, alice, messages, "claim-btc", id)

	confirmed := rigtest.Record(t, alice, id)

	if confirmed["state"] != "BTC_CLAIMED" {
		t.Fatalf("state = %v, want BTC_CLAIMED", confirmed["state"])
	}

	if confirmed["btc_claim_txid"] != txid {
		t.Fatalf("the confirmed claim is %v, want %s", confirmed["btc_claim_txid"], txid)
	}
}
