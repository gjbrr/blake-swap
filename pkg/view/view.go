// Package view builds the structured results the page renders.
//
// It is the one place that turns engine results into a form a document can display, so
// the page never parses CLI text and the worker never formats money. Every amount here
// is produced by pkg/amount or pkg/decimal and carried as a string: a JSON number would
// reach the browser as a float, and a float is not an amount.
//
// The constructors take engine types rather than primitives because the mapping is the
// interesting part: which field means what is protocol knowledge, and it belongs next to
// the protocol rather than in a renderer.
package view

import (
	"strconv"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/proto"
	"github.com/gjbrr/blake-swap/pkg/reconcile"
	"github.com/gjbrr/blake-swap/pkg/state"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// sats renders a satoshi count exactly.
//
// It is a string because the browser must never do arithmetic on it, and because a
// JS number cannot hold every int64 exactly.
func sats(value int64) string { return strconv.FormatInt(value, 10) }

// blake renders a BLAKE amount exactly.
func blake(value int64) string { return amount.Blake(value).Format() }

// tx renders a recorded transaction.
func tx(txid, blockHash *string) proto.TxView {
	view := proto.TxView{}

	if txid != nil {
		view.TxID = *txid
	}

	if blockHash != nil {
		view.BlockHash = *blockHash
	}

	view.Confirmed = view.BlockHash != ""

	return view
}

// deref reads an optional string, empty when unset.
func deref(value *string) string {
	if value == nil {
		return ""
	}

	return *value
}

// Timeline is the protocol's step list for one checkpoint.
//
// A step is reached when the current state can be reached from it, which is the same
// question as "did the protocol pass through here on the branch this swap took". The
// refund branch is marked rather than hidden: an aborted swap genuinely took it, and a
// timeline that drew only the happy path would leave an operator looking for the step
// they are on.
func Timeline(current state.State) []proto.TimelineStep {
	reached := map[state.State]bool{}

	// Everything reachable from OFFERED, walked forward from the current state's
	// ancestors. The graph is small and fixed, so a plain BFS is exact.
	queue := []state.State{state.Offered}

	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]

		if reached[node] {
			continue
		}

		reached[node] = true

		queue = append(queue, node.NextStates()...)
	}

	// Which entries are ancestors of the current state, not merely reachable from the
	// start: a swap at BTC_FUNDED never visited BLAKE_CLAIMED.
	ancestor := map[state.State]bool{}

	var walk func(state.State)

	walk = func(target state.State) {
		if target == current {
			ancestor[target] = true
		}

		for _, candidate := range state.All {
			for _, next := range candidate.NextStates() {
				if next != target {
					continue
				}

				if ancestor[candidate] {
					continue
				}

				ancestor[candidate] = true

				walk(candidate)
			}
		}
	}

	walk(current)

	ancestor[current] = true

	var steps []proto.TimelineStep

	for _, candidate := range orderedStates {
		if !reached[candidate] {
			continue
		}

		// A refund-branch state is only part of the timeline when the swap took it.
		// Otherwise every aborted swap's timeline would list the other branch's steps
		// as "not done" forever.
		if isRefund(candidate) && !ancestor[candidate] && current != candidate {
			continue
		}

		steps = append(steps, proto.TimelineStep{
			State:   string(candidate),
			Reached: ancestor[candidate],
			Current: candidate == current,
			Refund:  isRefund(candidate),
		})
	}

	return steps
}

// orderedStates is the timeline's display order: the happy path, then the abort branch.
var orderedStates = []state.State{
	state.Offered,
	state.BlakeFunded,
	state.BTCFunded,
	state.SecretRevealed,
	state.BTCClaimed,
	state.BlakeClaimed,
	state.Complete,
	state.BTCRefunded,
	state.BlakeRefunded,
}

// isRefund reports whether a checkpoint belongs to the abort branch.
func isRefund(candidate state.State) bool {
	return candidate == state.BTCRefunded || candidate == state.BlakeRefunded
}

// RoleOf derives this participant's protocol role from the record's identity pins.
//
// The person using the client does not choose a role and must not be asked to: which
// side of the swap they are is a fact about who signed the offer, and a UI that let
// somebody pick the wrong one would offer them actions the engine then refuses. The
// pins are the authority; an unpinned record falls back to the store's default, which
// is Alice's because a fresh record is created by Alice.
func RoleOf(record *model.Swap, me string) string {
	if record == nil || me == "" {
		return string(reconcile.Alice)
	}

	alice := deref(record.NostrAlicePub) == me
	bob := deref(record.NostrBobPub) == me

	switch {
	case alice && !bob:
		return string(reconcile.Alice)
	case bob && !alice:
		return string(reconcile.Bob)
	case alice && bob:
		// Both pins naming one key means the record is ambiguous, not that this
		// participant is both sides. Reporting neither is the honest answer, and the
		// engine refuses every role-gated action on such a record anyway.
		return ""
	default:
		return string(reconcile.Alice)
	}
}

// Summary builds one swap's view.
//
// report may be nil: a record that cannot be reconciled still has terms and transaction
// ids, and showing them is better than showing nothing. What a nil report costs is the
// next action, which is exactly the field that must not be invented.
func Summary(record *model.Swap, role string, report *reconcile.Report) proto.SwapSummary {
	if record == nil {
		return proto.SwapSummary{}
	}

	summary := proto.SwapSummary{
		SwapID:       record.SwapID,
		State:        string(record.State),
		Role:         role,
		BlakeAmount:  blake(record.BlakeAmountBaseUnits),
		BTCSats:      sats(record.BTCSats),
		SecretHash:   record.SecretHash,
		CreatedAt:    record.CreatedAt,
		BTCTimeout:   record.BTCTimeout,
		BlakeTimeout: record.BlakeTimeout,
		Timeline:     Timeline(record.State),
		Funding: proto.FundingView{
			BlakeTxID: deref(record.BlakeFundingTxID),
			BTCTxID:   deref(record.BTCFundingTxID),
		},
		Settlement: proto.SettlementView{
			BTCClaim:        tx(record.BTCCLaimTxID, record.BTCCLaimBlockHash),
			BTCRefund:       tx(record.BTCRefundTxID, record.BTCRefundBlockHash),
			BlakeClaim:      tx(record.BlakeClaimTxID, record.BlakeClaimBlockHash),
			BlakeRefund:     tx(record.BlakeRefundTxID, record.BlakeRefundBlockHash),
			ReorgClaimTxID:  deref(record.BlakeReorgClaimTxID),
			ReorgRefundTxID: deref(record.BlakeReorgRefundTxID),
		},
	}

	if record.BlakeFundingVout != nil {
		summary.Funding.BlakeVout = *record.BlakeFundingVout
	}

	if record.BTCFundingVout != nil {
		summary.Funding.BTCVout = *record.BTCFundingVout
	}

	summary.Counterparty = counterpartyOf(record, role)

	if report != nil {
		summary.NextAction = string(report.NextAction)
		summary.DecisionClass = string(report.DecisionClass)
		summary.Reason = report.Reason
		summary.Negotiation = report.NegotiationStatus
		summary.Chains = proto.ChainView{
			Blake: observation(report.Blake, report.Heights.Blake),
			BTC:   observation(report.BTC, report.Heights.BTC),
		}
	}

	return summary
}

// counterpartyOf is the other participant's Nostr key, given this one's role.
func counterpartyOf(record *model.Swap, role string) string {
	if role == string(reconcile.Alice) {
		return deref(record.NostrBobPub)
	}

	return deref(record.NostrAlicePub)
}

// observation renders one chain's observation.
func observation(observed reconcile.Observation, height int64) proto.ObservationView {
	branch := ""

	if observed.Branch != reconcile.BranchNone {
		branch = string(observed.Branch)
	}

	return proto.ObservationView{
		Status: string(observed.Status),
		Branch: branch,
		TxID:   observed.TxID,
		Height: height,
	}
}

// Dashboard builds the Swaps screen from the engine's classification.
//
// The classification itself is the engine's: which group a swap belongs in is a
// protocol decision, and a UI that re-derived it from the state string would be a second
// opinion nobody asked for.
func Dashboard(dashboard swap.Dashboard, archived []swap.DashboardEntry, unsolvable []swap.Unsolvable, heights proto.HeightView) proto.DashboardView {
	return proto.DashboardView{
		Active:     rows(dashboard.Active),
		Attention:  rows(dashboard.Attention),
		Settled:    rows(dashboard.Settled),
		Archived:   rows(archived),
		Heights:    heights,
		Unsolvable: unsolvableViews(unsolvable),
		Failed:     failedRows(dashboard),
	}
}

// rows renders a dashboard group.
func rows(entries []swap.DashboardEntry) []proto.DashboardRow {
	if len(entries) == 0 {
		return nil
	}

	out := make([]proto.DashboardRow, 0, len(entries))

	for _, entry := range entries {
		out = append(out, row(entry))
	}

	return out
}

// failedRows collects the entries whose report could not be built.
//
// They are in the engine's own list rather than a group of their own because a record
// that cannot be reconciled belongs to no group: it is not active, not settled, and
// calling it "attention" would claim a decision was made about it.
func failedRows(dashboard swap.Dashboard) []proto.DashboardRow {
	var out []proto.DashboardRow

	for _, group := range [][]swap.DashboardEntry{dashboard.Active, dashboard.Attention, dashboard.Settled} {
		for _, entry := range group {
			if entry.Failed {
				out = append(out, row(entry))
			}
		}
	}

	return out
}

// row renders one dashboard entry.
func row(entry swap.DashboardEntry) proto.DashboardRow {
	return proto.DashboardRow{
		SwapID:        entry.SwapID,
		State:         string(entry.State),
		Role:          string(entry.Role),
		Outcome:       entry.Outcome,
		NextAction:    string(entry.Action),
		DecisionClass: string(entry.Class),
		Reason:        entry.Reason,
		BlakeAmount:   entry.BlakeAmount.Format(),
		BTCSats:       sats(entry.BTCSats),
		BlakeStatus:   string(entry.BlakeStatus),
		BTCStatus:     string(entry.BTCStatus),
		TxIDs:         entry.TxIDs,
	}
}

// unsolvableViews renders the HTLCs no loaded wallet can spend.
func unsolvableViews(items []swap.Unsolvable) []proto.UnsolvableView {
	if len(items) == 0 {
		return nil
	}

	out := make([]proto.UnsolvableView, 0, len(items))

	for _, item := range items {
		out = append(out, proto.UnsolvableView{
			SwapID: item.SwapID, Chain: item.Chain, Reason: item.Reason,
		})
	}

	return out
}

// Action builds the view of a command that changed something.
func Action(command, swapID, kind string) proto.ActionView {
	return proto.ActionView{Command: command, SwapID: swapID, Kind: kind}
}

// WithoutTransaction marks an action that produced no transaction.
func WithoutTransaction(action proto.ActionView) proto.ActionView { return action }

// Settlement renders a claim or refund result.
func Settlement(action proto.ActionView, result *swap.SettlementResult) proto.ActionView {
	if result == nil {
		return action
	}

	action.TxID = result.TxID
	action.BlockHash = result.BlockHash
	action.Confirmed = !result.Pending && result.BlockHash != ""
	action.Pending = result.Pending
	action.Recovered = result.Recovered
	action.State = string(result.State)
	action.FeeSats = sats(result.FeeSats)
	action.Vsize = result.Vsize
	action.FeeRate = result.FeeRate.String()
	action.FeeSource = result.FeeSource

	return action
}

// Funding renders a funding result.
//
// The fee of a funding transaction is not reported by the engine, so there is no field
// here for one: a zero would look like a measured zero.
func Funding(action proto.ActionView, result *swap.FundingResult) proto.ActionView {
	if result == nil {
		return action
	}

	action.TxID = result.TxID
	action.BlockHash = result.BlockHash
	action.Confirmed = !result.Pending && result.BlockHash != ""
	action.Pending = result.Pending
	action.Recovered = result.Recovered
	action.State = string(result.State)
	action.Message = result.Note
	action.Details = []proto.ActionFact{
		{Label: "chain", Value: string(result.Chain)},
		{Label: "outpoint", Value: result.TxID + ":" + strconv.FormatInt(result.Vout, 10)},
		{Label: "address", Value: result.Address},
		{Label: "fee target", Value: strconv.FormatInt(result.FeeTargetBlocks, 10) + " block(s)"},
	}

	if result.FeeRate.String() != "" && result.FeeRate.String() != "0" {
		action.FeeRate = result.FeeRate.String()
		action.FeeSource = result.FeeSource
	}

	return action
}

// Message renders a published or received coordination message.
func Message(action proto.ActionView, result *swap.MessageResult) proto.ActionView {
	if result == nil {
		return action
	}

	action.Message = result.Location
	action.Sender = result.Sender
	action.State = string(result.State)
	action.Kind = result.Kind

	return action
}

// Announcement renders a claim or refund announcement.
func Announcement(action proto.ActionView, result *swap.Announcement) proto.ActionView {
	if result == nil {
		return action
	}

	action.Message = result.Location
	action.TxID = result.TxID

	return action
}

// Sync renders a funding or settlement sync.
func Sync(action proto.ActionView, result *swap.SyncResult) proto.ActionView {
	if result == nil {
		return action
	}

	action.SwapID = result.SwapID
	action.Start = string(result.Start)
	action.End = string(result.End)

	if len(result.Recovered) > 0 {
		action.Details = []proto.ActionFact{
			{Label: "recovered", Value: join(result.Recovered)},
		}
	}

	return action
}

// Resume renders one resume result.
func Resume(action proto.ActionView, result *swap.ResumeResult) proto.ActionView {
	if result == nil {
		return action
	}

	action.SwapID = result.SwapID
	action.Kind = string(result.Action)
	action.DecisionClass = string(result.Class)
	action.Reason = result.Reason
	action.Performed = result.Performed
	action.TxID = result.TxID
	action.State = string(result.State)
	action.Confirmed = result.TxID != ""

	return action
}

// Accept renders an accepted offer.
func Accept(action proto.ActionView, result *swap.AcceptResult) proto.ActionView {
	if result == nil {
		return action
	}

	if result.Swap != nil {
		action.SwapID = result.Swap.SwapID
	}

	action.Message = result.Location
	action.State = string(result.State)

	if result.Timeout != nil {
		action.Details = []proto.ActionFact{
			{Label: "BTC remaining safety", Value: hours(result.Timeout.BTCRemainingSeconds)},
			{Label: "cross-chain recovery gap", Value: hours(result.Timeout.ActualRecoveryGap)},
			{Label: "Bob required recovery", Value: hours(result.Timeout.RequiredBTCWindow)},
		}
	}

	return action
}

// NewSwap renders a created swap.
func NewSwap(action proto.ActionView, result *swap.NewResult) proto.ActionView {
	if result == nil {
		return action
	}

	if result.Swap != nil {
		action.SwapID = result.Swap.SwapID
		action.State = string(result.Swap.State)
	}

	return action
}

// Market builds the Market screen from the evidence the domain read.
//
// now is the clock the age and expiry fields are measured against; the caller passes the
// same instant it used to derive the book, so an offer cannot be described as open here
// and expired there.
func Market(
	read string,
	relays []string,
	open, mine []marketflow.Entry,
	takes []marketflow.TakeEntry,
	matches []marketflow.MatchEntry,
	selections []proto.MarketSelection,
	me string,
) proto.MarketView {
	market := proto.MarketView{
		Read:       read,
		Relays:     relays,
		Open:       offers(open, me),
		Mine:       offers(mine, me),
		Takes:      takeViews(takes),
		Matches:    matchViews(matches, me),
		Selections: selections,
	}

	return market
}

// offers renders a set of offers.
func offers(entries []marketflow.Entry, me string) []proto.MarketOffer {
	if len(entries) == 0 {
		return nil
	}

	out := make([]proto.MarketOffer, 0, len(entries))

	for _, entry := range entries {
		out = append(out, proto.MarketOffer{
			EventID:     entry.EventID(),
			Maker:       entry.Offer.Maker,
			Direction:   string(entry.Offer.Direction),
			BlakeAmount: entry.Offer.BlakeAmount.Format(),
			BTCSats:     sats(entry.Offer.BTCSats),
			CreatedAt:   entry.Offer.CreatedAt,
			ExpiresAt:   entry.Offer.ExpiresAt,
			State:       string(entry.State),
			Mine:        me != "" && entry.Offer.Maker == me,
		})
	}

	return out
}

// takeViews renders takes.
func takeViews(takes []marketflow.TakeEntry) []proto.MarketTake {
	if len(takes) == 0 {
		return nil
	}

	out := make([]proto.MarketTake, 0, len(takes))

	for _, take := range takes {
		out = append(out, proto.MarketTake{
			EventID:     take.EventID,
			OfferID:     take.Take.OfferID,
			Taker:       take.Taker,
			Direction:   string(take.Take.Direction),
			MakerRole:   take.Take.MakerRole,
			TakerRole:   take.Take.TakerRole,
			BlakeAmount: take.Take.BlakeAmount.Format(),
			BTCSats:     sats(take.Take.BTCSats),
			CreatedAt:   take.Take.CreatedAt,
			Reverse:     take.Take.Direction == market.BTCForBlake,
		})
	}

	return out
}

// matchViews renders matches.
//
// It is handed the participant's identity because a match is not the same fact on both
// sides: the taker imports a swap, and in the reverse direction the maker still has to
// import the taker's protocol offer. Which side this participant is on is what decides
// which of those the page offers.
func matchViews(matches []marketflow.MatchEntry, me string) []proto.MarketMatch {
	if len(matches) == 0 {
		return nil
	}

	out := make([]proto.MarketMatch, 0, len(matches))

	for _, match := range matches {
		mine := me != "" && match.Match.MakerPubkey == me
		reverse := match.Match.Direction == market.BTCForBlake

		out = append(out, proto.MarketMatch{
			EventID:     match.Match.EventID,
			OfferID:     match.Match.OfferID,
			TakeID:      match.Match.TakeID,
			SwapID:      match.Match.SwapID,
			Maker:       match.Match.MakerPubkey,
			Taker:       match.Match.Taker,
			Direction:   string(match.Match.Direction),
			MakerRole:   match.Match.MakerRole,
			TakerRole:   match.Match.TakerRole,
			BlakeAmount: match.Match.BlakeAmount.Format(),
			BTCSats:     sats(match.Match.BTCSats),
			CreatedAt:   match.Match.CreatedAt,
			Reverse:     reverse,
			Mine:        mine,
			AwaitsOffer: reverse && mine,
		})
	}

	return out
}

// hours renders a duration in hours to two decimals, as the reference does.
//
// It is integer arithmetic, not a float division: the value is a whole number of
// seconds and the rendered figure must not depend on how a platform rounds a binary
// fraction. Hundredths are rounded half away from zero, which is what a person means by
// "to two decimals".
func hours(seconds int64) string {
	negative := seconds < 0

	if negative {
		seconds = -seconds
	}

	hundredths := (seconds*100 + 1800) / 3600

	whole := hundredths / 100
	part := hundredths % 100

	out := strconv.FormatInt(whole, 10) + "." + pad2(part)

	if negative {
		out = "-" + out
	}

	return out
}

// pad2 renders 0..99 as two digits.
func pad2(value int64) string {
	if value < 10 {
		return "0" + strconv.FormatInt(value, 10)
	}

	return strconv.FormatInt(value, 10)
}

// join renders a string list for a details row.
func join(values []string) string {
	out := ""

	for index, value := range values {
		if index > 0 {
			out += ", "
		}

		out += value
	}

	return out
}
