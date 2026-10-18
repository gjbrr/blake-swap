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
	"github.com/gjbrr/blake-swap/pkg/reconcile"
	"github.com/gjbrr/blake-swap/pkg/state"
)

// Reconcile reports the protocol truth about one swap, read-only.
//
// It is the command an operator (or the browser dashboard) runs when they want to
// know what should happen next, and it deliberately performs no economic action: the
// only writes it makes are the ones observation itself cannot avoid, and none of
// those are protocol state.
func (e *Engine) Reconcile(ctx context.Context, swapID string) (*reconcile.Report, error) {
	swap, err := e.load(ctx, swapID)

	if err != nil {
		return nil, err
	}

	role, me, peer, reportRole, err := e.reconcileIdentity(swap)

	if err != nil {
		return nil, err
	}

	heights, blake, btc, err := e.observeBoth(ctx, swap)

	if err != nil {
		return nil, err
	}

	//
	// The wallet facts only matter for a funding action, so they are read after the
	// decision names one. Asking for both wallets up front would fail a report about
	// a swap that needs no wallet at all.
	//
	decision := reconcile.Decide(swap, reportRole, blake, btc, heights)

	wallet, err := e.walletFacts(ctx, swap, decision.Action)

	if err != nil {
		return nil, err
	}

	report := reconcile.Build(reconcile.Input{
		Swap:         swap,
		Role:         reportRole,
		Me:           me,
		Counterparty: peer,
		Heights:      heights,
		Blake:        blake,
		BTC:          btc,
		Wallet:       wallet,
		Config:       reconcile.DefaultReadinessConfig(),
	})

	_ = role

	return &report, nil
}

// Next reports the one action the engine recommends right now.
func (e *Engine) Next(ctx context.Context, swapID string) (reconcile.Action, reconcile.Class, string, error) {
	report, err := e.Reconcile(ctx, swapID)

	if err != nil {
		return "", "", "", err
	}

	return report.NextAction, report.DecisionClass, report.Reason, nil
}

// Resume performs whatever action reconciliation recommends, once.
//
// It is the operator loop in one call: observe, decide, act. Every action it takes
// goes through the same command the CLI exposes, so resuming is never a different
// code path with its own rules — it is the commands, chosen by the decision table.
//
// Waiting and settled actions are not failures; they are the correct answer, and are
// returned as such.
func (e *Engine) Resume(ctx context.Context, swapID string) (*ResumeResult, error) {
	report, err := e.Reconcile(ctx, swapID)

	if err != nil {
		return nil, err
	}

	result := &ResumeResult{
		SwapID:    swapID,
		Action:    report.NextAction,
		Class:     report.DecisionClass,
		Reason:    report.Reason,
		State:     report.CheckpointState,
		Performed: false,
	}

	switch report.NextAction {
	case reconcile.ActionNone, reconcile.ActionWait, reconcile.ActionManualAttention:
		return result, nil
	case reconcile.ActionFundBlake:
		funded, err := e.FundBlake(ctx, swapID)

		if err != nil {
			return result, err
		}

		result.Performed = true
		result.TxID = funded.TxID
		result.State = funded.State

		return result, nil
	case reconcile.ActionFundBTC:
		funded, err := e.FundBTC(ctx, swapID)

		if err != nil {
			return result, err
		}

		result.Performed = true
		result.TxID = funded.TxID
		result.State = funded.State

		return result, nil
	case reconcile.ActionClaimBTC:
		settled, err := e.ClaimBTC(ctx, swapID)

		if err != nil {
			return result, err
		}

		result.Performed = true
		result.TxID = settled.TxID
		result.State = settled.State

		return result, nil
	case reconcile.ActionClaimBlake:
		settled, err := e.ClaimBlake(ctx, swapID)

		if err != nil {
			return result, err
		}

		result.Performed = true
		result.TxID = settled.TxID
		result.State = settled.State

		return result, nil
	case reconcile.ActionRefundBTC:
		settled, err := e.RefundBTC(ctx, swapID)

		if err != nil {
			return result, err
		}

		result.Performed = true
		result.TxID = settled.TxID
		result.State = settled.State

		return result, nil
	case reconcile.ActionRefundBlake:
		settled, err := e.RefundBlake(ctx, swapID)

		if err != nil {
			return result, err
		}

		result.Performed = true
		result.TxID = settled.TxID
		result.State = settled.State

		return result, nil
	case reconcile.ActionComplete:
		if err := e.Complete(ctx, swapID); err != nil {
			return result, err
		}

		result.Performed = true
		result.State = stateOf(report)

		return result, nil
	}

	return result, fmt.Errorf(
		"no automatic action for %q; this swap needs an operator decision",
		report.NextAction,
	)
}

// ResumeResult is what a resume did.
type ResumeResult struct {
	SwapID    string
	Action    reconcile.Action
	Class     reconcile.Class
	Reason    string
	Performed bool
	TxID      string
	State     state.State
}

// WatchHTLC records this swap's HTLC in the watch-only wallet.
//
// It exists for the case the watch-only import exists for: the signing wallet is
// gone, and the descriptor is the only way to see — and later spend — the contract.
func (e *Engine) WatchHTLC(ctx context.Context, swapID string) (*fund.WatchStatus, error) {
	swap, err := e.load(ctx, swapID)

	if err != nil {
		return nil, err
	}

	role, _, _, _, err := e.reconcileIdentity(swap)

	if err != nil {
		return nil, err
	}

	var (
		statuses []fund.WatchStatus
	)

	for _, ch := range []chain.Chain{chain.Blake, chain.BTC} {
		node := e.nodeFor(ch)

		derived, err := contract.Derive(ctx, e.nodeFor(ch), swap, ch)

		if err != nil {
			// A leg whose keys are not pinned yet cannot have an HTLC; that is a
			// fact about the swap, not a failure of the command.
			continue
		}

		wallet := e.BlakeWatchWallet

		if ch == chain.BTC {
			wallet = e.BTCWatchWallet
		}

		status := fund.ImportWatchOnly(ctx, node, swap, ch, derived, wallet)
		statuses = append(statuses, status)
	}

	if len(statuses) == 0 {
		return nil, fmt.Errorf(
			"no HTLC can be derived yet: the swap has no contract keys for either chain (role %s)",
			role,
		)
	}

	// The caller wants one answer; report the leg that could not be watched if any,
	// because that is the one that needs attention.
	for i := range statuses {
		if !statuses[i].Imported {
			return &statuses[i], nil
		}
	}

	return &statuses[0], nil
}

// reconcileIdentity derives the report's identity facts.
func (e *Engine) reconcileIdentity(swap *model.Swap) (string, string, string, reconcile.Role, error) {
	if e.Identity == nil {
		return "", "", "", "", fmt.Errorf("no local Nostr identity is configured")
	}

	me := e.Identity.PublicKey()

	role, peer, err := LocalRole(swap, me)

	if err != nil {
		return "", "", "", "", err
	}

	reportRole := reconcile.Alice

	if role == "bob" {
		reportRole = reconcile.Bob
	}

	return role, me, peer, reportRole, nil
}

// observeBoth observes both legs.
func (e *Engine) observeBoth(
	ctx context.Context, swap *model.Swap,
) (reconcile.Heights, reconcile.Observation, reconcile.Observation, error) {
	heights := reconcile.Heights{}

	if e.Blake != nil {
		height, err := e.Blake.Height(ctx)

		if err != nil {
			return heights, reconcile.Observation{}, reconcile.Observation{}, err
		}

		heights.Blake = height
	}

	if e.BTC != nil {
		height, err := e.BTC.Height(ctx)

		if err != nil {
			return heights, reconcile.Observation{}, reconcile.Observation{}, err
		}

		heights.BTC = height
	}

	blake, err := e.observeLeg(ctx, swap, chain.Blake)

	if err != nil {
		return heights, reconcile.Observation{}, reconcile.Observation{}, err
	}

	btc, err := e.observeLeg(ctx, swap, chain.BTC)

	if err != nil {
		return heights, reconcile.Observation{}, reconcile.Observation{}, err
	}

	return heights, blake, btc, nil
}

// observeLeg observes one leg and converts it for the decision table.
func (e *Engine) observeLeg(
	ctx context.Context, swap *model.Swap, ch chain.Chain,
) (reconcile.Observation, error) {
	node := e.nodeFor(ch)

	if node == nil {
		return reconcile.Observation{Status: reconcile.Unknown, Reason: "no node configured"}, nil
	}

	observation, err := observe.Observe(ctx, node, swap, ch, e.observeConfig(ch))

	if err != nil {
		return reconcile.Observation{
			Status: reconcile.Unknown, Reason: err.Error(),
		}, nil
	}

	return convertObservation(observation), nil
}

// convertObservation maps the observation vocabulary onto the decision table's.
func convertObservation(observation *observe.Observation) reconcile.Observation {
	converted := reconcile.Observation{
		Status:        reconcile.Status(observation.Status),
		TxID:          observation.TxID,
		Vout:          observation.Vout,
		BlockHash:     observation.BlockHash,
		Confirmations: observation.Confirmations,
		Reason:        observation.Reason,
	}

	switch observation.Branch {
	case htlc.Claim:
		converted.Branch = reconcile.BranchClaim
	case htlc.Refund:
		converted.Branch = reconcile.BranchRefund
	}

	return converted
}

// walletFacts reads the wallet a funding action would spend from.
func (e *Engine) walletFacts(
	ctx context.Context, swap *model.Swap, action reconcile.Action,
) (reconcile.WalletFacts, error) {
	facts := reconcile.WalletFacts{}

	wallet := ""

	switch action {
	case reconcile.ActionFundBlake:
		wallet = e.BlakeWallet
	case reconcile.ActionFundBTC:
		wallet = e.BTCWallet
	default:
		return facts, nil
	}

	node := e.nodeFor(chain.Blake)

	if action == reconcile.ActionFundBTC {
		node = e.nodeFor(chain.BTC)
	}

	if node == nil {
		facts.Error = "no node configured"

		return facts, nil
	}

	facts.Available = true
	facts.Loaded = true

	balances, err := node.Bind(wallet).Balances(ctx)

	if err != nil {
		facts.Available = false
		facts.Error = err.Error()

		return facts, nil
	}

	trusted, err := balances.TrustedAmount()

	if err != nil {
		facts.Error = err.Error()

		return facts, nil
	}

	facts.Trusted = &trusted

	return facts, nil
}

// stateOf is the report's checkpoint state.
func stateOf(report *reconcile.Report) state.State { return report.CheckpointState }
