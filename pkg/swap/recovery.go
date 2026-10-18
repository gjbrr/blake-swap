package swap

import (
	"context"
	"fmt"

	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/contract"
	"github.com/gjbrr/blake-swap/pkg/fund"
	"github.com/gjbrr/blake-swap/pkg/reconcile"
	"github.com/gjbrr/blake-swap/pkg/state"
)

// RecoveryPlan reports how to recover an HTLC whose keys this wallet no longer
// holds.
//
// A restore, rename or re-creation that loses the private keys behind an HTLC's
// addresses makes both the claim and the refund impossible, and nothing in the swap
// record says so. This report is read-only: it names the exact descriptor, its
// checksum, the funding outpoint, which branch and key belong to this participant,
// and whether any loaded wallet can currently spend it.
type RecoveryPlan struct {
	SwapID string
	State  state.State
	Role   string
	Legs   []RecoveryLeg
}

// RecoveryLeg is one chain's part of a recovery plan.
type RecoveryLeg struct {
	Chain chain.Chain
	// Descriptor is the node-compiled public descriptor, checksummed.
	Descriptor string
	// Address is the address it derives.
	Address string
	// Amount is the expected value in the chain's smallest unit.
	Amount int64
	// FundingTxID and FundingVout are the recorded outpoint, when known.
	FundingTxID string
	FundingVout *int32
	// Timeout is the contract's timelock height.
	Timeout int64
	// OurClaimKey and OurRefundKey are this participant's keys on each branch; one
	// of them is empty, because a participant only has one side of a swap.
	OurClaimKey  string
	OurRefundKey string
	// Solvable reports whether a loaded wallet can spend the output, when asked.
	Solvable *bool
	// SolvableWallet names the wallet that can, when one can.
	SolvableWallet string
	// Reason explains an unavailable leg.
	Reason string
}

// RecoveryPlan builds the recovery report for one swap.
func (e *Engine) RecoveryPlan(ctx context.Context, swapID string) (*RecoveryPlan, error) {
	swap, err := e.load(ctx, swapID)

	if err != nil {
		return nil, err
	}

	role, _, _, _, err := e.reconcileIdentity(swap)

	if err != nil {
		return nil, err
	}

	plan := &RecoveryPlan{SwapID: swap.SwapID, State: swap.State, Role: role}

	for _, ch := range []chain.Chain{chain.Blake, chain.BTC} {
		leg := RecoveryLeg{
			Chain:   ch,
			Timeout: swap.BlakeTimeout,
		}

		if ch == chain.BTC {
			leg.Timeout = swap.BTCTimeout
		}

		if txid, vout, err := contract.Outpoint(swap, ch); err == nil {
			leg.FundingTxID = txid
			index := int32(vout)
			leg.FundingVout = &index
		}

		derived, err := contract.Derive(ctx, e.nodeFor(ch), swap, ch)

		if err != nil {
			leg.Reason = err.Error()
			plan.Legs = append(plan.Legs, leg)

			continue
		}

		leg.Descriptor = derived.Descriptor
		leg.Address = derived.Address
		leg.Amount = derived.Amount

		// Which key is ours depends on the side of this swap, and the sides are
		// mirrored between the chains: on BLAKE the maker claims and the taker
		// refunds, on Bitcoin the reverse.
		if ch == chain.Blake {
			leg.OurClaimKey = deref(swap.BlakeBobPub)
			leg.OurRefundKey = deref(swap.BlakeAlicePub)
		} else {
			leg.OurClaimKey = deref(swap.BTCAlicePub)
			leg.OurRefundKey = deref(swap.BTCBobPub)
		}

		solvability := fund.CheckSolvability(ctx, e.nodeFor(ch), swap, ch, fund.Options{
			Wallet:      e.walletForSettlement(ch),
			WatchWallet: watchWallet(e, ch),
		})

		leg.Solvable = solvability.Solvable
		leg.SolvableWallet = solvability.Wallet

		if solvability.Reason != "" {
			leg.Reason = solvability.Reason
		}

		plan.Legs = append(plan.Legs, leg)
	}

	return plan, nil
}

// watchWallet is the watch-only wallet for one chain.
func watchWallet(e *Engine, ch chain.Chain) string {
	if ch == chain.Blake {
		return e.BlakeWatchWallet
	}

	return e.BTCWatchWallet
}

// deref reads an optional string field.
func deref(value *string) string {
	if value == nil {
		return ""
	}

	return *value
}

// DashboardEntry is one swap's line in the dashboard.
type DashboardEntry struct {
	SwapID string
	State  state.State
	Class  reconcile.Class
	Action reconcile.Action
	// Outcome is the operator-facing label for this swap.
	Outcome string
	// Reason explains the recommendation.
	Reason string
	// Failed records that the report could not be built.
	Failed bool
}

// Dashboard summarises every local swap.
//
// It asks the same question `resume` would ask for each swap, so the dashboard cannot
// disagree with what resuming would do — a dashboard that computes its own opinion is
// a second decision table, and the protocol only has one.
func (e *Engine) Dashboard(ctx context.Context) ([]DashboardEntry, error) {
	records, err := e.List(ctx)

	if err != nil {
		return nil, err
	}

	entries := make([]DashboardEntry, 0, len(records))

	for _, record := range records {
		entry := DashboardEntry{SwapID: record.SwapID, State: record.State}

		report, err := e.Reconcile(ctx, record.SwapID)

		if err != nil {
			entry.Failed = true
			entry.Outcome = "NEEDS ATTENTION"
			entry.Reason = "reconciliation failed: " + err.Error()

			entries = append(entries, entry)

			continue
		}

		entry.Class = report.DecisionClass
		entry.Action = report.NextAction
		entry.Reason = report.Reason
		entry.Outcome = outcomeLabel(record.State, report)

		entries = append(entries, entry)
	}

	return entries, nil
}

// outcomeLabel is the operator-facing summary of one swap.
func outcomeLabel(current state.State, report *reconcile.Report) string {
	switch report.DecisionClass {
	case reconcile.ClassSettled:
		switch current {
		case state.Complete:
			return "COMPLETED"
		case state.BTCRefunded, state.BlakeRefunded:
			return "REFUNDED"
		}

		return "SETTLED"
	case reconcile.ClassManualAttention:
		return "NEEDS ATTENTION"
	}

	switch report.NextAction {
	case reconcile.ActionWait:
		switch current {
		case state.BlakeFunded:
			return "WAITING FOR BTC"
		case state.BTCFunded:
			return "WAITING FOR ALICE"
		case state.BTCClaimed:
			return "WAITING FOR BOB"
		case state.BTCRefunded:
			return "WAITING FOR BLAKE REFUND"
		}

		return "WAITING"
	case reconcile.ActionSyncFunding, reconcile.ActionSyncSettlement, reconcile.ActionComplete:
		return "READY TO RESUME"
	case reconcile.ActionFundBTC:
		return "READY TO FUND BTC"
	case reconcile.ActionClaimBTC:
		return "READY TO CLAIM BTC"
	case reconcile.ActionClaimBlake:
		return "READY TO CLAIM BLAKE"
	case reconcile.ActionRefundBTC:
		return "READY TO REFUND BTC"
	case reconcile.ActionRefundBlake:
		return "READY TO REFUND BLAKE"
	case reconcile.ActionFundBlake:
		return "READY TO FUND BLAKE"
	}

	return fmt.Sprint(report.NextAction)
}

// ResumeAll resumes every swap that has an action, and reports what happened.
//
// It is the operator loop over the whole state tree. Swaps that are settled,
// waiting, or need an operator decision are reported rather than touched, so running
// it is safe on a mixed set.
func (e *Engine) ResumeAll(ctx context.Context) ([]*ResumeResult, error) {
	records, err := e.List(ctx)

	if err != nil {
		return nil, err
	}

	var results []*ResumeResult

	for _, record := range records {
		result, err := e.Resume(ctx, record.SwapID)

		if result != nil {
			results = append(results, result)
		}

		if err != nil {
			return results, fmt.Errorf("%s: %w", record.SwapID, err)
		}
	}

	return results, nil
}
