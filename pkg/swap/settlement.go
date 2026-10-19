package swap

import (
	"context"
	"fmt"

	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/contract"
	"github.com/gjbrr/blake-swap/pkg/decimal"
	"github.com/gjbrr/blake-swap/pkg/htlc"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/observe"
	"github.com/gjbrr/blake-swap/pkg/settle"
	"github.com/gjbrr/blake-swap/pkg/state"
)

// SettlementResult is what a settlement command did.
type SettlementResult struct {
	Chain     chain.Chain
	Branch    htlc.Branch
	TxID      string
	BlockHash string
	Hex       string
	Pending   bool
	// Recovered means this call adopted a settlement the record already named
	// instead of signing a new one. A recovered settlement has no plan to report
	// and no fee this call measured, so it is rendered differently.
	Recovered   bool
	Rebroadcast bool
	State       state.State
	FeeSats     int64
	Vsize       int64
	FeeRate     decimal.Dec
	FeeSource   string
}

// recoveredBranch is a settlement the durable record already names.
type recoveredBranch struct {
	// Pending means the transaction is in the mempool and unconfirmed.
	Pending bool
	// Spend is the confirmed settlement, when there is one.
	Spend *observe.Spend
	// Rebroadcast means this call re-sent the stored signed transaction.
	Rebroadcast bool
}

// requireRecoveredBranch reports whether this participant already broadcast the
// expected settlement.
//
// It exists so a second invocation of a settlement command does not build a
// second transaction. That matters more than it sounds: two claims of the same
// HTLC are not two payments, they are a race, and one of them is wasted work at
// best and a stuck swap at worst.
//
// The cases are ordered by how much they can prove:
//
//  1. the recorded transaction is in the mempool — read it, classify it, wait;
//  2. the recorded signed hex is still known but the mempool forgot it (a node
//     restart legitimately loses its mempool) — prove the hex is the recorded
//     transaction, then rebroadcast it, but only while the HTLC output is still
//     unspent, because something else may already be spending it;
//  3. no recorded transaction — fall through to the hardened chain search.
func (e *Engine) requireRecoveredBranch(
	ctx context.Context, swap *model.Swap, ch chain.Chain, expected htlc.Branch,
) (*recoveredBranch, error) {
	if expected != htlc.Claim && expected != htlc.Refund {
		return nil, fmt.Errorf("unknown settlement branch: %s", expected)
	}

	node := e.nodeFor(ch)
	signer := e.nodeFor(ch)

	if node == nil {
		return nil, fmt.Errorf("the engine has no %s node", chainLabel(ch))
	}

	recordedTxID := settlementTxID(swap, ch, expected)
	recordedHex := settlementHex(swap, ch, expected)
	fundingTxID, fundingVout, err := contract.Outpoint(swap, ch)

	if err != nil {
		return nil, err
	}

	if recordedTxID != "" {
		inMempool, err := node.InMempool(ctx, recordedTxID)

		if err != nil {
			return nil, err
		}

		if inMempool {
			tx, err := node.RawTx(ctx, recordedTxID, "")

			if err != nil {
				return nil, err
			}

			actual, err := classifyTx(tx, fundingTxID, fundingVout, swap.SecretHash)

			if err != nil {
				return nil, err
			}

			if actual != expected {
				return nil, fmt.Errorf(
					"SECURITY FAILURE: recorded pending settlement uses %s branch; expected %s",
					actual, expected,
				)
			}

			return &recoveredBranch{Pending: true}, nil
		}

		if recordedHex != "" {
			decoded, err := node.DecodeRaw(ctx, recordedHex)

			if err != nil {
				return nil, err
			}

			if decoded.TxID != recordedTxID {
				return nil, fmt.Errorf(
					"SECURITY FAILURE: durable signed settlement transaction does not " +
						"match recorded txid",
				)
			}

			actual, err := classifyTx(decoded, fundingTxID, fundingVout, swap.SecretHash)

			if err != nil {
				return nil, err
			}

			if actual != expected {
				return nil, fmt.Errorf(
					"SECURITY FAILURE: durable signed settlement uses %s branch; expected %s",
					actual, expected,
				)
			}

			//
			// include_mempool semantics: if anything is already spending the
			// HTLC, gettxout reports nothing for it and the stored transaction
			// must not be rebroadcast.
			//
			utxo, err := node.TxOut(ctx, fundingTxID, fundingVout)

			if err != nil {
				return nil, err
			}

			if utxo != nil {
				rebroadcastTxID, err := node.SendRaw(ctx, recordedHex)

				if err != nil {
					//
					// The exact transaction may already be confirmed. Do not
					// infer that from node error text: ask the chain.
					//
					found, findErr := observe.FindSpend(ctx, node, swap, ch, e.observeConfig(ch))

					if findErr != nil {
						return nil, findErr
					}

					if found == nil {
						return nil, err
					}

					if found.Branch != expected {
						return nil, fmt.Errorf(
							"SECURITY FAILURE: HTLC was already spent by the %s branch; "+
								"expected %s", found.Branch, expected,
						)
					}

					if found.TxID != recordedTxID {
						return nil, fmt.Errorf(
							"SECURITY FAILURE: confirmed settlement txid differs from the " +
								"durable signed transaction",
						)
					}

					return &recoveredBranch{Spend: found}, nil
				}

				if rebroadcastTxID != recordedTxID {
					return nil, fmt.Errorf(
						"SECURITY FAILURE: exact settlement rebroadcast returned a " +
							"different txid",
					)
				}

				return &recoveredBranch{Pending: true, Rebroadcast: true}, nil
			}
		}
	}

	found, err := observe.FindSpend(ctx, node, swap, ch, e.observeConfig(ch))

	if err != nil {
		return nil, err
	}

	if found == nil {
		return nil, nil
	}

	if found.Branch != expected {
		return nil, fmt.Errorf(
			"SECURITY FAILURE: HTLC was already spent by the %s branch; expected %s",
			found.Branch, expected,
		)
	}

	if recordedTxID != "" && found.TxID != recordedTxID {
		return nil, fmt.Errorf(
			"SECURITY FAILURE: confirmed settlement txid differs from the exact " +
				"transaction previously broadcast",
		)
	}

	_ = signer

	return &recoveredBranch{Spend: found}, nil
}

// nodeFor returns the node for one chain.
func (e *Engine) nodeFor(ch chain.Chain) Node {
	if ch == chain.Blake {
		return e.Blake
	}

	return e.BTC
}

// observeConfig is the observation policy for one chain.
func (e *Engine) observeConfig(ch chain.Chain) observe.Config {
	wallet := e.BlakeWallet

	if ch == chain.BTC {
		wallet = e.BTCWallet
	}

	return observe.Config{Wallet: wallet}
}

// settlementTxID is the recorded settlement transaction for one branch.
func settlementTxID(swap *model.Swap, ch chain.Chain, branch htlc.Branch) string {
	field := settlementFields(swap, ch, branch)

	if field.txid == nil {
		return ""
	}

	return *field.txid
}

// settlementHex is the recorded signed settlement for one branch.
func settlementHex(swap *model.Swap, ch chain.Chain, branch htlc.Branch) string {
	field := settlementFields(swap, ch, branch)

	if field.hex == nil {
		return ""
	}

	return *field.hex
}

// settlementFieldSet is the trio of fields one settlement branch owns.
type settlementFieldSet struct {
	txid      *string
	blockHash *string
	hex       *string
}

// settlementFields locates the fields for one chain and branch.
func settlementFields(swap *model.Swap, ch chain.Chain, branch htlc.Branch) settlementFieldSet {
	if ch == chain.Blake {
		if branch == htlc.Claim {
			return settlementFieldSet{swap.BlakeClaimTxID, swap.BlakeClaimBlockHash, swap.BlakeClaimHex}
		}

		return settlementFieldSet{swap.BlakeRefundTxID, swap.BlakeRefundBlockHash, swap.BlakeRefundHex}
	}

	if branch == htlc.Claim {
		return settlementFieldSet{swap.BTCCLaimTxID, swap.BTCCLaimBlockHash, swap.BTCCLaimHex}
	}

	return settlementFieldSet{swap.BTCRefundTxID, swap.BTCRefundBlockHash, swap.BTCRefundHex}
}

// setSettlement records one settlement on the swap.
//
// The signed hex and the block hash are only written when they are known. An
// announcement carries no hex: blanking the field would discard rebroadcast
// material a crash window depends on, and an empty string is not valid hex for the
// record's own validation.
func setSettlement(swap *model.Swap, ch chain.Chain, branch htlc.Branch, txid, blockHash, hex string) {
	if ch == chain.Blake {
		if branch == htlc.Claim {
			swap.BlakeClaimTxID = &txid
			swap.BlakeClaimBlockHash = blockOrNil(blockHash, swap.BlakeClaimBlockHash)

			if hex != "" {
				swap.BlakeClaimHex = &hex
			}

			return
		}

		swap.BlakeRefundTxID = &txid
		swap.BlakeRefundBlockHash = blockOrNil(blockHash, swap.BlakeRefundBlockHash)

		if hex != "" {
			swap.BlakeRefundHex = &hex
		}

		return
	}

	if branch == htlc.Claim {
		swap.BTCCLaimTxID = &txid
		swap.BTCCLaimBlockHash = blockOrNil(blockHash, swap.BTCCLaimBlockHash)

		if hex != "" {
			swap.BTCCLaimHex = &hex
		}

		return
	}

	swap.BTCRefundTxID = &txid
	swap.BTCRefundBlockHash = blockOrNil(blockHash, swap.BTCRefundBlockHash)

	if hex != "" {
		swap.BTCRefundHex = &hex
	}
}

// blockOrNil returns the announced block hash, or the existing value when the
// caller does not know it. A pending settlement has no block hash yet, and writing
// an empty one would look like a confirmed settlement whose block was lost.
func blockOrNil(blockHash string, existing *string) *string {
	if blockHash != "" {
		return &blockHash
	}

	return existing
}

// classifyTx asks the HTLC classifier which branch a transaction used.
func classifyTx(tx *chain.Tx, fundingTxID string, fundingVout int64, secretHash string) (htlc.Branch, error) {
	inputs := make([]htlc.Input, 0, len(tx.Vin))

	for _, in := range tx.Vin {
		inputs = append(inputs, htlc.Input{TxID: in.TxID, Vout: in.Vout, Witness: in.Witness})
	}

	return htlc.ClassifySpend(htlc.Tx{Vin: inputs}, fundingTxID, fundingVout, secretHash)
}

// chainLabel is a chain's name as the messages spell it.
func chainLabel(ch chain.Chain) string {
	if ch == chain.Blake {
		return "BLAKE"
	}

	return "Bitcoin"
}

// settlementOptions maps the engine's settings onto the settlement layer's.
func (e *Engine) settlementOptions() settle.Options {
	return settle.Options{
		FeeRate:         e.Settlement.FeeRate,
		FeeSats:         e.Settlement.FeeSats,
		Multiplier:      e.Settlement.Multiplier,
		MaxPercent:      e.Settlement.MaxPercent,
		MaxSats:         e.Settlement.MaxSats,
		FeeTargetBlocks: e.Settlement.FeeTargetBlocks,
	}
}

// walletFor returns the signing wallet name for one chain.
func (e *Engine) walletForSettlement(ch chain.Chain) string {
	if ch == chain.Blake {
		return e.BlakeWallet
	}

	return e.BTCWallet
}

// settleAndRecord builds, records, broadcasts and confirms one settlement.
//
// The order is the durability contract, not an implementation detail:
//
//  1. build and finalize the transaction;
//  2. derive its identity and prove it spends the HTLC the way this branch must;
//  3. persist the signed candidate — txid and hex — *before* it exists on any
//     node;
//  4. broadcast, and require the node to return the txid that was persisted;
//  5. observe and confirm it;
//  6. advance the protocol state and persist that.
//
// Step 3 used to come after step 4. That is the same order right up until the
// process dies in between: after a broadcast the transaction is already mining,
// so a record written afterwards is a record that may never be written. A
// participant that dies with money in flight must come back able to recognise its
// own transaction — that is what the stored hex is for — and it cannot recognise
// what it never recorded.
func (e *Engine) settleAndRecord(
	ctx context.Context,
	swap *model.Swap,
	ch chain.Chain,
	branch htlc.Branch,
	build func(destination string) (*settle.Plan, error),
	advance func(*model.Swap) error,
) (*SettlementResult, error) {
	node := e.nodeFor(ch)

	wallet := e.walletForSettlement(ch)

	destination, err := node.Bind(wallet).NewAddress(ctx)

	if err != nil {
		return nil, err
	}

	plan, err := build(destination)

	if err != nil {
		return nil, err
	}

	// The transaction is final, so its identity and its shape can both be proved
	// before it is announced to anyone. Doing it here means a mis-signed
	// settlement is refused instead of persisted and broadcast.
	decoded, err := node.DecodeRaw(ctx, plan.Hex)

	if err != nil {
		return nil, err
	}

	if decoded.TxID == "" {
		return nil, fmt.Errorf(
			"signed %s %s has no decodable txid", chainLabel(ch), branch,
		)
	}

	if err := e.verifySettlementShape(swap, ch, branch, decoded); err != nil {
		return nil, err
	}

	// Measured from the finalized transaction rather than assumed from the
	// request, and measured *before* the broadcast: a failure after money moved
	// would otherwise lose the fee the operator needs to see.
	fee, err := settle.SpendFee(ctx, node, plan.Hex, inputSats(swap, ch))

	if err != nil {
		return nil, err
	}

	setSettlement(swap, ch, branch, decoded.TxID, "", plan.Hex)

	if err := e.save(ctx, swap); err != nil {
		return nil, err
	}

	failpoint(FailpointName(ch, branch, BoundaryCandidateSave))

	broadcast, err := node.SendRaw(ctx, plan.Hex)

	if err != nil {
		return nil, fmt.Errorf("%s %s broadcast: %w", chainLabel(ch), branch, err)
	}

	if broadcast != decoded.TxID {
		return nil, fmt.Errorf(
			"SECURITY FAILURE: %s %s broadcast returned a different txid",
			chainLabel(ch), branch,
		)
	}

	if err := crashAfterBroadcast(ch, branch); err != nil {
		return nil, err
	}

	failpoint(FailpointName(ch, branch, BoundaryBroadcast))

	result := &SettlementResult{
		Chain:     ch,
		Branch:    branch,
		TxID:      decoded.TxID,
		Hex:       plan.Hex,
		Pending:   true,
		State:     swap.State,
		FeeSats:   fee,
		Vsize:     plan.Vsize,
		FeeRate:   plan.Rate,
		FeeSource: plan.Source,
	}

	if e.Funding.AutoMine > 0 {
		if _, err := node.MineToAddress(ctx, e.Funding.AutoMine, destination); err != nil {
			return nil, err
		}
	}

	recovered, err := e.requireRecoveredBranch(ctx, swap, ch, branch)

	if err != nil {
		return nil, err
	}

	if recovered == nil {
		return nil, fmt.Errorf(
			"%s %s broadcast is neither pending nor confirmed", chainLabel(ch), branch,
		)
	}

	if recovered.Pending {
		return result, nil
	}

	setSettlement(swap, ch, branch, recovered.Spend.TxID, recovered.Spend.BlockHash, plan.Hex)

	if err := advance(swap); err != nil {
		return nil, err
	}

	failpoint(FailpointName(ch, branch, BoundaryConfirmation))

	if err := e.save(ctx, swap); err != nil {
		return nil, err
	}

	result.TxID = recovered.Spend.TxID
	result.BlockHash = recovered.Spend.BlockHash
	result.Pending = false
	result.State = swap.State

	return result, nil
}

// verifySettlementShape requires a signed settlement to spend the HTLC the way
// this branch is supposed to.
//
// The witness shape is what distinguishes a claim from a refund, and the two move
// money in opposite directions. A signature that classified as the other branch
// would be a transaction that does the opposite of what the record will say it
// did, so it is refused before it is stored or broadcast rather than discovered
// later.
func (e *Engine) verifySettlementShape(
	swap *model.Swap, ch chain.Chain, branch htlc.Branch, decoded *chain.Tx,
) error {
	fundingTxID, fundingVout, err := contract.Outpoint(swap, ch)

	if err != nil {
		return err
	}

	actual, err := classifyTx(decoded, fundingTxID, fundingVout, swap.SecretHash)

	if err != nil {
		return err
	}

	if actual != branch {
		return fmt.Errorf(
			"SECURITY FAILURE: signed %s %s classified as %s",
			chainLabel(ch), branch, actual,
		)
	}

	return nil
}

// inputSats is the HTLC's value in the chain's smallest unit.
func inputSats(swap *model.Swap, ch chain.Chain) int64 {
	if ch == chain.Blake {
		return swap.BlakeAmountBaseUnits
	}

	return swap.BTCSats
}
