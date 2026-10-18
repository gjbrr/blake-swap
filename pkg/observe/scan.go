package observe

import (
	"context"
	"fmt"

	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/contract"
	"github.com/gjbrr/blake-swap/pkg/model"
)

// Funding is one confirmed output that is the swap's HTLC.
type Funding struct {
	TxID      string
	Vout      int64
	BlockHash string
	Height    int64
	Address   string
	Amount    int64
}

// ScanFunding finds the unique confirmed funding output for a swap.
//
// Neither a coordination-layer txid nor a vout is trusted here: the expected
// script address and the expected amount are both deterministically derived
// from already-negotiated swap facts, so an output that matches them is this
// swap's HTLC and nothing else can be.
//
// The scan is bounded on purpose. A swap records a scan anchor at creation, so
// a scan much larger than the swap's age means the anchor is wrong, not that
// the swap is old; degrading to a genesis scan would be a denial of service
// against the participant's own node, and it would do it silently.
func ScanFunding(
	ctx context.Context,
	node Node,
	swap *model.Swap,
	ch chain.Chain,
	cfg Config,
) (*Funding, error) {
	derived, err := contract.Derive(ctx, node, swap, ch)

	if err != nil {
		return nil, err
	}

	tip, err := node.Height(ctx)

	if err != nil {
		return nil, err
	}

	start, err := scanAnchor(swap, ch, tip, cfg)

	if err != nil {
		return nil, err
	}

	blocks := tip - start + 1

	limit := cfg.MaxScanBlocks

	if limit <= 0 {
		limit = DefaultMaxScanBlocks
	}

	if blocks > limit {
		return nil, rejectf(
			"%s funding scan would cover %d blocks, above the %d-block limit "+
				"(BLAKE_SWAP_MAX_FUNDING_SCAN_BLOCKS). Refusing an unbounded "+
				"historical scan.",
			chainName(ch), blocks, limit,
		)
	}

	var matches []Funding

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
			if tx.TxID == "" {
				continue
			}

			for _, out := range tx.Vout {
				if out.ScriptPubKey.Address != derived.Address {
					continue
				}

				value, err := out.Value.Sats()

				if err != nil {
					continue
				}

				if int64(value) != derived.Amount {
					continue
				}

				matches = append(matches, Funding{
					TxID:      tx.TxID,
					Vout:      out.N,
					BlockHash: blockHash,
					Height:    height,
					Address:   derived.Address,
					Amount:    derived.Amount,
				})
			}
		}
	}

	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return &matches[0], nil
	}

	return nil, rejectf(
		"funding discovery is ambiguous. Found %d confirmed outputs matching "+
			"the exact %s HTLC address and amount. Refusing to guess which "+
			"output is canonical.",
		len(matches), chainName(ch),
	)
}

// scanAnchor reads the recorded anchor and bounds the scan against the tip.
//
// A missing anchor is refused rather than widened: see ScanFunding. The
// override exists only so a genuinely ancient durable state can be recovered
// by an operator who has decided the cost is acceptable.
func scanAnchor(swap *model.Swap, ch chain.Chain, tip int64, cfg Config) (int64, error) {
	var anchor *int64

	if ch == chain.Blake {
		anchor = swap.BlakeScanStartHeight
	} else {
		anchor = swap.BTCScanStartHeight
	}

	if anchor == nil {
		if !cfg.AllowUnanchoredScan {
			return 0, rejectf(
				"no recorded %s funding scan anchor. Refusing to scan from "+
					"genesis. Set BLAKE_SWAP_ALLOW_UNANCHORED_FUNDING_SCAN=1 to "+
					"force it for a legacy durable state.",
				chainName(ch),
			)
		}

		return 0, nil
	}

	start := *anchor

	if start < 0 {
		return 0, fmt.Errorf("Invalid %s funding scan anchor: %d", chainName(ch), start)
	}

	if start > tip {
		return 0, fmt.Errorf(
			"%s funding scan anchor %d is ahead of local tip %d; refusing to "+
				"guess after possible chain reset or inconsistent local state.",
			chainName(ch), start, tip,
		)
	}

	return start, nil
}
