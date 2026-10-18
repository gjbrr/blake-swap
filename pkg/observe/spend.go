package observe

import (
	"context"
	"fmt"

	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/htlc"
	"github.com/gjbrr/blake-swap/pkg/model"
)

// Spend is a confirmed transaction that spends the swap's HTLC.
type Spend struct {
	TxID      string
	BlockHash string
	Branch    htlc.Branch
}

// FindSpend finds a confirmed transaction spending this swap's HTLC.
//
// The careful part is what it does when it does not find one. An HTLC with no
// spendable output and no confirmed spender may already be spent by a
// transaction in the mempool, and broadcasting a competing settlement then
// would be irreversible. So that case is an error, not a nil result, and it is
// worded so the operator cannot mistake it for "nothing happened".
//
// A recorded settlement txid is a fast path, not an authority. It is verified
// exactly like a discovered one: the transaction must spend this outpoint, use
// the recorded branch, be confirmed, and still be on the active chain.
func FindSpend(
	ctx context.Context,
	node Node,
	swap *model.Swap,
	ch chain.Chain,
	cfg Config,
) (*Spend, error) {
	anchor := anchorFor(swap, ch)

	if anchor.TxID == nil || *anchor.TxID == "" {
		return nil, fmt.Errorf("swap has no %s funding txid", chainName(ch))
	}

	if anchor.Vout == nil {
		return nil, fmt.Errorf("swap has no %s funding vout", chainName(ch))
	}

	txid := *anchor.TxID
	vout := int64(*anchor.Vout)

	//
	// Fast path: the durable record already names the settlement. It is worth
	// using because the alternative is scanning every block since funding,
	// but it is not worth trusting.
	//
	var populated []knownSpender

	for _, candidate := range settlementSpenders(swap, ch) {
		if candidate.populated() {
			populated = append(populated, candidate)
		}
	}

	if len(populated) > 1 {
		return nil, rejectf(
			"durable state records multiple %s HTLC settlement transactions",
			chainName(ch),
		)
	}

	if len(populated) == 1 {
		return verifyRecordedSpend(ctx, node, swap, ch, anchored{
			txID: txid, vout: vout,
		}, populated[0])
	}

	start, err := spendScanStart(ctx, node, swap, ch, anchor)

	if err != nil {
		return nil, err
	}

	tip, err := node.Height(ctx)

	if err != nil {
		return nil, err
	}

	for height := start; height <= tip; height++ {
		blockHash, err := node.BlockHash(ctx, height)

		if err != nil {
			return nil, err
		}

		block, err := node.BlockTxs(ctx, blockHash)

		if err != nil {
			return nil, err
		}

		for _, tx := range block.Tx {
			if !spendsHTLC(&tx, txid, vout) {
				continue
			}

			branch, err := classify(&tx, txid, vout, swap.SecretHash)

			if err != nil {
				return nil, err
			}

			return &Spend{TxID: tx.TxID, BlockHash: blockHash, Branch: branch}, nil
		}
	}

	//
	// No confirmed spend. gettxout includes mempool effects, so an absent
	// output here means something may already be spending it.
	//
	utxo, err := node.TxOut(ctx, txid, vout)

	if err != nil {
		return nil, err
	}

	if utxo == nil {
		return nil, rejectf(
			"%s HTLC outpoint is unavailable, but no confirmed spender was "+
				"found. It may already have an unconfirmed spender. Refusing to "+
				"rebroadcast.",
			chainName(ch),
		)
	}

	return nil, nil
}

// anchored is the funding outpoint, named so the verification helpers do not
// take four positional strings.
type anchored struct {
	txID string
	vout int64
}

// spendScanStart is the height from which a spender scan may begin.
//
// A funding block hash is a precise starting point; without it the recorded
// anchor is used, and if that is missing the scan is refused rather than
// widened to genesis. Production state can legitimately lose the funding block
// hash (the outpoint is recorded while the funding is still pending), which is
// why the anchor exists at all.
func spendScanStart(
	ctx context.Context,
	node Node,
	swap *model.Swap,
	ch chain.Chain,
	anchor fundingAnchor,
) (int64, error) {
	if anchor.BlockHash != nil && *anchor.BlockHash != "" {
		block, err := node.BlockTxs(ctx, *anchor.BlockHash)

		if err != nil {
			return 0, err
		}

		return block.Height, nil
	}

	field := "btc_scan_start_height"
	value := swap.BTCScanStartHeight

	if ch == chain.Blake {
		field = "blake_scan_start_height"
		value = swap.BlakeScanStartHeight
	}

	if value == nil {
		return 0, rejectf(
			"%s funding blockhash is unknown and %s is unavailable; refusing "+
				"an unbounded spender scan",
			chainName(ch), field,
		)
	}

	return *value, nil
}

// verifyRecordedSpend proves a settlement the durable record names.
func verifyRecordedSpend(
	ctx context.Context,
	node Node,
	swap *model.Swap,
	ch chain.Chain,
	funding anchored,
	known knownSpender,
) (*Spend, error) {
	settlementTxID := *known.txid

	var (
		tx  *chain.Tx
		err error
	)

	if !known.confirmed() {
		//
		// A known txid without a block hash is the normal pending condition:
		// the engine persists the broadcast txid immediately and only records
		// the block once the transaction confirms.
		//
		tx, err = node.RawTx(ctx, settlementTxID, "")

		if err != nil {
			//
			// Nodes without txindex cannot necessarily retrieve a
			// wallet-created transaction through getrawtransaction once it has
			// left the mempool. This participant created it, so recover it from
			// the local wallet and decode the raw hex instead.
			//
			walletTx, walletErr := node.WalletTx(ctx, settlementTxID)

			if walletErr != nil {
				return nil, rejectf(
					"durable state records %s %s txid but the transaction "+
						"cannot be independently verified",
					chainName(ch), known.branch,
				)
			}

			tx, err = node.DecodeRaw(ctx, walletTx.Hex)

			if err != nil {
				return nil, rejectf(
					"durable state records %s %s txid but the transaction "+
						"cannot be independently verified",
					chainName(ch), known.branch,
				)
			}

			tx.Confirmations = walletTx.Confirmations

			if walletTx.BlockHash != "" {
				tx.BlockHash = walletTx.BlockHash
			}
		}

		if !spendsHTLC(tx, funding.txID, funding.vout) {
			return nil, rejectf(
				"recorded %s %s transaction does not spend this swap's HTLC",
				chainName(ch), known.branch,
			)
		}

		branch, err := classify(tx, funding.txID, funding.vout, swap.SecretHash)

		if err != nil {
			return nil, err
		}

		if branch != known.branch {
			return nil, rejectf(
				"recorded %s settlement branch mismatch: expected %s, observed %s",
				chainName(ch), known.branch, branch,
			)
		}

		if tx.Confirmations < 1 {
			return nil, pendf(
				"recorded %s %s transaction is unconfirmed; refusing to rebroadcast",
				chainName(ch), known.branch,
			)
		}

		if tx.BlockHash == "" {
			return nil, rejectf(
				"confirmed recorded %s %s transaction has no blockhash",
				chainName(ch), known.branch,
			)
		}

		return &Spend{
			TxID: settlementTxID, BlockHash: tx.BlockHash, Branch: branch,
		}, nil
	}

	tx, err = node.RawTx(ctx, settlementTxID, *known.blockHash)

	if err != nil {
		return nil, err
	}

	return verifyConfirmedSpend(tx, verification{
		ch:         ch,
		branch:     known.branch,
		secretHash: swap.SecretHash,
		funding:    funding,
	}, settlementTxID, *known.blockHash)
}

// verification is what a confirmed settlement is checked against, so the two
// paths into confirmations share one check rather than two that can drift.
type verification struct {
	ch         chain.Chain
	branch     htlc.Branch
	secretHash string
	funding    anchored
}

// verifyConfirmedSpend checks a confirmed settlement transaction.
func verifyConfirmedSpend(tx *chain.Tx, v verification, txid, blockHash string) (*Spend, error) {
	if tx.InActiveChain != nil && !*tx.InActiveChain {
		return nil, rejectf(
			"recorded %s %s transaction is no longer on the active chain",
			chainName(v.ch), v.branch,
		)
	}

	if tx.Confirmations < 1 {
		return nil, rejectf(
			"recorded %s %s transaction is not confirmed on the active chain",
			chainName(v.ch), v.branch,
		)
	}

	if !spendsHTLC(tx, v.funding.txID, v.funding.vout) {
		return nil, rejectf(
			"recorded %s %s transaction does not spend this swap's HTLC",
			chainName(v.ch), v.branch,
		)
	}

	branch, err := classify(tx, v.funding.txID, v.funding.vout, v.secretHash)

	if err != nil {
		return nil, err
	}

	if branch != v.branch {
		return nil, rejectf(
			"recorded %s settlement branch mismatch: expected %s, observed %s",
			chainName(v.ch), v.branch, branch,
		)
	}

	return &Spend{TxID: txid, BlockHash: blockHash, Branch: branch}, nil
}
