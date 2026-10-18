package swap

import (
	"context"
	"fmt"

	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/fund"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/state"
	"github.com/gjbrr/blake-swap/pkg/timeout"
)

// TimeoutRefresh is what a timeout refresh changed.
type TimeoutRefresh struct {
	SwapID          string
	OldBTCTimeout   int64
	NewBTCTimeout   int64
	OldBlakeTimeout int64
	NewBlakeTimeout int64
	BTCHeight       int64
	BlakeHeight     int64
	// ClearedIntents names the funding intents an explicit clear removed.
	ClearedIntents []string
}

// RefreshTimeouts renews an unfunded offer's timeout plan.
//
// The window between creating an offer and having it accepted can be long enough that
// the computed deadlines are no longer a fair distance away. Refreshing recomputes them
// from the current chain tips — and changes nothing else: not the identity, not the
// secret, not the economics.
//
// It is refused after the counterparty accepts, because from then on the advertised
// deadlines are part of the contract they accepted. It is also refused if anything
// suggests funding may have started, including a leftover funding intent: an intent is
// written *before* money can move, so its existence means a crash-window funding action
// may already have been broadcast, and moving the deadline under it would be the worst
// possible time.
func (e *Engine) RefreshTimeouts(
	ctx context.Context, swapID string, clearStaleIntent bool,
) (*TimeoutRefresh, error) {
	swap, err := e.load(ctx, swapID)

	if err != nil {
		return nil, err
	}

	if _, err := e.requireRole(swap, "alice", "timeout refresh"); err != nil {
		return nil, err
	}

	if swap.State != state.Offered {
		return nil, fmt.Errorf("refresh-timeouts requires state OFFERED")
	}

	if swap.BlakeBobPub != nil || swap.BTCBobPub != nil || swap.NostrBobPub != nil {
		return nil, fmt.Errorf(
			"SECURITY FAILURE: cannot refresh timeouts after counterparty acceptance",
		)
	}

	for _, evidence := range []struct {
		name    string
		present bool
	}{
		{"blake_funding_txid", swap.BlakeFundingTxID != nil},
		{"blake_funding_vout", swap.BlakeFundingVout != nil},
		{"blake_funding_blockhash", swap.BlakeFundingBlockHash != nil},
		{"btc_funding_txid", swap.BTCFundingTxID != nil},
		{"btc_funding_vout", swap.BTCFundingVout != nil},
		{"btc_funding_blockhash", swap.BTCFundingBlockHash != nil},
	} {
		if evidence.present {
			return nil, fmt.Errorf(
				"SECURITY FAILURE: cannot refresh timeouts after funding evidence "+
					"exists: %s", evidence.name,
			)
		}
	}

	result := &TimeoutRefresh{SwapID: swap.SwapID}

	if err := e.settleIntents(ctx, swap, clearStaleIntent, result); err != nil {
		return nil, err
	}

	blakeHeight, err := e.Blake.Height(ctx)

	if err != nil {
		return nil, err
	}

	btcHeight, err := e.BTC.Height(ctx)

	if err != nil {
		return nil, err
	}

	plan, err := timeout.BuildPlan(btcHeight, blakeHeight, timeout.DefaultPolicy())

	if err != nil {
		return nil, err
	}

	if err := timeout.ValidatePlan(plan, timeout.DefaultPolicy()); err != nil {
		return nil, err
	}

	result.BTCHeight = btcHeight
	result.BlakeHeight = blakeHeight
	result.OldBTCTimeout = swap.BTCTimeout
	result.OldBlakeTimeout = swap.BlakeTimeout
	result.NewBTCTimeout = plan.BTCTimeoutHeight
	result.NewBlakeTimeout = plan.BlakeTimeoutHeight

	// Everything the refresh is *not* allowed to change is remembered here, so the
	// check after the update is about the record rather than about this code's intent.
	original := struct {
		swapID      string
		secretHex   *string
		secretHash  string
		blakeAmount int64
		btcSats     int64
	}{
		swapID: swap.SwapID, secretHex: swap.SecretHex, secretHash: swap.SecretHash,
		blakeAmount: swap.BlakeAmountBaseUnits, btcSats: swap.BTCSats,
	}

	version := "1.5"
	btcWindow := plan.BTCWindowSeconds
	blakeWindow := plan.BlakeWindowSeconds
	recovery := plan.BobRecoverySeconds

	swap.BTCTimeout = plan.BTCTimeoutHeight
	swap.BlakeTimeout = plan.BlakeTimeoutHeight
	swap.TimeoutPolicyVersion = &version
	swap.BTCTimeoutWindowSeconds = &btcWindow
	swap.BlakeTimeoutWindowSeconds = &blakeWindow
	swap.BobRecoverySeconds = &recovery

	// The anchors move with the plan: a confirmed funding cannot precede them, and the
	// scan they bound should start from the chain as it is now.
	swap.BTCScanStartHeight = &btcHeight
	swap.BlakeScanStartHeight = &blakeHeight

	if err := swap.Validate(); err != nil {
		return nil, err
	}

	switch {
	case swap.SwapID != original.swapID:
		return nil, fmt.Errorf("internal error: timeout refresh changed the swap id")
	case swap.SecretHash != original.secretHash:
		return nil, fmt.Errorf("internal error: timeout refresh changed the secret hash")
	case swap.BlakeAmountBaseUnits != original.blakeAmount:
		return nil, fmt.Errorf("internal error: timeout refresh changed the BLAKE amount")
	case swap.BTCSats != original.btcSats:
		return nil, fmt.Errorf("internal error: timeout refresh changed the BTC amount")
	}

	if original.secretHex != nil && (swap.SecretHex == nil || *swap.SecretHex != *original.secretHex) {
		return nil, fmt.Errorf("internal error: timeout refresh changed the secret")
	}

	if err := e.save(ctx, swap); err != nil {
		return nil, err
	}

	return result, nil
}

// settleIntents resolves leftover funding intents before a refresh.
//
// A confirmed intent resolves itself and is cleared by the resolver. Anything else is
// refused by default — an intent may mean a funding broadcast is in flight, and an
// orphaned one would otherwise block refresh forever. The explicit `clearStaleIntent`
// flag is the operator's way out, and it still refuses a *pending* intent: that one has
// a broadcast to wait for.
func (e *Engine) settleIntents(
	ctx context.Context, swap *model.Swap, clearStale bool, result *TimeoutRefresh,
) error {
	for _, ch := range []chain.Chain{chain.Blake, chain.BTC} {
		wallet := e.walletForSettlement(ch)

		resolution, err := fund.ResolveIntent(ctx, e.nodeFor(ch), swap, ch, wallet, e.Intents)

		if err != nil {
			return err
		}

		if resolution == fund.ResolutionNone || resolution == fund.ResolutionConfirmed {
			continue
		}

		if !clearStale {
			return fmt.Errorf(
				"SECURITY FAILURE: a %s funding intent exists (%s); a funding "+
					"broadcast may have started. Resolve it, or pass "+
					"--clear-stale-intent after verifying no funding was broadcast",
				chainLabel(ch), resolution,
			)
		}

		if resolution == fund.ResolutionPending {
			return fmt.Errorf(
				"SECURITY FAILURE: refusing to clear the %s funding intent: a funding "+
					"broadcast is still pending. Let it confirm or be replaced first.",
				chainLabel(ch),
			)
		}

		if err := fund.ClearIntent(ctx, e.Intents, swap.SwapID, ch); err != nil {
			return err
		}

		result.ClearedIntents = append(result.ClearedIntents, chainLabel(ch))
	}

	return nil
}
