package swap

import (
	"context"
	"fmt"

	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/contract"
	"github.com/gjbrr/blake-swap/pkg/htlc"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/observe"
	"github.com/gjbrr/blake-swap/pkg/settle"
	"github.com/gjbrr/blake-swap/pkg/state"
)

// ClaimBTC spends Bitcoin's hashlock branch, which is what reveals the secret.
//
// Alice can only do this in BTC_FUNDED, and only with the secret she created. The
// claim is the protocol's pivot: once it is confirmed, Bob can read the preimage
// off the chain and claim BLAKE, which is why the durable state moves through
// SECRET_REVEALED before BTC_CLAIMED.
func (e *Engine) ClaimBTC(ctx context.Context, swapID string) (*SettlementResult, error) {
	swap, err := e.load(ctx, swapID)

	if err != nil {
		return nil, err
	}

	if _, err := e.requireRole(swap, "alice", "BTC claim"); err != nil {
		return nil, err
	}

	if swap.State != state.BTCFunded {
		return nil, fmt.Errorf("claim-btc requires state BTC_FUNDED")
	}

	recovered, err := e.requireRecoveredBranch(ctx, swap, chain.BTC, htlc.Claim)

	if err != nil {
		return nil, err
	}

	if recovered != nil {
		return e.finishRecovered(ctx, swap, chain.BTC, htlc.Claim, recovered, func(swap *model.Swap) error {
			if err := swap.Transition(state.SecretRevealed); err != nil {
				return err
			}

			return swap.Transition(state.BTCClaimed)
		})
	}

	if err := contract.VerifyFunding(ctx, e.BTC, swap, chain.BTC); err != nil {
		return nil, err
	}

	if swap.SecretHex == nil || *swap.SecretHex == "" {
		return nil, fmt.Errorf("this swap has no secret to claim with")
	}

	derived, err := contract.Derive(ctx, e.BTC, swap, chain.BTC)

	if err != nil {
		return nil, err
	}

	return e.settleAndRecord(ctx, swap, chain.BTC, htlc.Claim, func(destination string) (*settle.Plan, error) {
		return settle.Claim(ctx, e.BTC, settle.Request{
			Chain:       chain.BTC,
			Funding:     chain.Outpoint{TxID: *swap.BTCFundingTxID, Vout: int64(*swap.BTCFundingVout)},
			Destination: destination,
			Descriptor:  derived.Descriptor,
			InputSats:   swap.BTCSats,
			SecretHash:  swap.SecretHash,
			Preimage:    *swap.SecretHex,
			Wallet:      e.BTCWallet,
		}, e.settlementOptions())
	}, func(swap *model.Swap) error {
		if err := swap.Transition(state.SecretRevealed); err != nil {
			return err
		}

		return swap.Transition(state.BTCClaimed)
	})
}

// ClaimBlake spends BLAKE's hashlock branch with the secret Bob read off
// Bitcoin.
//
// The preimage is the *revealed* one, never a local copy: Bob's record gains it
// only from Alice's confirmed Bitcoin claim, and using anything else would claim
// with a secret the chain does not already hold.
func (e *Engine) ClaimBlake(ctx context.Context, swapID string) (*SettlementResult, error) {
	swap, err := e.load(ctx, swapID)

	if err != nil {
		return nil, err
	}

	if _, err := e.requireRole(swap, "bob", "BLAKE claim"); err != nil {
		return nil, err
	}

	if swap.State != state.BTCClaimed {
		return nil, fmt.Errorf("claim-blake requires state BTC_CLAIMED")
	}

	recovered, err := e.requireRecoveredBranch(ctx, swap, chain.Blake, htlc.Claim)

	if err != nil {
		return nil, err
	}

	if recovered != nil {
		return e.finishRecovered(ctx, swap, chain.Blake, htlc.Claim, recovered, func(swap *model.Swap) error {
			return swap.Transition(state.BlakeClaimed)
		})
	}

	if err := contract.VerifyFunding(ctx, e.Blake, swap, chain.Blake); err != nil {
		return nil, err
	}

	if swap.RevealedSecretHex == nil || *swap.RevealedSecretHex == "" {
		return nil, fmt.Errorf(
			"this swap has no revealed secret; claim-blake requires the preimage " +
				"from Bitcoin",
		)
	}

	derived, err := contract.Derive(ctx, e.Blake, swap, chain.Blake)

	if err != nil {
		return nil, err
	}

	return e.settleAndRecord(ctx, swap, chain.Blake, htlc.Claim, func(destination string) (*settle.Plan, error) {
		return settle.Claim(ctx, e.Blake, settle.Request{
			Chain:       chain.Blake,
			Funding:     chain.Outpoint{TxID: *swap.BlakeFundingTxID, Vout: int64(*swap.BlakeFundingVout)},
			Destination: destination,
			Descriptor:  derived.Descriptor,
			InputSats:   swap.BlakeAmountBaseUnits,
			SecretHash:  swap.SecretHash,
			Preimage:    *swap.RevealedSecretHex,
			Wallet:      e.BlakeWallet,
		}, e.settlementOptions())
	}, func(swap *model.Swap) error {
		return swap.Transition(state.BlakeClaimed)
	})
}

// RefundBTC spends Bitcoin's timelock branch: Bob's fallback when Alice never
// claims.
func (e *Engine) RefundBTC(ctx context.Context, swapID string) (*SettlementResult, error) {
	swap, err := e.load(ctx, swapID)

	if err != nil {
		return nil, err
	}

	if _, err := e.requireRole(swap, "bob", "BTC refund"); err != nil {
		return nil, err
	}

	if swap.State != state.BTCFunded {
		return nil, fmt.Errorf("refund-btc requires state BTC_FUNDED")
	}

	recovered, err := e.requireRecoveredBranch(ctx, swap, chain.BTC, htlc.Refund)

	if err != nil {
		return nil, err
	}

	if recovered != nil {
		return e.finishRecovered(ctx, swap, chain.BTC, htlc.Refund, recovered, func(swap *model.Swap) error {
			return swap.Transition(state.BTCRefunded)
		})
	}

	if err := contract.VerifyFunding(ctx, e.BTC, swap, chain.BTC); err != nil {
		return nil, err
	}

	derived, err := contract.Derive(ctx, e.BTC, swap, chain.BTC)

	if err != nil {
		return nil, err
	}

	return e.settleAndRecord(ctx, swap, chain.BTC, htlc.Refund, func(destination string) (*settle.Plan, error) {
		return settle.Refund(ctx, e.BTC, settle.Request{
			Chain:       chain.BTC,
			Funding:     chain.Outpoint{TxID: *swap.BTCFundingTxID, Vout: int64(*swap.BTCFundingVout)},
			Destination: destination,
			Descriptor:  derived.Descriptor,
			InputSats:   swap.BTCSats,
			Wallet:      e.BTCWallet,
			LockTime:    swap.BTCTimeout,
		}, e.settlementOptions())
	}, func(swap *model.Swap) error {
		return swap.Transition(state.BTCRefunded)
	})
}

// RefundBlake spends BLAKE's timelock branch after the Bitcoin side was refunded.
//
// The BTC_REFUNDED state is re-proved against the active chain first. Stale local
// state is not authority to move money: a reorg can put the Bitcoin HTLC back,
// and Alice refunding BLAKE while Bob's Bitcoin refund is gone would leave one
// side paid and the other claimed.
func (e *Engine) RefundBlake(ctx context.Context, swapID string) (*SettlementResult, error) {
	swap, err := e.load(ctx, swapID)

	if err != nil {
		return nil, err
	}

	if _, err := e.requireRole(swap, "alice", "BLAKE refund"); err != nil {
		return nil, err
	}

	if swap.State != state.BlakeFunded && swap.State != state.BTCRefunded {
		return nil, fmt.Errorf("refund-blake requires state BLAKE_FUNDED or BTC_REFUNDED")
	}

	if swap.State == state.BTCRefunded {
		if err := e.requireConfirmedBTCRefund(ctx, swap); err != nil {
			return nil, err
		}
	}

	recovered, err := e.requireRecoveredBranch(ctx, swap, chain.Blake, htlc.Refund)

	if err != nil {
		return nil, err
	}

	if recovered != nil {
		return e.finishRecovered(ctx, swap, chain.Blake, htlc.Refund, recovered, func(swap *model.Swap) error {
			return swap.Transition(state.BlakeRefunded)
		})
	}

	if err := contract.VerifyFunding(ctx, e.Blake, swap, chain.Blake); err != nil {
		return nil, err
	}

	derived, err := contract.Derive(ctx, e.Blake, swap, chain.Blake)

	if err != nil {
		return nil, err
	}

	return e.settleAndRecord(ctx, swap, chain.Blake, htlc.Refund, func(destination string) (*settle.Plan, error) {
		return settle.Refund(ctx, e.Blake, settle.Request{
			Chain:       chain.Blake,
			Funding:     chain.Outpoint{TxID: *swap.BlakeFundingTxID, Vout: int64(*swap.BlakeFundingVout)},
			Destination: destination,
			Descriptor:  derived.Descriptor,
			InputSats:   swap.BlakeAmountBaseUnits,
			Wallet:      e.BlakeWallet,
			LockTime:    swap.BlakeTimeout,
		}, e.settlementOptions())
	}, func(swap *model.Swap) error {
		return swap.Transition(state.BlakeRefunded)
	})
}

// Complete closes a swap whose BLAKE claim is confirmed and still active.
//
// The confirmation is re-proved rather than trusted: a swap is not complete
// because a record says so, it is complete because the chain still holds the claim
// that finished it.
func (e *Engine) Complete(ctx context.Context, swapID string) error {
	swap, err := e.load(ctx, swapID)

	if err != nil {
		return err
	}

	if swap.State != state.BlakeClaimed {
		return fmt.Errorf("complete requires state BLAKE_CLAIMED")
	}

	if err := e.requireActiveBlakeClaim(ctx, swap); err != nil {
		return err
	}

	if err := swap.Transition(state.Complete); err != nil {
		return err
	}

	return e.save(ctx, swap)
}

// finishRecovered records a settlement the chain already holds.
func (e *Engine) finishRecovered(
	ctx context.Context,
	swap *model.Swap,
	ch chain.Chain,
	branch htlc.Branch,
	recovered *recoveredBranch,
	advance func(*model.Swap) error,
) (*SettlementResult, error) {
	result := &SettlementResult{
		Chain:       ch,
		Branch:      branch,
		Pending:     recovered.Pending,
		Rebroadcast: recovered.Rebroadcast,
		State:       swap.State,
	}

	if recovered.Pending {
		setSettlement(swap, ch, branch, settlementTxID(swap, ch, branch), "", settlementHex(swap, ch, branch))

		result.TxID = settlementTxID(swap, ch, branch)

		return result, nil
	}

	setSettlement(swap, ch, branch, recovered.Spend.TxID, recovered.Spend.BlockHash, settlementHex(swap, ch, branch))

	if err := advance(swap); err != nil {
		return nil, err
	}

	if err := e.save(ctx, swap); err != nil {
		return nil, err
	}

	result.TxID = recovered.Spend.TxID
	result.BlockHash = recovered.Spend.BlockHash
	result.State = swap.State

	return result, nil
}

// requireConfirmedBTCRefund re-proves Bob's Bitcoin refund on the active chain.
func (e *Engine) requireConfirmedBTCRefund(ctx context.Context, swap *model.Swap) error {
	observation, err := observe.Observe(ctx, e.BTC, swap, chain.BTC, e.observeConfig(chain.BTC))

	if err != nil {
		return err
	}

	if observation.Status != observe.Spent || observation.Branch != htlc.Refund {
		return fmt.Errorf(
			"SECURITY FAILURE: BTC_REFUNDED is not confirmed on the active chain "+
				"(observed %s/%s); refusing to refund BLAKE",
			observation.Status, observation.Branch,
		)
	}

	return nil
}

// requireActiveBlakeClaim re-proves the final BLAKE claim on the active chain.
func (e *Engine) requireActiveBlakeClaim(ctx context.Context, swap *model.Swap) error {
	observation, err := observe.Observe(ctx, e.Blake, swap, chain.Blake, e.observeConfig(chain.Blake))

	if err != nil {
		return err
	}

	if observation.Status != observe.Spent || observation.Branch != htlc.Claim {
		return fmt.Errorf(
			"SECURITY FAILURE: the final BLAKE claim is not confirmed on the active "+
				"chain (observed %s/%s); refusing to complete",
			observation.Status, observation.Branch,
		)
	}

	recorded := settlementTxID(swap, chain.Blake, htlc.Claim)

	if recorded != "" && observation.SpendTxID != recorded {
		return fmt.Errorf(
			"SECURITY FAILURE: the confirmed BLAKE claim is %s, not the recorded %s",
			observation.SpendTxID, recorded,
		)
	}

	return nil
}
