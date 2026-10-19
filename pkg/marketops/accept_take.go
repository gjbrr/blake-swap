package marketops

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// errNotExist is what Files returns for an absent document.
var errNotExist = fs.ErrNotExist

// AcceptTake turns one signed take into an atomic swap.
//
// This is the maker's one irreversible market act. Everything before it is an advertisement
// or an expression of interest; accepting creates the swap, pins both authenticated
// identities, and publishes both the ordinary swap offer and the market match that binds
// offer, take and swap together.
//
// Three rules shape the order of the work here, and all three are the reference's:
//
//   - the take is re-validated through the same canonical validator everybody else uses, so
//     a take that reached this command through a hand-edited file or a relay that served
//     something else cannot create a swap;
//   - the durable selection records are written before anything is announced, because they
//     are what stop a second take creating a second swap from one offer; and
//   - the duplicate path is not an error. A crash between publishing and recording, or an
//     operator running the command twice, must end in "already accepted" rather than in a
//     second secret.
//
// supplied is the take event a caller already fetched, which is how the CLI avoids reading
// the relay twice and printing the fetch announcement twice.
func AcceptTake(
	ctx context.Context, env *Env, takeID string, supplied *event.Event, hooks TakeHooks,
) (*TakeAcceptance, error) {
	if err := env.check(); err != nil {
		return nil, err
	}

	if len(takeID) != 64 {
		return nil, fmt.Errorf("take_id must be the full 64-character Nostr event ID")
	}

	now := env.now()

	takeEvent := supplied

	if takeEvent == nil {
		fetched, err := FetchEvent(ctx, env, takeID)

		if err != nil {
			return nil, err
		}

		takeEvent = fetched
	}

	if err := event.Verify(*takeEvent, now); err != nil {
		return nil, err
	}

	// The take's content is read before it is validated as a take, because the two refusals
	// are different facts: a message that is not JSON is not a malformed take, it is not a
	// take at all.
	var message map[string]any

	if err := json.Unmarshal([]byte(takeEvent.Content), &message); err != nil {
		return nil, fmt.Errorf("Market take content is invalid JSON")
	}

	if kind, _ := message["type"].(string); kind != "market-take" {
		return nil, fmt.Errorf("Referenced event is not a market take")
	}

	offerID, _ := message["offer_id"].(string)

	if len(offerID) != 64 {
		return nil, fmt.Errorf("Market take has invalid offer_id")
	}

	takeSelectionName := selectionPath(takeID)
	offerSelectionName := selectionPath("offer-" + offerID)

	// A durable selection is the record that this negotiation already happened, and it is
	// what allows a retry to proceed past expiry: an established negotiation is not erased
	// by the offer's lifetime running out.
	offerSelection, err := env.readDocument(ctx, offerSelectionName)

	if err != nil {
		return nil, err
	}

	takeSelection, err := env.readDocument(ctx, takeSelectionName)

	if err != nil {
		return nil, err
	}

	alreadySelected := offerSelection != nil || takeSelection != nil

	set, err := ReadTakeSet(ctx, env, offerID, 200, []event.Event{*takeEvent}, alreadySelected, alreadySelected)

	if err != nil {
		return nil, err
	}

	var selected *marketflow.TakeEntry

	for index := range set.Takes {
		if set.Takes[index].EventID == takeID {
			selected = &set.Takes[index]

			break
		}
	}

	if selected == nil {
		return nil, fmt.Errorf("SECURITY FAILURE: selected take did not pass market-take validation")
	}

	// In the reverse direction the offer's maker is protocol Bob and must never create
	// Alice's secret, so that path is a different operation rather than a branch here.
	if selected.Take.Direction == market.BTCForBlake {
		return nil, &ReverseTakeError{
			Offer: set.Offer, Terms: set.Listed, Take: takeEvent,
			OfferID: offerID, TakeID: takeID,
		}
	}

	alice := env.Identity.Public
	bob := takeEvent.Pubkey

	if set.Offer.Pubkey != alice {
		return nil, fmt.Errorf(
			"SECURITY FAILURE: current Alice identity did not create the referenced market offer",
		)
	}

	if selected.Taker != bob {
		return nil, fmt.Errorf("SECURITY FAILURE: validated taker identity mismatch")
	}

	if offerSelection != nil {
		boundTake, _ := offerSelection["take_id"].(string)
		boundSwap, _ := offerSelection["swap_id"].(string)

		if value, _ := offerSelection["offer_id"].(string); value != offerID ||
			boundTake == "" || boundSwap == "" {
			return nil, fmt.Errorf("Existing market offer selection record is malformed")
		}

		if boundTake != takeID {
			return nil, fmt.Errorf(
				"SECURITY FAILURE: market offer has already been accepted through a different take",
			)
		}
	}

	if takeSelection != nil {
		if value, _ := takeSelection["offer_id"].(string); value != offerID {
			return nil, fmt.Errorf("Existing market selection record is malformed")
		}

		if value, _ := takeSelection["take_id"].(string); value != takeID {
			return nil, fmt.Errorf("Existing market selection record is malformed")
		}

		if value, _ := takeSelection["swap_id"].(string); value == "" {
			return nil, fmt.Errorf("Existing market selection record is malformed")
		}
	}

	if offerSelection != nil || takeSelection != nil {
		return repeatAccept(ctx, env, repeatRequest{
			OfferID:          offerID,
			TakeID:           takeID,
			Alice:            alice,
			Bob:              bob,
			OfferSelection:   offerSelection,
			TakeSelection:    takeSelection,
			TakeName:         takeSelectionName,
			OfferName:        offerSelectionName,
			OfferSelectionOn: offerSelection != nil,
			TakeSelectionOn:  takeSelection != nil,
		}, hooks)
	}

	// Fresh parameters under the same timeout policy normal swap construction uses: an
	// accepted take is an ordinary swap that happens to have been negotiated in public.
	created, err := env.Engine.New(ctx, swap.NewRequest{
		BlakeAmount: set.Listed.BlakeAmount,
		BTCSats:     set.Listed.BTCSats,
		NostrBobPub: bob,
		Random:      env.Random,
	})

	if err != nil {
		return nil, err
	}

	record := *created.Swap

	selectionRecord := map[string]any{
		"version":         1,
		"offer_id":        offerID,
		"take_id":         takeID,
		"swap_id":         record.SwapID,
		"nostr_alice_pub": alice,
		"nostr_bob_pub":   bob,
	}

	// Durable before anything is announced: these two files are what stop a second take
	// creating a second swap from this offer, so losing either is losing that guarantee.
	if err := env.writeDocument(ctx, takeSelectionName, selectionRecord); err != nil {
		return nil, err
	}

	if err := env.writeDocument(ctx, offerSelectionName, selectionRecord); err != nil {
		return nil, err
	}

	acceptance := TakeAcceptance{
		OfferID: offerID, TakeID: takeID, SwapID: record.SwapID,
		Alice: alice, Bob: bob,
		Blake: set.Listed.BlakeAmount, BTCSats: set.Listed.BTCSats,
		BTCTimeout: record.BTCTimeout, BlakeTimeout: record.BlakeTimeout,
	}

	if hooks.Accepted != nil {
		hooks.Accepted(acceptance)
	}

	if hooks.BeforeOfferPublish != nil {
		hooks.BeforeOfferPublish()
	}

	published, err := env.Engine.PublishOffer(ctx, record.SwapID)

	if err != nil {
		return nil, err
	}

	if published != nil {
		acceptance.OfferLocation = published.Location
	}

	if hooks.AfterOfferPublish != nil {
		hooks.AfterOfferPublish(acceptance.OfferLocation)
	}

	if hooks.BeforeMatchPublish != nil {
		hooks.BeforeMatchPublish()
	}

	match, err := PublishMatch(ctx, env, takeID)

	if err != nil {
		return nil, err
	}

	acceptance.MatchID = match.MatchID

	if hooks.AfterMatchPublish != nil {
		hooks.AfterMatchPublish(match)
	}

	return &acceptance, nil
}

// repeatRequest describes a selection that already exists.
type repeatRequest struct {
	OfferID          string
	TakeID           string
	Alice            string
	Bob              string
	OfferSelection   map[string]any
	TakeSelection    map[string]any
	TakeName         string
	OfferName        string
	OfferSelectionOn bool
	TakeSelectionOn  bool
}

// repeatAccept finishes a negotiation that was already recorded.
//
// Two records describe one accepted take. If an earlier run stopped between the two writes,
// one of them is missing, and this repairs it rather than refusing to work — but only after
// both records and the swap agree on every identity and on the swap id, because repairing a
// disagreement would be inventing a fact.
func repeatAccept(
	ctx context.Context, env *Env, req repeatRequest, hooks TakeHooks,
) (*TakeAcceptance, error) {
	canonical := req.TakeSelection

	if canonical == nil {
		canonical = req.OfferSelection
	}

	swapID, _ := canonical["swap_id"].(string)

	if req.OfferSelectionOn && req.TakeSelectionOn {
		offerSwap, _ := req.OfferSelection["swap_id"].(string)
		takeSwap, _ := req.TakeSelection["swap_id"].(string)

		if offerSwap != takeSwap {
			return nil, fmt.Errorf("SECURITY FAILURE: market selection records disagree about swap_id")
		}
	}

	if value, _ := canonical["nostr_alice_pub"].(string); value != req.Alice {
		return nil, fmt.Errorf("SECURITY FAILURE: existing market selection Alice identity mismatch")
	}

	if value, _ := canonical["nostr_bob_pub"].(string); value != req.Bob {
		return nil, fmt.Errorf("SECURITY FAILURE: existing market selection Bob identity mismatch")
	}

	record, err := env.Records.Load(ctx, swapID)

	if err != nil {
		return nil, err
	}

	if record.NostrAlicePub == nil || *record.NostrAlicePub != req.Alice {
		return nil, fmt.Errorf("SECURITY FAILURE: existing swap Alice identity mismatch")
	}

	if record.NostrBobPub == nil || *record.NostrBobPub != req.Bob {
		return nil, fmt.Errorf("SECURITY FAILURE: existing swap Bob identity mismatch")
	}

	if !req.TakeSelectionOn {
		if err := env.writeDocument(ctx, req.TakeName, canonical); err != nil {
			return nil, err
		}
	}

	if !req.OfferSelectionOn {
		if err := env.writeDocument(ctx, req.OfferName, canonical); err != nil {
			return nil, err
		}
	}

	acceptance := TakeAcceptance{
		OfferID: req.OfferID, TakeID: req.TakeID, SwapID: swapID,
		Alice: req.Alice, Bob: req.Bob,
		Blake: amount.Blake(record.BlakeAmountBaseUnits), BTCSats: record.BTCSats,
		BTCTimeout: record.BTCTimeout, BlakeTimeout: record.BlakeTimeout,
		Repeated: true,
	}

	if hooks.Accepted != nil {
		hooks.Accepted(acceptance)
	}

	return &acceptance, nil
}

// PublishMatch signs and publishes the maker's match for an accepted take.
//
// The match is the terminal market event: it binds an offer, a take and a local swap, and it
// adds no new terms — everything in it is re-derived from the two events it names and checked
// against the swap. The local selection record is what makes that possible, and it is
// rewritten with the match id afterwards, because a crash that lost the link would leave a
// published match unresolvable from this machine.
func PublishMatch(ctx context.Context, env *Env, takeID string) (MatchPublication, error) {
	if err := env.check(); err != nil {
		return MatchPublication{}, err
	}

	if len(takeID) != 64 {
		return MatchPublication{}, fmt.Errorf(
			"take_id must be the full 64-character Nostr event ID",
		)
	}

	selection, err := env.readDocument(ctx, selectionPath(takeID))

	if err != nil {
		return MatchPublication{}, err
	}

	if selection == nil {
		return MatchPublication{}, fmt.Errorf("Alice has not accepted this market take")
	}

	swapID, _ := selection["swap_id"].(string)

	if swapID == "" {
		return MatchPublication{}, fmt.Errorf("Market selection has no swap_id")
	}

	offerID, _ := selection["offer_id"].(string)

	record, err := env.Records.Load(ctx, swapID)

	if err != nil {
		return MatchPublication{}, err
	}

	takeEvent, err := FetchEvent(ctx, env, takeID)

	if err != nil {
		return MatchPublication{}, err
	}

	take, err := marketflow.ParseTake(*takeEvent, env.now())

	if err != nil {
		return MatchPublication{}, fmt.Errorf("Referenced event is not a market take")
	}

	// The take must name the offer the selection recorded: a selection that has been edited,
	// or a take that has been swapped underneath it, is not the negotiation that was accepted.
	if take.OfferID != offerID {
		return MatchPublication{}, fmt.Errorf("SECURITY FAILURE: take references a different market offer")
	}

	offerEvent, err := FetchEvent(ctx, env, offerID)

	if err != nil {
		return MatchPublication{}, err
	}

	listed, err := marketflow.ParseOffer(*offerEvent, env.now())

	if err != nil {
		return MatchPublication{}, fmt.Errorf("Referenced event is not a market offer")
	}

	if err := event.Verify(*offerEvent, env.now()); err != nil {
		return MatchPublication{}, err
	}

	if err := event.Verify(*takeEvent, env.now()); err != nil {
		return MatchPublication{}, err
	}

	// A durable selection may allow recovery of an old negotiation, but it must not create a
	// newly signed post-expiry match: counterparties reject those.
	if listed.ExpiresAt <= env.now().Unix() {
		return MatchPublication{}, fmt.Errorf(
			"Market offer expired before match publication; publish a fresh offer instead",
		)
	}

	alice, bob := env.Identity.Public, takeEvent.Pubkey

	if offerEvent.Pubkey != alice {
		return MatchPublication{}, fmt.Errorf("SECURITY FAILURE: market offer was not signed by current Alice")
	}

	if value, _ := selection["nostr_alice_pub"].(string); value != alice {
		return MatchPublication{}, fmt.Errorf("SECURITY FAILURE: stored Alice identity mismatch")
	}

	if value, _ := selection["nostr_bob_pub"].(string); value != bob {
		return MatchPublication{}, fmt.Errorf("SECURITY FAILURE: stored Bob identity mismatch")
	}

	if record.NostrAlicePub == nil || *record.NostrAlicePub != alice {
		return MatchPublication{}, fmt.Errorf("SECURITY FAILURE: swap Alice identity mismatch")
	}

	if record.NostrBobPub == nil || *record.NostrBobPub != bob {
		return MatchPublication{}, fmt.Errorf("SECURITY FAILURE: swap Bob identity mismatch")
	}

	if listed.BlakeAmount != record.BlakeAmount() {
		return MatchPublication{}, fmt.Errorf("SECURITY FAILURE: BLAKE terms changed")
	}

	if listed.BTCSats != record.BTCSats {
		return MatchPublication{}, fmt.Errorf("SECURITY FAILURE: BTC terms changed")
	}

	key := env.Identity

	built, _, err := marketflow.BuildMatch(key, marketflow.BuildMatchOptions{
		Offer:       *listed,
		OfferEvent:  *offerEvent,
		TakeEvent:   *takeEvent,
		Taker:       take,
		SwapID:      record.SwapID,
		BlakeAmount: listed.BlakeAmount,
		BTCSats:     listed.BTCSats,
		CreatedAt:   env.now().Unix(),
	})

	if err != nil {
		return MatchPublication{}, err
	}

	report, err := env.Client.Publish(ctx, built)

	if err != nil {
		return MatchPublication{}, err
	}

	// Cached before the selection is rewritten: a match this machine published must stay
	// resolvable from its own cache after the relay window closes.
	if env.Client.Cache != nil {
		if err := env.Client.Cache.Put(ctx, built); err != nil {
			return MatchPublication{}, err
		}
	}

	selection["market_match_id"] = built.ID

	if err := env.writeDocument(ctx, selectionPath(takeID), selection); err != nil {
		return MatchPublication{}, err
	}

	return MatchPublication{
		OfferID: offerID, TakeID: takeID, MatchID: built.ID, SwapID: record.SwapID,
		Published: built, Report: report,
	}, nil
}
