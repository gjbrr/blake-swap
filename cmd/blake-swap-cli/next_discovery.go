package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gjbrr/blake-swap/pkg/coord"
	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/reconcile"
	"github.com/gjbrr/blake-swap/pkg/state"
	"github.com/gjbrr/blake-swap/pkg/store"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// The argument-less `next` is a different command from `next SWAP_ID`, which reports one
// swap's recommendation and stays available as an extension. This one is the reference's
// discovery pass: it scans the whole local state tree and the configured relays, finds the
// ONE unambiguous non-economic coordination step, and performs it.
//
// The reference is `command_next` in src/swap_v2.py. Its order of collection is part of
// the contract, because it decides which step a converging loop reaches first:
//
//   a. take an open offer from another maker that this identity has not already taken;
//   b. import an authenticated Bob acceptance waiting on this Alice's OFFERED records;
//   c. import a fresh validated market match addressed to this taker;
//   d. import protocol Alice's ordinary offer for a reverse-direction negotiation this
//      maker already selected;
//   e. accept a valid take on one of this maker's own still-open offers.
//
// If exactly one of those exists it is performed, and it moves no funds: every one of them
// publishes or imports a signed coordination message. If more than one exists, nothing is
// performed and the choice is handed back to the operator. Only when there is no
// coordination action at all does `next` look at settlement and funding, and there it
// reports rather than acts.

const (
	// nextFreshEnv is the reference's freshness window variable.
	nextFreshEnv = "BLAKE_SWAP_NEXT_FRESH_SECONDS"
	// nextDefaultFreshSeconds is the window when the variable is unset.
	nextDefaultFreshSeconds = 3600
	// nextMinimumFreshSeconds is the smallest window the command will accept. A window
	// short enough to reject a match the relays just served is not a policy, it is a bug.
	nextMinimumFreshSeconds = 60
	// nextFutureSkewSeconds is the reference's clock-skew allowance. It is used where the
	// reference uses FUTURE_SKEW_SECONDS: a durable reverse selection is re-verified
	// against its exact signed match, and a match dated slightly in the future is still
	// live work rather than history.
	nextFutureSkewSeconds = 300
)

// The coordination action kinds, spelled as the reference spells them.
const (
	nextActionTakeOffer          = "take-offer"
	nextActionReceiveAccept      = "receive-accept"
	nextActionAcceptMatch        = "accept-match"
	nextActionAcceptReverseOffer = "accept-reverse-offer"
	nextActionAcceptTake         = "accept-take"
)

// nextAction is one coordination step, with the exact label the reference prints.
//
// The label is built when the action is collected and printed verbatim, because it carries
// the evidence the decision was made from — which offer, which take, which match — and an
// operator comparing two engines is comparing those lines.
type nextAction struct {
	kind    string
	label   string
	offerID string
	takeID  string
	matchID string
	swapID  string
}

// nextHistoricalMatch is a validated match that is outside the automatic window.
//
// It is kept rather than dropped because "we found this and will not act on it" is a
// different report from "we found nothing", and only one of them tells an operator that
// `accept-match` would still work.
type nextHistoricalMatch struct {
	eventID string
	age     int64
	reason  string
}

// runNextDiscovery performs the reference's argument-less `next`.
//
// It never funds, claims, refunds or advances a settlement: it may publish one signed
// coordination event, and every path that would touch money stops and says so.
func runNextDiscovery(ctx context.Context, engine *swap.Engine, _ []string) error {
	key, err := engineKey(engine)

	if err != nil {
		return err
	}

	me := key.PublicKey()

	freshness, err := nextFreshnessSeconds()

	if err != nil {
		return err
	}

	actions, historical, err := collectNextActions(ctx, engine, key, freshness)

	if err != nil {
		return err
	}

	if len(actions) == 1 {
		return performNextAction(ctx, engine, actions[0])
	}

	if len(actions) > 1 {
		fmt.Print(renderNextMultipleActions(actions))

		return nil
	}

	return reportNextNoCoordination(ctx, engine, me, freshness, historical)
}

// nextFreshnessSeconds reads and validates the automatic-coordination window.
func nextFreshnessSeconds() (int64, error) {
	raw := strings.TrimSpace(os.Getenv(nextFreshEnv))

	if raw == "" {
		return nextDefaultFreshSeconds, nil
	}

	seconds, err := strconv.ParseInt(raw, 10, 64)

	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", nextFreshEnv)
	}

	if seconds < nextMinimumFreshSeconds {
		return 0, fmt.Errorf("%s must be at least %d", nextFreshEnv, nextMinimumFreshSeconds)
	}

	return seconds, nil
}

// collectNextActions performs the reference's ordered scan.
//
// Every read here is silent. The reference runs each of these inside
// contextlib.redirect_stdout because the commands that implement them print operator
// output, and `next` prints exactly one block: its own. The same rule is kept here by
// calling the fetching helpers rather than the printing commands, except where a step is
// actually performed — and then the output belongs to the step.
func collectNextActions(
	ctx context.Context, engine *swap.Engine, key *nostr.Key, freshness int64,
) ([]nextAction, []nextHistoricalMatch, error) {
	me := key.PublicKey()
	client := marketClient()
	now := time.Now()

	events := nextMarketEvidence(ctx, 100)
	openOffers := nextOpenOffers(events, now)

	cached := nextVerifiedCachedEvents(ctx, client)

	// (a) Open offers from other makers this identity has not already taken.
	//
	// The prior-take check does not rely on the relay returning one take: a relay may hold
	// several, and two takes for one offer from one taker are two statements of intent.
	var actions []nextAction

	for _, offer := range openOffers {
		if offer.Offer.Maker == me {
			continue
		}

		if priorTake(cached, me, offer.EventID()) != nil {
			continue
		}

		actions = append(actions, nextAction{
			kind:    nextActionTakeOffer,
			offerID: offer.EventID(),
			label: fmt.Sprintf(
				"Take open offer %s for %s BLAKE / %s sats",
				truncate(offer.EventID(), 12),
				pythonFloat(blakeFloat(offer.Offer.BlakeAmount)),
				thousands(offer.Offer.BTCSats),
			),
		})
	}

	// (b) Authenticated Bob accept messages waiting to be imported.
	actions = append(actions, nextReceiveAcceptActions(ctx, engine, me)...)

	// (c) Fresh validated matches addressed to this taker.
	matchActions, historical, err := nextAcceptMatchActions(ctx, key, now, freshness)

	if err != nil {
		return nil, nil, err
	}

	actions = append(actions, matchActions...)

	// (d) Reverse-direction negotiations this maker selected durably.
	actions = append(actions, nextAcceptReverseOfferActions(ctx, engine, me, freshness)...)

	// (e) Valid takes on this maker's own still-open offers.
	actions = append(actions, nextAcceptTakeActions(ctx, client, me, openOffers)...)

	return actions, historical, nil
}

// nextMarketEvidence is the canonical market read, or nothing.
//
// A relay that cannot be reached is not a reason to fail `next`: the command's job is to
// report the one safe step, and "no step was found because nothing answered" is the honest
// answer. The reference swallows the same error for the same reason.
func nextMarketEvidence(ctx context.Context, limit int) []event.Event {
	read, err := readMarket(ctx, limit)

	if err != nil {
		return nil
	}

	return read.events
}

// nextOpenOffers derives the offers that are open now.
func nextOpenOffers(events []event.Event, now time.Time) []marketflow.Entry {
	entries, err := marketflow.Book(events, now)

	if err != nil {
		return nil
	}

	return marketflow.OpenOffers(entries)
}

// nextVerifiedCachedEvents is the durable, signature-verified event cache.
//
// The reference's prior-take lookup re-verifies every cached event before trusting it, and
// so does this: the cache is a file an operator can edit, and a forged take in it would
// otherwise suppress a real one.
func nextVerifiedCachedEvents(ctx context.Context, client *nostr.Client) []event.Event {
	now := time.Now()
	cached := cachedEvents(ctx, client.Cache)
	verified := make([]event.Event, 0, len(cached))

	for _, candidate := range cached {
		if err := event.Verify(candidate, now); err != nil {
			continue
		}

		verified = append(verified, candidate)
	}

	return verified
}

// nextReceiveAcceptActions collects OFFERED Alice records with an unimported acceptance.
func nextReceiveAcceptActions(ctx context.Context, engine *swap.Engine, me string) []nextAction {
	if engine.Transport == nil {
		return nil
	}

	var actions []nextAction

	for _, record := range nextLocalRecords(ctx) {
		if record.State != state.Offered {
			continue
		}

		if record.NostrAlicePub == nil || *record.NostrAlicePub != me {
			continue
		}

		if record.NostrBobPub == nil || *record.NostrBobPub == "" {
			continue
		}

		// Already imported: this belongs in READY TO FUND, not in the work queue.
		if record.BlakeBobPub != nil && record.BTCBobPub != nil {
			continue
		}

		// The read is non-destructive on every transport, which is what lets the step be
		// collected here and consumed by the command that performs it.
		message, err := engine.Transport.Receive(ctx, record.SwapID, "accept", coord.ReceiveOptions{
			ExpectedPubkey: *record.NostrBobPub,
			Mode:           engine.Mode,
		})

		if err != nil {
			continue
		}

		// Relay-side author filtering is an optimisation; this is the security boundary.
		if message.Sender != *record.NostrBobPub {
			continue
		}

		if swapID, _ := message.Body["swap_id"].(string); swapID != record.SwapID {
			continue
		}

		if !presentString(message.Body["blake_bob_pub"]) {
			continue
		}

		if !presentString(message.Body["btc_bob_pub"]) {
			continue
		}

		actions = append(actions, nextAction{
			kind:   nextActionReceiveAccept,
			swapID: record.SwapID,
			label:  "Import Bob acceptance for swap " + record.SwapID,
		})
	}

	return actions
}

// nextAcceptMatchActions collects fresh matches addressed to this taker.
func nextAcceptMatchActions(
	ctx context.Context, key *nostr.Key, now time.Time, freshness int64,
) ([]nextAction, []nextHistoricalMatch, error) {
	matches, err := fetchMatches(ctx, key, 200)

	if err != nil {
		return nil, nil, nil
	}

	// An unusable tombstone policy is a configuration error, not a discovery miss, so it is
	// reported rather than swallowed: silently forgetting which matches were dismissed would
	// resurface work the participant already refused.
	ignored, err := ignoredMatchIDs()

	if err != nil {
		return nil, nil, err
	}

	var actions []nextAction
	var historical []nextHistoricalMatch

	for _, item := range matches {
		if ignored[item.Event.ID] {
			continue
		}

		swapID := item.Match.SwapID

		// A match whose swap already exists locally has been imported; surfacing it again
		// would offer a second import of the same negotiation.
		if len(swapID) == 12 && localSwapExists(swapID) {
			continue
		}

		age := now.Unix() - item.Event.CreatedAt

		// A future-dated match is historical, not fresh: the reference calls it
		// "future-dated" and refuses to act on it, which keeps a clock-skewed relay from
		// driving work.
		if age < 0 {
			historical = append(historical, nextHistoricalMatch{
				eventID: item.Event.ID, age: age, reason: "future-dated",
			})

			continue
		}

		if age > freshness {
			historical = append(historical, nextHistoricalMatch{
				eventID: item.Event.ID, age: age, reason: "older-than-auto-window",
			})

			continue
		}

		actions = append(actions, nextAction{
			kind:    nextActionAcceptMatch,
			matchID: item.Event.ID,
			swapID:  swapID,
			label: fmt.Sprintf(
				"Import fresh market match %s for swap %s",
				truncate(item.Event.ID, 12), swapID,
			),
		})
	}

	return actions, historical, nil
}

// nextAcceptReverseOfferActions collects reverse-direction work this maker selected.
//
// Market matches are tagged with the *taker*, so a reverse-direction maker discovers this
// step from the durable selection he wrote when he accepted the take — and that selection
// is re-verified against the exact signed match before it can become work again. A durable
// record survives restarts; it must not resurrect an ancient negotiation.
func nextAcceptReverseOfferActions(
	ctx context.Context, engine *swap.Engine, me string, freshness int64,
) []nextAction {
	if engine.Transport == nil {
		return nil
	}

	directory := filepath.Join(stateDirectory(), selectionsDirectory)

	entries, err := os.ReadDir(directory)

	if err != nil {
		return nil
	}

	records := store.NewFile(stateDirectory())
	client := marketClient()

	var actions []nextAction

	for _, entry := range entries {
		name := entry.Name()

		if entry.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, "offer-") {
			continue
		}

		selection, err := readSelection(filepath.Join(directory, name))

		if err != nil || selection == nil {
			continue
		}

		if direction, _ := selection["direction"].(string); direction != string(market.BTCForBlake) {
			continue
		}

		maker, _ := selection["nostr_bob_pub"].(string)

		if maker == "" {
			maker, _ = selection["maker"].(string)
		}

		if maker != me {
			continue
		}

		swapID, _ := selection["swap_id"].(string)
		matchID, _ := selection["market_match_id"].(string)

		expectedAlice, _ := selection["nostr_alice_pub"].(string)

		if expectedAlice == "" {
			expectedAlice, _ = selection["taker"].(string)
		}

		if len(swapID) != 12 || len(matchID) != 64 || len(expectedAlice) != 64 {
			continue
		}

		if retired, err := records.Retired(ctx, swapID); err != nil || retired {
			continue
		}

		matchEvent, ok := nextFetchEvent(ctx, client, matchID)

		if !ok {
			continue
		}

		var message map[string]any

		if err := json.Unmarshal([]byte(matchEvent.Content), &message); err != nil {
			continue
		}

		if kind, _ := message["type"].(string); kind != "market-match" {
			continue
		}

		if direction, _ := message["direction"].(string); direction != string(market.BTCForBlake) {
			continue
		}

		if value, _ := message["swap_id"].(string); value != swapID {
			continue
		}

		if value, _ := message["maker"].(string); value != me {
			continue
		}

		if value, _ := message["taker"].(string); value != expectedAlice {
			continue
		}

		createdAt := int64Field(message["created_at"])

		if createdAt == 0 {
			createdAt = matchEvent.CreatedAt
		}

		now := time.Now().Unix()

		if createdAt <= 0 ||
			createdAt > now+nextFutureSkewSeconds ||
			now-createdAt > freshness {
			continue
		}

		// Bob's handshake is already complete, so there is nothing left to import.
		if existing, err := records.Load(ctx, swapID); err == nil {
			if role, ok := nextProtocolRole(existing, me); ok && role == "bob" &&
				existing.NostrBobPub != nil && *existing.NostrBobPub == me &&
				existing.BlakeBobPub != nil && existing.BTCBobPub != nil {
				continue
			}
		}

		// Protocol Alice's ordinary offer has to be readable before this step exists;
		// surfacing it earlier would advertise work that cannot be done.
		if _, err := engine.Transport.Receive(ctx, swapID, "offer", coord.ReceiveOptions{
			ExpectedPubkey: expectedAlice,
			Mode:           engine.Mode,
		}); err != nil {
			continue
		}

		actions = append(actions, nextAction{
			kind:    nextActionAcceptReverseOffer,
			matchID: matchID,
			swapID:  swapID,
			label: fmt.Sprintf(
				"Accept reverse swap offer for %s from match %s",
				swapID, truncate(matchID, 12),
			),
		})
	}

	return actions
}

// nextAcceptTakeActions collects valid takes on this maker's own still-open offers.
//
// A durable selection means this offer already produced a swap, so accepting another take
// for it would be a second swap from one advertisement — the one thing the selection
// records exist to prevent.
func nextAcceptTakeActions(
	ctx context.Context, client *nostr.Client, me string, openOffers []marketflow.Entry,
) []nextAction {
	now := time.Now()
	selections := filepath.Join(stateDirectory(), selectionsDirectory)

	var actions []nextAction

	for index := range openOffers {
		offer := &openOffers[index]

		if offer.Offer.Maker != me {
			continue
		}

		offerID := offer.EventID()

		if fileExists(filepath.Join(selections, "offer-"+offerID+".json")) {
			continue
		}

		for _, take := range nextTakesFor(ctx, client, offer, now) {
			actions = append(actions, nextAction{
				kind:    nextActionAcceptTake,
				offerID: offerID,
				takeID:  take.EventID,
				label: fmt.Sprintf(
					"Accept take %s on offer %s",
					truncate(take.EventID, 12), truncate(offerID, 12),
				),
			})
		}
	}

	return actions
}

// nextTakesFor reads the valid takes of one offer, silently.
//
// This is the same validator the take commands use (marketflow.TakesFor over every
// candidate event, with authentic takes cached for the next command); only the operator
// line those commands print is left out, because `next` prints one block of its own.
func nextTakesFor(
	ctx context.Context, client *nostr.Client, offer *marketflow.Entry, now time.Time,
) []marketflow.TakeEntry {
	report, err := client.FetchReport(ctx, nostr.Filter{
		Kinds:  []int{1},
		Topics: []string{marketflow.TakeTag},
		ETags:  []string{offer.EventID()},
		Limit:  200,
	})

	if err != nil {
		return nil
	}

	cacheMarketTakes(ctx, client, report.Events)

	candidates := append([]event.Event{}, report.Events...)
	candidates = append(candidates, cachedEvents(ctx, client.Cache)...)

	return marketflow.TakesFor(&offer.Offer, candidates, now)
}

// nextFetchEvent reads one event by id, from the durable cache first and the relays after.
//
// A durable selection references an immutable signed match by exact id, so a cached copy is
// as good as a fresh one — and reading the cache first is what makes the decision survive a
// relay that has forgotten the negotiation. The read is silent because `next` prints one
// block; the command that *performs* the step announces where its evidence came from.
func nextFetchEvent(ctx context.Context, client *nostr.Client, id string) (*event.Event, bool) {
	now := time.Now()

	if client.Cache != nil {
		if cached, ok, err := client.Cache.Get(ctx, id); err == nil && ok {
			if event.Verify(cached, now) == nil {
				return &cached, true
			}
		}
	}

	report, err := client.FetchReport(ctx, nostr.Filter{Kinds: []int{1}, Ids: []string{id}})

	if err != nil {
		return nil, false
	}

	for index := range report.Events {
		if report.Events[index].ID != id {
			continue
		}

		found := report.Events[index]

		if event.Verify(found, now) != nil {
			continue
		}

		if client.Cache != nil {
			_ = client.Cache.Put(ctx, found)
		}

		return &found, true
	}

	return nil, false
}

// nextLocalRecords loads every canonical local swap record, skipping unreadable ones.
//
// A scan must not fail because one record is corrupt: `next` is the command an autonomous
// loop calls, and a single unreadable file would otherwise stop every other swap from being
// reported. The reference behaves the same way, and reports the failure where it matters
// (in the dashboard and in resume-all).
func nextLocalRecords(ctx context.Context) []*model.Swap {
	records := store.NewFile(stateDirectory())

	ids, err := records.List(ctx)

	if err != nil {
		return nil
	}

	swaps := make([]*model.Swap, 0, len(ids))

	for _, id := range ids {
		record, err := records.Load(ctx, id)

		if err != nil {
			continue
		}

		swaps = append(swaps, record)
	}

	return swaps
}

// nextProtocolRole derives this participant's protocol role from the pinned identities.
//
// The two pinned roles are mutually exclusive, and a record that pins this identity as both
// is not a record this participant can reason about, so that case reports no role at all
// rather than picking one.
func nextProtocolRole(record *model.Swap, me string) (string, bool) {
	alice := record.NostrAlicePub != nil && *record.NostrAlicePub == me
	bob := record.NostrBobPub != nil && *record.NostrBobPub == me

	if alice && bob {
		return "", false
	}

	if alice {
		return "alice", true
	}

	if bob {
		return "bob", true
	}

	return "", false
}

// nextIsParty reports whether this identity is pinned to the swap at all.
func nextIsParty(record *model.Swap, me string) bool {
	if record.NostrAlicePub != nil && *record.NostrAlicePub == me {
		return true
	}

	return record.NostrBobPub != nil && *record.NostrBobPub == me
}

// performNextAction prints the single-step block and performs the step.
//
// The label, the disclaimer and the closing lines are the reference's, including the blank
// lines: the step's own output appears between them, which is where the reference puts it.
func performNextAction(ctx context.Context, engine *swap.Engine, action nextAction) error {
	fmt.Print("\n")
	fmt.Print("NEXT SAFE STEP\n")
	fmt.Print("==============\n")
	fmt.Print(action.label + "\n")
	fmt.Print("\n")
	fmt.Print("PASS: this coordination step does not itself fund, claim, or refund an HTLC\n")
	fmt.Print("\n")

	if err := dispatchNextAction(ctx, engine, action); err != nil {
		return err
	}

	fmt.Print("\n")
	fmt.Print("PASS: coordination step completed\n")
	fmt.Print("\n")
	fmt.Print("Run `blake next` again to see what is safe now.\n")

	return nil
}

// dispatchNextAction runs the one command that performs a collected step.
//
// It delegates rather than reimplementing: each of these is the explicit command an operator
// would type for the same step, so `next` and the explicit surface cannot disagree about
// what a step does.
func dispatchNextAction(ctx context.Context, engine *swap.Engine, action nextAction) error {
	switch action.kind {
	case nextActionTakeOffer:
		return runTakeMarketOffer(ctx, engine, []string{action.offerID})
	case nextActionReceiveAccept:
		return runReceiveAccept(ctx, engine, []string{action.swapID})
	case nextActionAcceptMatch:
		return runAcceptMatch(ctx, engine, []string{action.matchID})
	case nextActionAcceptReverseOffer:
		return runAcceptReverseSwapOffer(ctx, engine, []string{action.matchID})
	case nextActionAcceptTake:
		return runAcceptMarketTake(ctx, engine, []string{action.takeID})
	}

	return fmt.Errorf("Unknown next-step action: %s", action.kind)
}

// renderNextMultipleActions is the refusal to choose.
//
// It lists every candidate and performs none of them: which counterparty to trade with is
// the participant's decision, and a client that ordered two live negotiations for them
// would be making it.
func renderNextMultipleActions(actions []nextAction) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("MULTIPLE SAFE COORDINATION STEPS\n")
	out.WriteString("================================\n")
	out.WriteString("More than one authenticated negotiation needs attention. Refusing to choose automatically.\n")
	out.WriteString("\n")

	for index, action := range actions {
		fmt.Fprintf(&out, "%d. %s\n", index+1, action.label)
	}

	out.WriteString("\n")
	out.WriteString("Resolve the intended trade explicitly:\n")
	out.WriteString("  take <offer-prefix>\n")
	out.WriteString("  accept-take <offer-prefix>\n")
	out.WriteString("  accept-match <match-prefix>\n")
	out.WriteString("  engine accept-reverse-swap-offer <match-id>\n")
	out.WriteString("\n")
	out.WriteString("PASS: no funds moved and no ambiguous choice was made\n")

	return out.String()
}

// nextResumable is one swap with live post-funding work and the report that says so.
type nextResumable struct {
	swap   *model.Swap
	report *reconcile.Report
}

// reportNextNoCoordination is the part of `next` that looks at what happens next.
//
// With no coordination step, the report moves from negotiation to settlement. It still
// performs nothing: settlement economics belong to the recovery policy (`resume`) and
// funding belongs to an explicit `fund`, and this command exists partly to make that
// boundary visible.
func reportNextNoCoordination(
	ctx context.Context, engine *swap.Engine, me string, freshness int64,
	historical []nextHistoricalMatch,
) error {
	swaps := nextLocalRecords(ctx)

	resumable := nextResumableSwaps(ctx, engine, me, swaps)

	if len(resumable) == 1 {
		fmt.Print(renderNextSettlementWork(resumable[0]))

		return nil
	}

	if len(resumable) > 1 {
		fmt.Print(renderNextMultipleActiveSwaps(resumable))

		return nil
	}

	fundingReady := nextFundingReadySwaps(swaps, me)

	if len(fundingReady) == 1 {
		fmt.Print(renderNextReadyToFund(fundingReady[0]))

		return nil
	}

	if len(fundingReady) > 1 {
		fmt.Print(renderNextMultipleFundingReady(fundingReady))

		return nil
	}

	fmt.Print(renderNextNothingToDo(freshness, historical))

	return nil
}

// nextResumableSwaps is the reference's post-funding work queue.
//
// An ordinary unfunded OFFERED negotiation is never post-funding work. The one OFFERED
// exception is funding that is already confirmed on chain, which reconciliation reports as
// recovery-needed -> sync-funding.
func nextResumableSwaps(
	ctx context.Context, engine *swap.Engine, me string, swaps []*model.Swap,
) []nextResumable {
	var resumable []nextResumable

	for _, record := range swaps {
		if !nextIsParty(record, me) {
			continue
		}

		report, err := engine.Reconcile(ctx, record.SwapID)

		if err != nil {
			continue
		}

		if report.DecisionClass == reconcile.ClassSettled ||
			report.DecisionClass == reconcile.ClassManualAttention {
			continue
		}

		if record.State == state.Offered {
			if !(report.DecisionClass == reconcile.ClassRecoveryNeeded &&
				report.NextAction == reconcile.ActionSyncFunding) {
				continue
			}
		}

		resumable = append(resumable, nextResumable{swap: record, report: report})
	}

	return resumable
}

// nextFundingReadySwaps is the reference's funding queue.
//
// BLAKE funding is always the protocol-Alice commitment, whichever local participant
// profile created or imported the swap, so this asks who the protocol role is rather than
// who the market role was.
func nextFundingReadySwaps(swaps []*model.Swap, me string) []*model.Swap {
	var ready []*model.Swap

	for _, record := range swaps {
		if record.State != state.Offered {
			continue
		}

		role, ok := nextProtocolRole(record, me)

		if !ok || role != "alice" {
			continue
		}

		if record.NostrBobPub == nil || *record.NostrBobPub == "" {
			continue
		}

		if record.BlakeBobPub == nil || record.BTCBobPub == nil {
			continue
		}

		ready = append(ready, record)
	}

	return ready
}

// renderNextSettlementWork points at the existing recovery policy and does not run it.
func renderNextSettlementWork(item nextResumable) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("SETTLEMENT WORK AVAILABLE\n")
	out.WriteString("=========================\n")
	fmt.Fprintf(&out, "swap:  %s\n", item.swap.SwapID)
	fmt.Fprintf(&out, "state: %s\n", item.swap.State)
	fmt.Fprintf(&out, "next:  %s\n", item.report.NextAction)
	fmt.Fprintf(&out, "why:   %s\n", item.report.Reason)
	out.WriteString("\n")
	out.WriteString("On-chain funding or settlement evidence exists. ")
	out.WriteString("Use the existing autonomous recovery/settlement policy:\n")
	fmt.Fprintf(&out, "  scripts/blake resume %s\n", item.swap.SwapID)
	out.WriteString("\n")
	out.WriteString("PASS: next did not execute settlement economics\n")

	return out.String()
}

// renderNextMultipleActiveSwaps lists the live swaps and refuses to pick one.
func renderNextMultipleActiveSwaps(items []nextResumable) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("MULTIPLE ACTIVE SWAPS\n")
	out.WriteString("=====================\n")

	for _, item := range items {
		fmt.Fprintf(&out, "%s  %s  %s\n", item.swap.SwapID, item.swap.State, item.report.NextAction)
	}

	out.WriteString("\n")
	out.WriteString("Use `scripts/blake swaps` or `scripts/blake resume <swap-prefix>`.\n")

	return out.String()
}

// renderNextReadyToFund stops at the economic boundary.
func renderNextReadyToFund(record *model.Swap) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("READY TO FUND\n")
	out.WriteString("=============\n")
	fmt.Fprintf(&out, "swap:  %s\n", record.SwapID)
	fmt.Fprintf(&out, "trade: %s BLAKE / %s sats\n",
		pythonFloat(blakeFloat(record.BlakeAmount())), thousands(record.BTCSats))
	out.WriteString("\n")
	out.WriteString("All authenticated negotiation and settlement keys are in place.\n")
	out.WriteString("\n")
	out.WriteString("STOP: funding is an explicit economic commitment.\n")
	out.WriteString("Run:\n")
	out.WriteString("  scripts/blake fund\n")
	out.WriteString("\n")
	out.WriteString("PASS: next did not move funds\n")

	return out.String()
}

// renderNextMultipleFundingReady lists the ready trades and refuses to choose one.
func renderNextMultipleFundingReady(ready []*model.Swap) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("MULTIPLE FUNDING-READY SWAPS\n")
	out.WriteString("============================\n")
	out.WriteString("More than one trade is ready for an explicit BLAKE commitment. Refusing to choose.\n")
	out.WriteString("\n")

	for _, record := range ready {
		fmt.Fprintf(&out, "%s  %s BLAKE / %s sats\n",
			record.SwapID, pythonFloat(blakeFloat(record.BlakeAmount())), thousands(record.BTCSats))
	}

	out.WriteString("\n")
	out.WriteString("Use `fund-blake <swap-id>` for an explicit choice.\n")
	out.WriteString("PASS: no funds moved\n")

	return out.String()
}

// renderNextNothingToDo is the quiet answer, plus whatever history was set aside.
func renderNextNothingToDo(freshness int64, historical []nextHistoricalMatch) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("NOTHING TO DO\n")
	out.WriteString("=============\n")
	out.WriteString("No unambiguous current coordination, funding-ready trade, or resumable settlement was found.\n")

	if len(historical) > 0 {
		out.WriteString("\n")
		out.WriteString("HISTORICAL MATCHES IGNORED\n")
		out.WriteString("==========================\n")
		fmt.Fprintf(&out,
			"%d valid unimported match(es) were outside the automatic %d-minute window.\n",
			len(historical), freshness/60)
		out.WriteString("They remain available explicitly with `scripts/blake accept-match <prefix>`.\n")
	}

	out.WriteString("\n")
	out.WriteString("Use `scripts/blake market` to browse offers or ")
	out.WriteString("`scripts/blake swaps` for the dashboard.\n")

	return out.String()
}

// presentString reports whether a decoded field is a non-empty string.
func presentString(value any) bool {
	text, ok := value.(string)

	return ok && text != ""
}

// int64Field reads an integer from decoded JSON, whatever shape the decoder chose.
func int64Field(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case int64:
		return typed
	case json.Number:
		parsed, err := typed.Int64()

		if err != nil {
			return 0
		}

		return parsed
	}

	return 0
}
