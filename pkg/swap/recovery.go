package swap

import (
	"context"
	"fmt"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/contract"
	"github.com/gjbrr/blake-swap/pkg/fund"
	"github.com/gjbrr/blake-swap/pkg/reconcile"
	"github.com/gjbrr/blake-swap/pkg/state"
	"github.com/gjbrr/blake-swap/pkg/store"
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
//
// It carries the facts a UI shows rather than only the label it shows them under: the
// browser and the terminal render the same document, and a client that had to re-derive
// the roles or the amounts from the record would be a second opinion about the protocol.
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

	// Role is the protocol role this participant holds in this swap.
	Role reconcile.Role
	// BlakeStatus and BTCStatus are the two chains' observations.
	BlakeStatus reconcile.Status
	BTCStatus   reconcile.Status
	// BlakeAmount and BTCSats are the agreed trade.
	BlakeAmount amount.Blake
	BTCSats     int64
	// TxIDs are the six settlement anchors, empty when unset.
	TxIDs map[string]string
}

// Unsolvable is one HTLC no loaded wallet can spend.
//
// It is the dashboard's most important line: a swap whose contract cannot be solved
// by anything on this machine is a swap that needs a recovery plan before anything
// else, and it is invisible in the per-swap state.
type Unsolvable struct {
	SwapID string
	Chain  string
	Reason string
}

// Dashboard is the whole summary, classified the way the dashboard displays it.
type Dashboard struct {
	// Unsolvable lists the HTLCs no loaded wallet can spend.
	Unsolvable []Unsolvable
	Active     []DashboardEntry
	Attention  []DashboardEntry
	Settled    []DashboardEntry
	Archived   []DashboardEntry
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
		entry.Role = report.ProtocolRole
		entry.BlakeStatus = report.Blake.Status
		entry.BTCStatus = report.BTC.Status
		entry.BlakeAmount = record.BlakeAmount()
		entry.BTCSats = record.BTCSats
		entry.TxIDs = map[string]string{
			"blake_funding_txid": deref(record.BlakeFundingTxID),
			"btc_funding_txid":   deref(record.BTCFundingTxID),
			"btc_claim_txid":     deref(record.BTCCLaimTxID),
			"blake_claim_txid":   deref(record.BlakeClaimTxID),
			"btc_refund_txid":    deref(record.BTCRefundTxID),
			"blake_refund_txid":  deref(record.BlakeRefundTxID),
		}

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

// Classify sorts dashboard entries into the view's three live groups.
//
// The classification is the decision class, not the state: a swap whose reconciliation says
// it needs a person does not become active because its record still says BTC_FUNDED, and a
// settled swap does not become active because it is not COMPLETE.
func Classify(entries []DashboardEntry) Dashboard {
	summary := Dashboard{
		Active:    make([]DashboardEntry, 0, len(entries)),
		Attention: make([]DashboardEntry, 0, 2),
		Settled:   make([]DashboardEntry, 0, 2),
	}

	for _, entry := range entries {
		switch {
		case entry.Failed || entry.Class == reconcile.ClassManualAttention:
			summary.Attention = append(summary.Attention, entry)
		case entry.Class == reconcile.ClassSettled:
			summary.Settled = append(summary.Settled, entry)
		default:
			summary.Active = append(summary.Active, entry)
		}
	}

	return summary
}

// UnsolvableHTLCs lists the funded HTLCs no loaded wallet can spend.
//
// It asks the recovery plan rather than the observation, because "can anything here
// spend it" is a wallet question: a funded HTLC whose keys are gone looks exactly
// like a funded one whose keys are here, until someone tries.
func (e *Engine) UnsolvableHTLCs(ctx context.Context) []Unsolvable {
	records, err := e.List(ctx)

	if err != nil {
		return nil
	}

	var found []Unsolvable

	for _, record := range records {
		plan, err := e.RecoveryPlan(ctx, record.SwapID)

		if err != nil {
			continue
		}

		for _, leg := range plan.Legs {
			if leg.Solvable == nil || *leg.Solvable {
				continue
			}

			found = append(found, Unsolvable{
				SwapID: record.SwapID,
				Chain:  chainSlug(leg.Chain),
				Reason: "no loaded wallet can solve this HTLC; import the descriptor " +
					"with `watch-htlc` (see docs/KEY_RECOVERY.md)",
			})
		}
	}

	return found
}

// Archive lists the retired records the dashboard shows.
//
// A store that cannot enumerate its archive reports none rather than failing: the dashboard
// is a summary, and an older store is not a reason to refuse to show the live swaps.
func (e *Engine) Archive(ctx context.Context) []DashboardEntry {
	reader, ok := e.Store.(store.ArchiveReader)

	if !ok {
		return nil
	}

	records, err := reader.Archived(ctx)

	if err != nil {
		return nil
	}

	entries := make([]DashboardEntry, 0, len(records))

	for _, record := range records {
		entries = append(entries, DashboardEntry{
			SwapID:      record.SwapID,
			State:       record.State,
			BlakeAmount: record.BlakeAmount(),
			BTCSats:     record.BTCSats,
		})
	}

	return entries
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
