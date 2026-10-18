package observe

import (
	"context"
	"fmt"

	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/contract"
	"github.com/gjbrr/blake-swap/pkg/htlc"
	"github.com/gjbrr/blake-swap/pkg/model"
)

// Observe reports what is true about one leg of a swap, without changing
// anything durable.
//
// The order of the checks is the design. Funding is confirmed before a spend
// is even considered; a spend is only reported when it is confirmed and proven
// to use the expected branch; and a spend that is merely in the mempool is
// reported as a wait, never as absence. Every path that cannot decide returns
// a status the engine will not act on.
func Observe(
	ctx context.Context,
	node Node,
	swap *model.Swap,
	ch chain.Chain,
	cfg Config,
) (*Observation, error) {
	if swap == nil {
		return nil, fmt.Errorf("swap is nil")
	}

	anchor := anchorFor(swap, ch)

	//
	// Half an outpoint is not a weaker claim, it is a corrupt one: a txid
	// without a vout could name any output of that transaction, and funding a
	// swap against the wrong output is unrecoverable.
	//
	if (anchor.TxID == nil) != (anchor.Vout == nil) {
		return nil, rejectf(
			"incomplete recorded %s funding outpoint", chainName(ch),
		)
	}

	var (
		txid       string
		vout       int64
		blockHash  string
		discovered bool
	)

	if anchor.TxID != nil {
		txid = *anchor.TxID
		vout = int64(*anchor.Vout)
	}

	if anchor.BlockHash != nil {
		blockHash = *anchor.BlockHash
	}

	if anchor.TxID == nil {
		//
		// Recovery is deliberately allowed in this read-only report: it
		// updates only the caller's copy of the observation.
		//
		funding, err := ScanFunding(ctx, node, swap, ch, cfg)

		if err != nil {
			return &Observation{Status: Unknown, Reason: err.Error()}, nil
		}

		if funding == nil {
			return &Observation{Status: Absent}, nil
		}

		discovered = true
		txid = funding.TxID
		vout = funding.Vout
		blockHash = funding.BlockHash
	}

	utxo, err := node.TxOut(ctx, txid, vout)

	if err != nil {
		return nil, err
	}

	if utxo != nil {
		if utxo.Confirmations < 1 {
			//
			// A zero-confirmation output is not invalid merely because it is
			// waiting for a block, but it also is not proof: prove the amount
			// and address independently before calling it pending.
			//
			derived, err := contract.Derive(ctx, node, swap, ch)

			if err != nil {
				return &Observation{
					Status: Invalid, TxID: txid, Vout: vout, Reason: err.Error(),
				}, nil
			}

			if err := derived.CheckOutput(utxo.Value, utxo.ScriptPubKey.Address); err != nil {
				return &Observation{
					Status: Invalid, TxID: txid, Vout: vout, Reason: err.Error(),
				}, nil
			}

			return &Observation{
				Status:        Pending,
				TxID:          txid,
				Vout:          vout,
				BlockHash:     blockHash,
				Confirmations: utxo.Confirmations,
				Discovered:    discovered,
			}, nil
		}

		derived, err := contract.Derive(ctx, node, swap, ch)

		if err != nil {
			return &Observation{
				Status: Invalid, TxID: txid, Vout: vout, Reason: err.Error(),
			}, nil
		}

		if err := derived.CheckOutput(utxo.Value, utxo.ScriptPubKey.Address); err != nil {
			return &Observation{
				Status: Invalid, TxID: txid, Vout: vout, Reason: err.Error(),
			}, nil
		}

		return &Observation{
			Status:        Funded,
			TxID:          txid,
			Vout:          vout,
			BlockHash:     blockHash,
			Confirmations: utxo.Confirmations,
			Discovered:    discovered,
		}, nil
	}

	//
	// A recorded funding transaction may still be in the mempool, and some
	// node paths fail to expose its output through gettxout even though the
	// exact transaction is available. Only an unconfirmed transaction is
	// classified here; anything confirmed goes through settlement recovery,
	// because an absent output then means the HTLC was spent.
	//
	if blockHash == "" {
		pendingTx, err := node.RawTx(ctx, txid, "")

		if err == nil && pendingTx != nil && pendingTx.Confirmations < 1 {
			var output *chain.Vout

			for i := range pendingTx.Vout {
				if pendingTx.Vout[i].N == vout {
					output = &pendingTx.Vout[i]

					break
				}
			}

			if output == nil {
				return &Observation{
					Status: Invalid,
					TxID:   txid,
					Vout:   vout,
					Reason: "recorded unconfirmed funding transaction does not " +
						"contain the recorded vout",
				}, nil
			}

			derived, err := contract.Derive(ctx, node, swap, ch)

			if err != nil {
				return &Observation{
					Status: Invalid, TxID: txid, Vout: vout, Reason: err.Error(),
				}, nil
			}

			if err := derived.CheckOutput(output.Value, output.ScriptPubKey.Address); err != nil {
				return &Observation{
					Status: Invalid, TxID: txid, Vout: vout, Reason: err.Error(),
				}, nil
			}

			return &Observation{
				Status:        Pending,
				TxID:          txid,
				Vout:          vout,
				Confirmations: 0,
				Discovered:    discovered,
			}, nil
		}
	}

	//
	// The participant may already have broadcast its expected settlement and
	// recorded the txid while it is still unconfirmed. Proving the exact
	// mempool transaction spends this HTLC is a wait condition, not a manual
	// one, and it is the difference between waiting and rebroadcasting.
	//
	var pending []knownSpender

	for _, spend := range settlementSpenders(swap, ch) {
		if spend.populated() && !spend.confirmed() {
			pending = append(pending, spend)
		}
	}

	if len(pending) > 1 {
		return &Observation{
			Status: Invalid,
			TxID:   txid,
			Vout:   vout,
			Reason: fmt.Sprintf(
				"durable state records multiple unconfirmed %s settlement "+
					"transactions", chainName(ch),
			),
		}, nil
	}

	if len(pending) == 1 {
		spend := pending[0]

		settlementTxID := *spend.txid
		settlementTx, err := node.RawTx(ctx, settlementTxID, "")

		//
		// Failure to retrieve the transaction here is not ambiguity: it may
		// have just confirmed, or a node without txindex may no longer expose
		// it. Fall through to the hardened recovery path.
		//
		if err == nil && settlementTx != nil && settlementTx.Confirmations < 1 {
			return classifyPendingSettlement(
				settlementTx, swap, ch, txid, vout, settlementTxID, spend.branch, discovered,
			), nil
		}
	}

	spend, err := FindSpend(ctx, node, swap, ch, cfg)

	if err != nil {
		return &Observation{
			Status: PendingOrAmbiguous, TxID: txid, Vout: vout, Reason: err.Error(),
		}, nil
	}

	if spend == nil {
		return &Observation{
			Status: Unknown,
			TxID:   txid,
			Vout:   vout,
			Reason: "outpoint unavailable but no confirmed spender was found",
		}, nil
	}

	return &Observation{
		Status:         Spent,
		TxID:           txid,
		Vout:           vout,
		SpendTxID:      spend.TxID,
		SpendBlockHash: spend.BlockHash,
		Branch:         spend.Branch,
		Discovered:     discovered,
	}, nil
}

// classifyPendingSettlement proves an unconfirmed settlement uses the branch
// the durable record claims.
func classifyPendingSettlement(
	tx *chain.Tx,
	swap *model.Swap,
	ch chain.Chain,
	fundingTxID string,
	fundingVout int64,
	settlementTxID string,
	expected htlc.Branch,
	discovered bool,
) *Observation {
	invalid := func(reason string) *Observation {
		return &Observation{
			Status: PendingOrAmbiguous,
			TxID:   fundingTxID,
			Vout:   fundingVout,
			Reason: reason,
		}
	}

	if !spendsHTLC(tx, fundingTxID, fundingVout) {
		return invalid(
			"recorded pending settlement transaction does not spend this " +
				"swap's HTLC",
		)
	}

	actual, err := classify(tx, fundingTxID, fundingVout, swap.SecretHash)

	if err != nil {
		return invalid(err.Error())
	}

	if actual != expected {
		return invalid(fmt.Sprintf(
			"recorded pending settlement branch mismatch: expected %s, "+
				"observed %s", expected, actual,
		))
	}

	return &Observation{
		Status:        PendingSpend,
		TxID:          fundingTxID,
		Vout:          fundingVout,
		SpendTxID:     settlementTxID,
		Branch:        actual,
		Confirmations: 0,
		Discovered:    discovered,
	}
}
