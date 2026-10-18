package marketflow

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/nostr"
)

// MatchTag identifies a market match.
const MatchTag = "blake-swap-market-match"

// BuildMatchOptions is what a maker decides when sealing a trade.
type BuildMatchOptions struct {
	// Offer is the parsed offer being matched.
	Offer ListedOffer
	// OfferEvent and TakeEvent are the exact signed events the match references.
	OfferEvent, TakeEvent event.Event
	// Taker is the take's parsed content.
	Taker *market.Take
	// SwapID is the atomic swap the match binds.
	SwapID string
	// BlakeAmount and BTCSats are the agreed terms.
	BlakeAmount amount.Blake
	BTCSats     int64
	// CreatedAt pins the event time. Zero means now.
	CreatedAt int64
}

// BuildMatch builds and signs a market match.
//
// A match is the maker's receipt that a particular take was accepted for a
// particular swap. It embeds no new terms: it restates the offer's economics and
// names the take, so any reader can re-derive every claim from the two events it
// references.
func BuildMatch(key *nostr.Key, opts BuildMatchOptions) (event.Event, market.Match, error) {
	if key == nil {
		return event.Event{}, market.Match{}, fmt.Errorf("a market match needs an identity")
	}

	//
	// Only the original maker may seal the trade. A taker that could publish a
	// match would be able to choose which take it is matched with.
	//
	if opts.Offer.Maker != key.Public {
		return event.Event{}, market.Match{}, fmt.Errorf(
			"SECURITY FAILURE: market match must be signed by the offer maker",
		)
	}

	if opts.Taker == nil {
		return event.Event{}, market.Match{}, fmt.Errorf("a market match needs the take it seals")
	}

	if opts.Taker.OfferID != opts.Offer.EventID {
		return event.Event{}, market.Match{}, fmt.Errorf(
			"the take does not reference the offer",
		)
	}

	if opts.Taker.Taker != opts.TakeEvent.Pubkey {
		return event.Event{}, market.Match{}, fmt.Errorf(
			"the take is not signed by its taker",
		)
	}

	if len(opts.SwapID) != 12 {
		return event.Event{}, market.Match{}, fmt.Errorf("atomic swap ID must be 12 hex characters")
	}

	createdAt := opts.CreatedAt

	if createdAt == 0 {
		createdAt = time.Now().Unix()
	}

	if createdAt < opts.TakeEvent.CreatedAt {
		return event.Event{}, market.Match{}, fmt.Errorf(
			"market match predates the take",
		)
	}

	if createdAt >= opts.Offer.ExpiresAt {
		return event.Event{}, market.Match{}, fmt.Errorf(
			"market match was created at or after offer expiry",
		)
	}

	match := market.Match{
		Version:     opts.Offer.Version,
		OfferID:     opts.Offer.EventID,
		TakeID:      opts.TakeEvent.ID,
		SwapID:      opts.SwapID,
		Maker:       opts.Offer.Maker,
		Taker:       opts.Taker.Taker,
		BlakeAmount: opts.BlakeAmount,
		BTCSats:     opts.BTCSats,
		CreatedAt:   createdAt,
	}

	message := map[string]any{
		"type":         "market-match",
		"version":      opts.Offer.Version,
		"offer_id":     opts.Offer.EventID,
		"take_id":      opts.TakeEvent.ID,
		"swap_id":      opts.SwapID,
		"maker":        opts.Offer.Maker,
		"taker":        opts.Taker.Taker,
		"base":         "BLAKE",
		"quote":        "BTC",
		"blake_amount": countString(opts.BlakeAmount),
		"btc_sats":     opts.BTCSats,
		"created_at":   createdAt,
	}

	tags := [][]string{
		{"t", MatchTag},
		{"e", opts.Offer.EventID},
		{"e", opts.TakeEvent.ID},
		{"p", opts.Taker.Taker},
		{"d", opts.SwapID},
	}

	if opts.Offer.Version == 2 {
		match.Direction = opts.Offer.Direction
		match.MakerRole = opts.Offer.MakerRole
		match.TakerRole = opts.Offer.TakerRole

		message["direction"] = string(opts.Offer.Direction)
		message["maker_role"] = opts.Offer.MakerRole
		message["taker_role"] = opts.Offer.TakerRole

		tags = append(tags, []string{"direction", string(opts.Offer.Direction)})
	} else if opts.Offer.Version != 1 {
		return event.Event{}, market.Match{}, fmt.Errorf(
			"cannot build a match for unsupported market offer version %d",
			opts.Offer.Version,
		)
	}

	canonical, err := event.CanonicalJSON(message)

	if err != nil {
		return event.Event{}, market.Match{}, err
	}

	built, err := signEvent(key, canonical, tags, createdAt)

	if err != nil {
		return event.Event{}, market.Match{}, err
	}

	return built, match, nil
}

// ParsedMatch is a verified match with the facts a caller needs.
type ParsedMatch struct {
	market.Match
	// EventID is the match event's id.
	EventID string
	// Maker is the signer, which the protocol requires to be the offer's maker.
	MakerPubkey string
}

// ParseMatch verifies a match against the offer and take it references.
//
// The whole point of the match is that it introduces no new claims: everything in it
// can be re-derived from the two events it names, and this function re-derives all of
// it. A match that disagrees with either event on any fact is not evidence of a
// match — it is a claim about one, and this protocol does not accept claims.
func ParseMatch(
	e event.Event, offerEvent, takeEvent event.Event, now time.Time,
) (*ParsedMatch, error) {
	if err := event.Verify(e, now); err != nil {
		return nil, err
	}

	if e.Kind != event.KindTextNote {
		return nil, fmt.Errorf("market match Nostr kind is not 1")
	}

	if !e.HasTag("t", MatchTag) {
		return nil, fmt.Errorf("market match is missing expected Nostr market-match tag")
	}

	var message map[string]any

	if err := json.Unmarshal([]byte(e.Content), &message); err != nil {
		return nil, fmt.Errorf("market match contains invalid JSON: %w", err)
	}

	match, err := market.ParseMatch(message)

	if err != nil {
		return nil, err
	}

	if match.CreatedAt != e.CreatedAt {
		return nil, fmt.Errorf("market match timestamp mismatch")
	}

	// The two events it references must be the two events given, and both must be
	// named by signed `e` tags.
	if match.OfferID != offerEvent.ID {
		return nil, fmt.Errorf("market match references a different offer")
	}

	if match.TakeID != takeEvent.ID {
		return nil, fmt.Errorf("market match references a different take")
	}

	references := map[string]bool{}

	for _, tag := range e.Tags {
		if len(tag) >= 2 && tag[0] == "e" {
			references[tag[1]] = true
		}
	}

	if !references[offerEvent.ID] {
		return nil, fmt.Errorf("market match does not reference the offer")
	}

	if !references[takeEvent.ID] {
		return nil, fmt.Errorf("market match does not reference the take")
	}

	if reference, ok := e.TagValue("d"); !ok || reference != match.SwapID {
		return nil, fmt.Errorf("market match swap_id does not match Nostr d-tag")
	}

	// The offer and take are re-validated here, not trusted because a caller
	// supplied them.
	offer, err := ParseOffer(offerEvent, now)

	if err != nil {
		return nil, err
	}

	if e.Pubkey != offer.Maker {
		return nil, fmt.Errorf(
			"SECURITY FAILURE: market match was not signed by the original maker",
		)
	}

	if match.Maker != offer.Maker {
		return nil, fmt.Errorf("market match maker mismatch")
	}

	take, err := ParseTake(takeEvent, now)

	if err != nil {
		return nil, err
	}

	if take.OfferID != offer.EventID {
		return nil, fmt.Errorf("the take does not reference the offer")
	}

	if match.Taker != take.Taker {
		return nil, fmt.Errorf("market match taker disagrees with referenced take")
	}

	if !e.HasTag("p", take.Taker) {
		return nil, fmt.Errorf("market match does not tag taker")
	}

	if take.Direction != offer.Direction {
		return nil, fmt.Errorf("market match direction disagrees with referenced take")
	}

	if match.Version == 2 {
		if take.MakerRole != offer.MakerRole || take.TakerRole != offer.TakerRole {
			return nil, fmt.Errorf("market match roles disagree with referenced take")
		}

		if match.MakerRole != offer.MakerRole || match.TakerRole != offer.TakerRole {
			return nil, fmt.Errorf("market match role mismatch")
		}

		if direction, ok := e.TagValue("direction"); !ok || direction != string(offer.Direction) {
			return nil, fmt.Errorf("market match direction does not match Nostr direction tag")
		}
	}

	// The economics must be the offer's, not the match's own opinion of them.
	if match.BlakeAmount != offer.BlakeAmount {
		return nil, fmt.Errorf("match changed BLAKE terms")
	}

	if match.BTCSats != offer.BTCSats {
		return nil, fmt.Errorf("match changed BTC terms")
	}

	// A maker cannot seal a take before it exists, and cannot seal one for an offer
	// that has already expired.
	if e.CreatedAt < takeEvent.CreatedAt {
		return nil, fmt.Errorf("market match predates the take")
	}

	if e.CreatedAt >= offer.ExpiresAt {
		return nil, fmt.Errorf("market match was created at or after offer expiry")
	}

	return &ParsedMatch{
		Match: match, EventID: e.ID, MakerPubkey: e.Pubkey,
	}, nil
}
