package swap

import (
	"context"
	"fmt"

	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/contract"
	"github.com/gjbrr/blake-swap/pkg/fund"
	"github.com/gjbrr/blake-swap/pkg/htlc"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/observe"
	"github.com/gjbrr/blake-swap/pkg/state"
)

// SyncResult is what a chain reconciliation recovered.
type SyncResult struct {
	SwapID    string
	Start     state.State
	End       state.State
	Recovered []string
}

// SyncFunding reconciles funding state directly from confirmed chain history.
//
// It deliberately does not require a coordination message: if the peer's announcement
// never arrived, or the message spool was lost, the chain is still the authority on
// whether the HTLC exists. One invocation may recover more than one stage, because a
// state that is stale by two events is exactly the state an operator reaches for this
// command with.
func (e *Engine) SyncFunding(ctx context.Context, swapID string) (*SyncResult, error) {
	swap, err := e.load(ctx, swapID)

	if err != nil {
		return nil, err
	}

	result := &SyncResult{SwapID: swap.SwapID, Start: swap.State, End: swap.State}

	// A hard bound, not an expected count: the funding graph has two edges.
	for attempt := 0; attempt < 3; attempt++ {
		switch swap.State {
		case state.Offered:
			funding, err := e.findConfirmedFunding(ctx, swap, chain.Blake)

			if err != nil {
				return result, err
			}

			if funding == nil {
				result.End = swap.State

				return result, nil
			}

			setFunding(swap, chain.Blake, ptr(funding.TxID), ptr(int32(funding.Vout)), ptr(funding.BlockHash))

			if err := swap.Transition(state.BlakeFunded); err != nil {
				return result, err
			}

			if err := e.save(ctx, swap); err != nil {
				return result, err
			}

			result.Recovered = append(result.Recovered, "blake funding")
		case state.BlakeFunded:
			funding, err := e.findConfirmedFunding(ctx, swap, chain.BTC)

			if err != nil {
				return result, err
			}

			if funding == nil {
				result.End = swap.State

				return result, nil
			}

			setFunding(swap, chain.BTC, ptr(funding.TxID), ptr(int32(funding.Vout)), ptr(funding.BlockHash))

			if err := swap.Transition(state.BTCFunded); err != nil {
				return result, err
			}

			if err := e.save(ctx, swap); err != nil {
				return result, err
			}

			result.Recovered = append(result.Recovered, "btc funding")
		default:
			result.End = swap.State

			return result, nil
		}

		result.End = swap.State
	}

	return result, nil
}

// SyncSettlement reconciles durable state with confirmed settlement truth.
//
// Coordination messages are not required: the participant already knows the exact
// HTLC outpoints from previously verified state, so the chain is asked which branch was
// spent and the record is advanced only when the branch is the one the state permits.
func (e *Engine) SyncSettlement(ctx context.Context, swapID string) (*SyncResult, error) {
	swap, err := e.load(ctx, swapID)

	if err != nil {
		return nil, err
	}

	result := &SyncResult{SwapID: swap.SwapID, Start: swap.State, End: swap.State}

	// A hard bound against a loop that could otherwise keep recovering.
	for attempt := 0; attempt < 6; attempt++ {
		var (
			ch        chain.Chain
			permitted []htlc.Branch
		)

		switch swap.State {
		case state.Complete, state.BlakeClaimed, state.BlakeRefunded, state.Offered:
			result.End = swap.State

			return result, nil
		case state.BlakeFunded:
			ch = chain.Blake
			permitted = []htlc.Branch{htlc.Refund}
		case state.BTCFunded:
			ch = chain.BTC
			permitted = []htlc.Branch{htlc.Claim, htlc.Refund}
		case state.BTCClaimed:
			ch = chain.Blake
			permitted = []htlc.Branch{htlc.Claim}
		case state.BTCRefunded:
			ch = chain.Blake
			permitted = []htlc.Branch{htlc.Refund}
		case state.SecretRevealed:
			return result, fmt.Errorf(
				"SECURITY FAILURE: durable state is unexpectedly SECRET_REVEALED; " +
					"this intermediate state should not exist independently on disk",
			)
		default:
			return result, fmt.Errorf(
				"cannot reconcile settlement from state %s", swap.State,
			)
		}

		spend, err := observe.FindSpend(ctx, e.nodeFor(ch), swap, ch, e.observeConfig(ch))

		if err != nil {
			return result, err
		}

		if spend == nil {
			result.End = swap.State

			return result, nil
		}

		if err := applySettlementSpend(e, ctx, swap, ch, spend, permitted); err != nil {
			return result, err
		}

		if err := e.save(ctx, swap); err != nil {
			return result, err
		}

		result.Recovered = append(result.Recovered, string(ch)+" "+string(spend.Branch))
		result.End = swap.State
	}

	return result, nil
}

// applySettlementSpend records one confirmed settlement and advances the state.
func applySettlementSpend(
	e *Engine,
	ctx context.Context,
	swap *model.Swap,
	ch chain.Chain,
	spend *observe.Spend,
	permitted []htlc.Branch,
) error {
	allowed := false

	for _, branch := range permitted {
		if spend.Branch == branch {
			allowed = true

			break
		}
	}

	if !allowed {
		return fmt.Errorf(
			"SECURITY FAILURE: confirmed HTLC spend is inconsistent with local "+
				"protocol state. state=%s chain=%s branch=%s",
			swap.State, ch, spend.Branch,
		)
	}

	setSettlement(swap, ch, spend.Branch, spend.TxID, spend.BlockHash, settlementHex(swap, ch, spend.Branch))

	switch {
	case ch == chain.BTC && spend.Branch == htlc.Claim:
		if err := swap.Transition(state.SecretRevealed); err != nil {
			return err
		}

		if err := swap.Transition(state.BTCClaimed); err != nil {
			return err
		}

		//
		// The party who did not claim must recover the preimage from the chain. It is
		// the only way they can claim the other leg, and verifying it against the
		// swap's own hash is what makes "recovered from Bitcoin" mean something.
		//
		if role, _, _, _, err := e.reconcileIdentity(swap); err == nil && role == "bob" {
			if _, err := e.extractSecretInto(ctx, swap); err != nil {
				return err
			}
		}
	case ch == chain.BTC && spend.Branch == htlc.Refund:
		if err := swap.Transition(state.BTCRefunded); err != nil {
			return err
		}
	case ch == chain.Blake && spend.Branch == htlc.Claim:
		if err := swap.Transition(state.BlakeClaimed); err != nil {
			return err
		}
	case ch == chain.Blake && spend.Branch == htlc.Refund:
		if err := swap.Transition(state.BlakeRefunded); err != nil {
			return err
		}
	}

	return nil
}

// findConfirmedFunding finds a confirmed funding output for one leg.
//
// Three sources, in order of precision: the outpoint this record already names, a
// payment this participant's own watch-only wallet knows about, and finally the bounded
// chain scan. The first is exact, the second is what a lost announcement leaves behind,
// and the third is the fallback for a record that never learned the outpoint at all.
func (e *Engine) findConfirmedFunding(
	ctx context.Context, swap *model.Swap, ch chain.Chain,
) (*observe.Funding, error) {
	node := e.nodeFor(ch)

	derived, err := contract.Derive(ctx, node, swap, ch)

	if err != nil {
		return nil, err
	}

	// 1. The recorded outpoint, if the chain agrees it is still there.
	if txid, vout, err := contract.Outpoint(swap, ch); err == nil {
		if err := contract.VerifyFunding(ctx, node, swap, ch); err == nil {
			if anchor, err := e.anchorOf(ctx, swap, ch, txid, vout); err == nil {
				return &observe.Funding{
					TxID: anchor.TxID, Vout: anchor.Vout,
					BlockHash: anchor.BlockHash, Address: derived.Address,
					Amount: derived.Amount,
				}, nil
			}
		}
	}

	// 2. A payment this participant's watch-only wallet recorded.
	watch := watchWallet(e, ch)

	if watch != "" {
		payment, err := fund.FindPayment(ctx, node.Bind(watch), derived.Address, derived.Amount)

		if err == nil && payment != nil && payment.BlockHash != "" {
			anchor, err := e.anchorOf(ctx, swap, ch, payment.TxID, payment.Vout)

			if err == nil {
				return &observe.Funding{
					TxID: anchor.TxID, Vout: anchor.Vout,
					BlockHash: anchor.BlockHash, Address: derived.Address,
					Amount: derived.Amount,
				}, nil
			}
		}
	}

	// 3. The bounded scan.
	return observe.ScanFunding(ctx, node, swap, ch, e.observeConfig(ch))
}

// anchorOf proves one outpoint and returns its anchor.
func (e *Engine) anchorOf(
	ctx context.Context, swap *model.Swap, ch chain.Chain, txid string, vout int64,
) (*fund.Anchor, error) {
	node := e.nodeFor(ch)

	utxo, err := node.TxOut(ctx, txid, vout)

	if err != nil {
		return nil, err
	}

	if utxo == nil || utxo.Confirmations < 1 {
		return nil, fmt.Errorf("%s funding output is not confirmed", chainLabel(ch))
	}

	tip, err := node.Height(ctx)

	if err != nil {
		return nil, err
	}

	height := tip - utxo.Confirmations + 1

	blockHash, err := node.BlockHash(ctx, height)

	if err != nil {
		return nil, err
	}

	return fund.VerifyAnchor(ctx, node, swap, ch, txid, vout, blockHash)
}

// ptr returns a pointer to a copy of a value.
func ptr[T any](value T) *T { return &value }
