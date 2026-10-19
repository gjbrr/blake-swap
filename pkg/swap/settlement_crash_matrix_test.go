//go:build !js

package swap_test

import (
	"testing"

	"github.com/gjbrr/blake-swap/internal/rigtest"
	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// Parity: tests/swap_settlement_crash_matrix_tests.py
//
// Four economic operations, each killed at three durability boundaries, then
// restarted. The claim the matrix makes is narrow and worth stating exactly: after
// every deliberate death the participant converges on the same economic
// transaction, and never on a second HTLC spend.
func TestSettlementCrashMatrix(t *testing.T) {
	r := rigtest.Load(t)

	for _, operation := range operations() {
		for boundary, point := range boundaries(operation) {
			t.Run(operation.name+"/"+boundary, func(t *testing.T) {
				scene := prepare(t, r, operation)

				scene.crash(t, operation, point)

				// The crash moved no durable state, and it left the signed
				// candidate behind: that is the whole point of saving before
				// broadcasting.
				scene.assertSourceStateUnchanged(t, operation, boundary)

				txid, blockHash, hexTx := scene.durable(t, operation)

				if txid == "" {
					t.Fatalf("%s/%s: the crash lost the durable settlement txid",
						operation.name, boundary)
				}

				if hexTx == "" {
					t.Fatalf("%s/%s: the crash lost the durable signed transaction",
						operation.name, boundary)
				}

				if blockHash != "" {
					t.Fatalf("%s/%s: a pre-confirmation record carries a block hash",
						operation.name, boundary)
				}

				// Where the transaction got to, per boundary.
				switch boundary {
				case swap.BoundaryCandidateSave:
					if scene.inMempool(t, operation, txid) {
						t.Fatalf("%s: the transaction was broadcast before the "+
							"broadcast boundary", operation.name)
					}

				case swap.BoundaryBroadcast:
					if !scene.inMempool(t, operation, txid) {
						t.Fatalf("%s: the transaction is not in the mempool after "+
							"the broadcast boundary", operation.name)
					}

				case swap.BoundaryConfirmation:
					if scene.inMempool(t, operation, txid) {
						t.Fatalf("%s: a confirmed settlement is still in the mempool",
							operation.name)
					}
				}

				// Restart. The transaction must be adopted, not re-signed.
				output, exit, err := r.Exec(t, scene.actor, scene.messages, nil,
					operation.command, scene.id)

				if err != nil {
					t.Fatalf("%s/%s: restarting: %v", operation.name, boundary, err)
				}

				if exit != 0 {
					t.Fatalf("%s/%s: the restart failed with exit %d:\n%s",
						operation.name, boundary, exit, output)
				}

				restarted, _, _ := scene.durable(t, operation)

				if restarted != txid {
					t.Fatalf("%s/%s: the restart changed the canonical txid %s to %s",
						operation.name, boundary, txid, restarted)
				}

				if spenders := scene.spenders(t, operation); len(spenders) > 1 {
					t.Fatalf("%s/%s: %d transactions are spending the HTLC: %v",
						operation.name, boundary, len(spenders), spenders)
				}

				// The confirming block is produced by the test, and the same
				// transaction completes the swap.
				scene.mineAndFinish(t, operation, txid)
			})
		}
	}
}

// TestSettlementFaultMatrix is the same twelve cases with the stricter claim the
// reference's fault matrix makes: at every observation, the HTLC has at most one
// spender, and after the swap completes exactly one transaction ever spent it.
//
// The two suites overlap by design. The crash matrix asks "did the participant
// recover its own transaction"; the fault matrix asks "did it ever create a
// second one", which is the failure that costs real money.
func TestSettlementFaultMatrix(t *testing.T) {
	r := rigtest.Load(t)

	for _, operation := range operations() {
		for boundary, point := range boundaries(operation) {
			t.Run(operation.name+"/"+boundary, func(t *testing.T) {
				scene := prepare(t, r, operation)

				scene.crash(t, operation, point)

				txid, _, _ := scene.durable(t, operation)

				if txid == "" {
					t.Fatalf("%s/%s: no durable settlement txid after the fault",
						operation.name, boundary)
				}

				// Never two spenders, at any point, on any boundary.
				if spenders := scene.spenders(t, operation); len(spenders) > 1 {
					t.Fatalf("%s/%s: the fault produced %d competing HTLC spends: %v",
						operation.name, boundary, len(spenders), spenders)
				}

				// A second invocation without a prior restart must not add one
				// either: the durable record is what stops it.
				output, exit, err := r.Exec(t, scene.actor, scene.messages, nil,
					operation.command, scene.id)

				if err != nil {
					t.Fatalf("%s/%s: running again: %v", operation.name, boundary, err)
				}

				if exit != 0 {
					t.Fatalf("%s/%s: running again failed with exit %d:\n%s",
						operation.name, boundary, exit, output)
				}

				if restarted, _, _ := scene.durable(t, operation); restarted != txid {
					t.Fatalf("%s/%s: a second run signed a different transaction: "+
						"%s -> %s", operation.name, boundary, txid, restarted)
				}

				if spenders := scene.spenders(t, operation); len(spenders) > 1 {
					t.Fatalf("%s/%s: a second run produced %d competing HTLC spends: %v",
						operation.name, boundary, len(spenders), spenders)
				}

				scene.mineAndFinish(t, operation, txid)

				// Exactly one transaction ever spent the HTLC, and it is the one
				// the record names.
				if spenders := scene.confirmedSpenders(t, operation); len(spenders) != 1 {
					t.Fatalf("%s/%s: %d transactions spent the HTLC in its block: %v",
						operation.name, boundary, len(spenders), spenders)
				} else if spenders[0] != txid {
					t.Fatalf("%s/%s: the HTLC was spent by %s, not %s",
						operation.name, boundary, spenders[0], txid)
				}
			})
		}
	}
}

// confirmedSpenders lists the transactions in the settlement's block that spend
// the HTLC, which is the durable answer to "how many economic actions happened".
func (s *settlementScene) confirmedSpenders(t *testing.T, o settlementOperation) []string {
	t.Helper()

	txid, _, _ := s.durable(t, o)
	blockHash := s.confirmedBlock(t, o, txid)

	chainName := "btc"

	if o.chain == chain.Blake {
		chainName = "blake"
	}

	raw := s.r.Node(t, chainName, "getblock", blockHash, "2")

	var block struct {
		Tx []struct {
			TxID string `json:"txid"`
			Vin  []struct {
				TxID string `json:"txid"`
				Vout int64  `json:"vout"`
			} `json:"vin"`
		} `json:"tx"`
	}

	if err := decodeJSON(raw, &block); err != nil {
		t.Fatalf("%s: decoding the block: %v", o.name, err)
	}

	fundingTxID, fundingVout := s.funding(t, o)

	var found []string

	for _, tx := range block.Tx {
		for _, input := range tx.Vin {
			if input.TxID == fundingTxID && input.Vout == fundingVout {
				found = append(found, tx.TxID)

				break
			}
		}
	}

	return found
}
