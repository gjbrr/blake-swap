package marketops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/store"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// AcceptMatch imports the swap a market match binds, as the taker.
//
// The match is the maker's receipt, and this is the taker's side of it: the taker validates
// the match against the exact offer and take it names, then binds that immutable negotiation
// tuple to the swap id before creating any local state. A later signed match may be new
// evidence for the same tuple but may not redefine it — offer, take, swap, identities and
// terms are what "the same negotiation" means, so a second match that disagrees with the
// first is refused rather than preferred.
//
// supplied is the match event a caller already fetched, so the relay is not read twice and
// the fetch announcement is not printed twice.
func AcceptMatch(
	ctx context.Context, env *Env, matchID string, supplied *event.Event, hooks MatchHooks,
) (*MatchImport, error) {
	if err := env.check(); err != nil {
		return nil, err
	}

	if len(matchID) != 64 {
		return nil, fmt.Errorf("match_id must be the full 64-character Nostr event ID")
	}

	bob := env.Identity.Public

	candidate := supplied

	if candidate == nil {
		fetched, err := FetchEvent(ctx, env, matchID)

		if err != nil {
			return nil, err
		}

		candidate = fetched
	}

	item, err := validateMatch(ctx, env, *candidate, bob)

	if err != nil {
		return nil, err
	}

	bindingName := bindingPath(item.SwapID)

	proposed := map[string]any{
		"version":  1,
		"offer_id": item.OfferID,
		"take_id":  item.TakeID,
		"swap_id":  item.SwapID,
		"maker":    item.Maker,
		"taker":    item.Taker,

		"blake_amount": json.Number(item.BlakeAmount.Format()),
		"btc_sats":     item.BTCSats,

		// Informational evidence only: the tuple above is the negotiation.
		"first_match_id": matchID,
	}

	existing, err := env.readDocument(ctx, bindingName)

	if err != nil {
		return nil, err
	}

	if existing != nil {
		if err := bindingConflict(existing, proposed); err != nil {
			return nil, err
		}
	}

	record, err := env.Records.Load(ctx, item.SwapID)

	if err == nil {
		return repeatImport(ctx, env, repeatImportRequest{
			MatchID: matchID, Item: item, Bob: bob, Record: record,
			Binding: existing, Name: bindingName, Write: proposed,
		}, hooks)
	}

	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	//
	// No local swap yet: the offer this match names has to be accepted as an ordinary swap,
	// with the market's terms pinned so a different offer cannot answer instead.
	//
	if hooks.BeforeAccept != nil {
		hooks.BeforeAccept()
	}

	blake, sats := item.BlakeAmount, item.BTCSats

	accepted, err := env.Engine.AcceptOffer(ctx, swap.AcceptRequest{
		SwapID:              item.SwapID,
		ExpectedAlicePub:    item.Maker,
		ExpectedBlakeAmount: &blake,
		ExpectedBTCSats:     &sats,
	})

	if err != nil {
		return nil, err
	}

	if hooks.AfterAccept != nil {
		hooks.AfterAccept(accepted)
	}

	pinned, err := env.Records.Load(ctx, item.SwapID)

	if err != nil {
		return nil, err
	}

	if err := pinBob(ctx, env.Records, pinned, bob); err != nil {
		return nil, err
	}

	// Persist the immutable market-to-swap binding only after the taker's swap state exists:
	// a binding that named no record would be a claim about state this machine does not have.
	if existing == nil {
		if err := env.writeDocument(ctx, bindingName, proposed); err != nil {
			return nil, err
		}
	}

	imported := MatchImport{
		MatchID: matchID, OfferID: item.OfferID, TakeID: item.TakeID, SwapID: item.SwapID,
		Maker: item.Maker, Taker: item.Taker,
		Blake: item.BlakeAmount, BTCSats: item.BTCSats,
		Accept: accepted,
	}

	if hooks.AfterImport != nil {
		hooks.AfterImport(imported)
	}

	return &imported, nil
}

// repeatImportRequest describes a match whose swap already exists locally.
type repeatImportRequest struct {
	MatchID string
	Item    *marketflow.ParsedMatch
	Bob     string
	Record  *model.Swap
	Binding map[string]any
	Name    string
	Write   map[string]any
}

// repeatImport finishes an import that was already done, or repairs it.
//
// The taker's record must agree with the match on every identity and on the trade, because
// those are the facts the binding is supposed to preserve. A missing binding is the one gap
// that is repaired: a crash between the two writes leaves a valid swap and no receipt, and
// refusing that would make the swap unusable.
func repeatImport(
	ctx context.Context,
	env *Env,
	req repeatImportRequest,
	hooks MatchHooks,
) (*MatchImport, error) {
	record := req.Record
	item := req.Item

	if record.NostrAlicePub == nil || *record.NostrAlicePub != item.Maker {
		return nil, fmt.Errorf("SECURITY FAILURE: existing Bob swap has different Alice identity")
	}

	if record.NostrBobPub == nil {
		if err := pinBob(ctx, env.Records, record, req.Bob); err != nil {
			return nil, err
		}
	} else if *record.NostrBobPub != req.Bob {
		return nil, fmt.Errorf("SECURITY FAILURE: existing Bob swap has different Bob identity")
	}

	if record.BlakeAmount() != item.BlakeAmount {
		return nil, fmt.Errorf("SECURITY FAILURE: existing Bob swap has different BLAKE terms")
	}

	if record.BTCSats != item.BTCSats {
		return nil, fmt.Errorf("SECURITY FAILURE: existing Bob swap has different BTC terms")
	}

	if req.Binding == nil {
		if err := env.writeDocument(ctx, req.Name, req.Write); err != nil {
			return nil, err
		}
	}

	imported := MatchImport{
		MatchID: req.MatchID, OfferID: item.OfferID, TakeID: item.TakeID, SwapID: item.SwapID,
		Maker: item.Maker, Taker: item.Taker,
		Blake: item.BlakeAmount, BTCSats: item.BTCSats,
		Repeated: true,
	}

	if hooks.AfterImport != nil {
		hooks.AfterImport(imported)
	}

	return &imported, nil
}

// pinBob records the taker's authenticated identity on their own swap.
func pinBob(ctx context.Context, records store.Store, record *model.Swap, bob string) error {
	if record.NostrBobPub != nil && *record.NostrBobPub == bob {
		return nil
	}

	record.NostrBobPub = &bob

	return records.Save(ctx, store.Bob, record)
}

// validateMatch validates one match against the events it references.
//
// The two references are immutable, so they are satisfied from the verified cache when
// possible and only fetched when this machine has never seen them.
func validateMatch(
	ctx context.Context, env *Env, candidate event.Event, taker string,
) (*marketflow.ParsedMatch, error) {
	if err := event.Verify(candidate, env.now()); err != nil {
		return nil, err
	}

	var message map[string]any

	if err := json.Unmarshal([]byte(candidate.Content), &message); err != nil {
		return nil, fmt.Errorf("Market match contains invalid JSON")
	}

	offerID, _ := message["offer_id"].(string)

	if len(offerID) != 64 {
		return nil, fmt.Errorf("Invalid market offer ID")
	}

	takeID, _ := message["take_id"].(string)

	if len(takeID) != 64 {
		return nil, fmt.Errorf("Invalid market take ID")
	}

	offer, err := referencedEvent(ctx, env, offerID)

	if err != nil {
		return nil, err
	}

	take, err := referencedEvent(ctx, env, takeID)

	if err != nil {
		return nil, err
	}

	parsed, err := marketflow.ParseMatch(candidate, *offer, *take, env.now())

	if err != nil {
		return nil, err
	}

	if parsed.Taker != taker {
		return nil, fmt.Errorf("Market match is for another taker")
	}

	return parsed, nil
}

// referencedEvent reads one immutable referenced event, cache first.
func referencedEvent(ctx context.Context, env *Env, id string) (*event.Event, error) {
	if env.Client.Cache != nil {
		cached, ok, err := env.Client.Cache.Get(ctx, id)

		if err == nil && ok {
			if verifyErr := event.Verify(cached, env.now()); verifyErr == nil {
				return &cached, nil
			}
		}
	}

	return FetchEvent(ctx, env, id)
}

// bindingConflict refuses a binding that describes a different negotiation.
func bindingConflict(existing, proposed map[string]any) error {
	for _, key := range []string{
		"offer_id", "take_id", "swap_id", "maker", "taker", "blake_amount", "btc_sats",
	} {
		if _, ok := existing[key]; !ok {
			return fmt.Errorf("Existing market match binding is malformed")
		}
	}

	for _, key := range []string{"offer_id", "take_id", "swap_id", "maker", "taker"} {
		if fmt.Sprint(existing[key]) != fmt.Sprint(proposed[key]) {
			return fmt.Errorf(
				"SECURITY FAILURE: existing market match binding disagrees with validated match",
			)
		}
	}

	if asFloat(existing["blake_amount"]) != asFloat(proposed["blake_amount"]) {
		return fmt.Errorf(
			"SECURITY FAILURE: existing market match binding disagrees with validated match",
		)
	}

	if asFloat(existing["btc_sats"]) != asFloat(proposed["btc_sats"]) {
		return fmt.Errorf(
			"SECURITY FAILURE: existing market match binding disagrees with validated match",
		)
	}

	return nil
}

// asFloat reads a JSON number, which is how both amounts are stored.
func asFloat(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case json.Number:
		parsed, err := typed.Float64()

		if err != nil {
			return math.NaN()
		}

		return parsed
	case int64:
		return float64(typed)
	case int:
		return float64(typed)
	}

	return math.NaN()
}

// ImportReverseMatch imports a btc-for-blake match as the taker.
//
// In this direction the market maker is protocol Bob and the taker is protocol Alice, so
// this is where Alice creates the secret. That is the whole asymmetry: Bob publishes the
// match and reserves the swap id, and only Alice generates S, from her own machine, against
// deadlines she derives from her own view of both chains.
func ImportReverseMatch(
	ctx context.Context, env *Env, matchID string, supplied *event.Event, hooks MatchHooks,
) (*MatchImport, error) {
	if err := env.check(); err != nil {
		return nil, err
	}

	if len(matchID) != 64 {
		return nil, fmt.Errorf("match_id must be the full 64-character Nostr event ID")
	}

	alice := env.Identity.Public

	candidate := supplied

	if candidate == nil {
		fetched, err := FetchEvent(ctx, env, matchID)

		if err != nil {
			return nil, err
		}

		candidate = fetched
	}

	item, err := validateMatch(ctx, env, *candidate, alice)

	if err != nil {
		return nil, err
	}

	if err := reverseMatchRoles(item, alice); err != nil {
		return nil, err
	}

	bob := item.Maker

	record, err := env.Records.Load(ctx, item.SwapID)

	if err == nil {
		if record.NostrAlicePub == nil || *record.NostrAlicePub != alice {
			return nil, fmt.Errorf("SECURITY FAILURE: existing Alice swap has different Alice identity")
		}

		if record.NostrBobPub == nil || *record.NostrBobPub != bob {
			return nil, fmt.Errorf("SECURITY FAILURE: existing Alice swap has different Bob identity")
		}

		if record.BlakeAmount() != item.BlakeAmount {
			return nil, fmt.Errorf("SECURITY FAILURE: existing Alice swap has different BLAKE terms")
		}

		if record.BTCSats != item.BTCSats {
			return nil, fmt.Errorf("SECURITY FAILURE: existing Alice swap has different BTC terms")
		}

		imported := MatchImport{
			MatchID: matchID, OfferID: item.OfferID, TakeID: item.TakeID, SwapID: item.SwapID,
			Maker: item.Maker, Taker: item.Taker,
			Blake: item.BlakeAmount, BTCSats: item.BTCSats,
			Repeated: true,
		}

		if hooks.AfterImport != nil {
			hooks.AfterImport(imported)
		}

		return &imported, nil
	}

	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	// The reserved swap id is adopted, not invented: it is what both sides refer to when
	// they speak about this negotiation, so only the secret is new here.
	created, err := env.Engine.New(ctx, swap.NewRequest{
		BlakeAmount: item.BlakeAmount,
		BTCSats:     item.BTCSats,
		SwapID:      item.SwapID,
		NostrBobPub: bob,
		Random:      env.Random,
	})

	if err != nil {
		return nil, err
	}

	// The binding reserves the swap id for this exact match, so it must survive a crash
	// that loses the process but not the disk.
	binding := map[string]any{
		"version":   2,
		"direction": string(item.Direction),

		"offer_id": item.OfferID,
		"take_id":  item.TakeID,
		"match_id": matchID,
		"swap_id":  item.SwapID,

		"maker": item.Maker,
		"taker": item.Taker,

		"maker_role": item.MakerRole,
		"taker_role": item.TakerRole,

		"blake_amount": json.Number(item.BlakeAmount.Format()),
		"btc_sats":     item.BTCSats,
	}

	if err := env.writeDocument(ctx, bindingPath(item.SwapID), binding); err != nil {
		return nil, err
	}

	imported := MatchImport{
		MatchID: matchID, OfferID: item.OfferID, TakeID: item.TakeID, SwapID: created.Swap.SwapID,
		Maker: item.Maker, Taker: item.Taker,
		Blake: item.BlakeAmount, BTCSats: item.BTCSats,
	}

	if hooks.AfterImport != nil {
		hooks.AfterImport(imported)
	}

	return &imported, nil
}

// reverseMatchRoles checks that a validated match is this participant's reverse trade.
//
// The direction implies the roles, so all four checks say the same thing four ways: a
// btc-for-blake match is Bob's advertisement accepted by Alice, and the current participant
// must be that Alice. Each is refused separately because the operator's remedy differs — a
// wrong direction is a wrong command, a wrong taker is a wrong key.
func reverseMatchRoles(item *marketflow.ParsedMatch, alice string) error {
	if item.Direction != market.BTCForBlake {
		return fmt.Errorf("Market match is not btc-for-blake")
	}

	if item.MakerRole != "bob" {
		return fmt.Errorf("SECURITY FAILURE: reverse match maker is not protocol Bob")
	}

	if item.TakerRole != "alice" {
		return fmt.Errorf("SECURITY FAILURE: reverse match taker is not protocol Alice")
	}

	if item.Taker != alice {
		return fmt.Errorf("SECURITY FAILURE: current participant is not the matched taker")
	}

	return nil
}
