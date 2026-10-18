package marketflow

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/keys"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/nostr"
)

// CancelTag and TakeTag identify the market's reply events.
const (
	CancelTag = "blake-swap-market-cancel"
	TakeTag   = "blake-swap-market-take"
)

// BuildCancelOptions is what a maker decides when withdrawing an offer.
type BuildCancelOptions struct {
	// OfferID is the event id of the offer being withdrawn.
	OfferID string
	// CreatedAt pins the event time. Zero means now.
	CreatedAt int64
}

// BuildCancel builds and signs a market cancellation.
//
// A cancel names the offer by its event id rather than restating its terms, so
// there is exactly one way to say "that advertisement, not this one" — and a reader
// can check the reference against the signed `e` tag instead of trusting content.
func BuildCancel(key *nostr.Key, opts BuildCancelOptions) (event.Event, market.Cancel, error) {
	if key == nil {
		return event.Event{}, market.Cancel{}, fmt.Errorf("a market cancel needs an identity")
	}

	if len(opts.OfferID) != 64 {
		return event.Event{}, market.Cancel{}, fmt.Errorf(
			"offer_id must be a 64-character Nostr event ID",
		)
	}

	createdAt := opts.CreatedAt

	if createdAt == 0 {
		createdAt = time.Now().Unix()
	}

	message := map[string]any{
		"type":       "market-cancel",
		"version":    1,
		"offer_id":   opts.OfferID,
		"created_at": createdAt,
	}

	canonical, err := event.CanonicalJSON(message)

	if err != nil {
		return event.Event{}, market.Cancel{}, err
	}

	tags := [][]string{
		{"t", CancelTag},
		{"e", opts.OfferID},
	}

	built, err := signEvent(key, canonical, tags, createdAt)

	if err != nil {
		return event.Event{}, market.Cancel{}, err
	}

	return built, market.Cancel{Version: 1, OfferID: opts.OfferID, CreatedAt: createdAt}, nil
}

// ParseCancel verifies a cancellation and returns what it cancels.
func ParseCancel(e event.Event, now time.Time) (*market.Cancel, error) {
	if err := event.Verify(e, now); err != nil {
		return nil, err
	}

	if e.Kind != event.KindTextNote {
		return nil, fmt.Errorf("market cancel Nostr kind is not 1")
	}

	if !e.HasTag("t", CancelTag) {
		return nil, fmt.Errorf("market cancel is missing expected Nostr cancel tag")
	}

	var message map[string]any

	if err := json.Unmarshal([]byte(e.Content), &message); err != nil {
		return nil, fmt.Errorf("market cancel content is invalid JSON: %w", err)
	}

	cancel, err := market.ParseCancel(message)

	if err != nil {
		return nil, err
	}

	if cancel.CreatedAt != e.CreatedAt {
		return nil, fmt.Errorf("market cancel created_at mismatch")
	}

	//
	// The signed `e` tag and the content must name the same offer. Without this a
	// cancel could carry one id in its content and another in its tag, and which
	// offer it withdraws would depend on which field the reader happened to trust.
	//
	if referenced, ok := e.TagValue("e"); !ok || referenced != cancel.OfferID {
		return nil, fmt.Errorf("market cancel references a different offer than its tag")
	}

	return &cancel, nil
}

// BuildTakeOptions is what a taker decides when accepting an offer.
type BuildTakeOptions struct {
	// Offer is the parsed offer being taken.
	Offer ListedOffer
	// CreatedAt pins the event time. Zero means now.
	CreatedAt int64
	// Nonce is 8 random bytes as 16 hex characters. Empty means generated.
	Nonce string
}

// BuildTake builds and signs a take for one offer.
//
// The roles in a version 2 take are *derived* from the offer's direction rather than
// chosen by the taker: a taker that could name its own role could claim the side that
// funds later.
func BuildTake(key *nostr.Key, opts BuildTakeOptions) (event.Event, market.Take, error) {
	if key == nil {
		return event.Event{}, market.Take{}, fmt.Errorf("a market take needs an identity")
	}

	if opts.Offer.EventID == "" {
		return event.Event{}, market.Take{}, fmt.Errorf("a take needs an offer")
	}

	if opts.Offer.Version != 1 && opts.Offer.Version != 2 {
		return event.Event{}, market.Take{}, fmt.Errorf(
			"cannot build a take for unsupported market offer version %d",
			opts.Offer.Version,
		)
	}

	createdAt := opts.CreatedAt

	if createdAt == 0 {
		createdAt = time.Now().Unix()
	}

	nonce := opts.Nonce

	if nonce == "" {
		generated, err := randomNonce()

		if err != nil {
			return event.Event{}, market.Take{}, err
		}

		nonce = generated
	}

	take := market.Take{
		Version:     opts.Offer.Version,
		OfferID:     opts.Offer.EventID,
		Maker:       opts.Offer.Maker,
		Taker:       key.Public,
		BlakeAmount: opts.Offer.BlakeAmount,
		BTCSats:     opts.Offer.BTCSats,
		CreatedAt:   createdAt,
		Nonce:       nonce,
	}

	message := map[string]any{
		"type":         "market-take",
		"version":      opts.Offer.Version,
		"offer_id":     opts.Offer.EventID,
		"maker":        opts.Offer.Maker,
		"base":         "BLAKE",
		"quote":        "BTC",
		"blake_amount": countString(opts.Offer.BlakeAmount),
		"btc_sats":     opts.Offer.BTCSats,
		"created_at":   createdAt,
		"nonce":        nonce,
	}

	tags := [][]string{
		{"t", TakeTag},
		{"e", opts.Offer.EventID},
		{"p", opts.Offer.Maker},
	}

	if opts.Offer.Version == 2 {
		take.Direction = opts.Offer.Direction
		take.MakerRole = opts.Offer.MakerRole
		take.TakerRole = opts.Offer.TakerRole

		message["direction"] = string(opts.Offer.Direction)
		message["taker"] = key.Public
		message["maker_role"] = opts.Offer.MakerRole
		message["taker_role"] = opts.Offer.TakerRole

		tags = append(tags, []string{"direction", string(opts.Offer.Direction)})
	}

	canonical, err := event.CanonicalJSON(message)

	if err != nil {
		return event.Event{}, market.Take{}, err
	}

	built, err := signEvent(key, canonical, tags, createdAt)

	if err != nil {
		return event.Event{}, market.Take{}, err
	}

	return built, take, nil
}

// ParseTake verifies a take and returns what it accepts.
func ParseTake(e event.Event, now time.Time) (*market.Take, error) {
	if err := event.Verify(e, now); err != nil {
		return nil, err
	}

	if e.Kind != event.KindTextNote {
		return nil, fmt.Errorf("market take Nostr kind is not 1")
	}

	if !e.HasTag("t", TakeTag) {
		return nil, fmt.Errorf("market take is missing expected Nostr take tag")
	}

	var message map[string]any

	if err := json.Unmarshal([]byte(e.Content), &message); err != nil {
		return nil, fmt.Errorf("market take content is invalid JSON: %w", err)
	}

	take, err := market.ParseTake(message)

	if err != nil {
		return nil, err
	}

	if take.CreatedAt != e.CreatedAt {
		return nil, fmt.Errorf("market take created_at mismatch")
	}

	if referenced, ok := e.TagValue("e"); !ok || referenced != take.OfferID {
		return nil, fmt.Errorf("market take references a different offer than its tag")
	}

	if maker, ok := e.TagValue("p"); !ok || maker != take.Maker {
		return nil, fmt.Errorf("market take names a different maker than its tag")
	}

	if take.Version == 2 {
		if direction, ok := e.TagValue("direction"); !ok || direction != string(take.Direction) {
			return nil, fmt.Errorf("market take direction does not match Nostr direction tag")
		}

		//
		// The roles are a function of the direction, and a take that names
		// different ones is asking for a different trade than the offer it accepts.
		// Checking the direction alone would let a take claim, say, that its taker
		// is the one who funds BLAKE.
		//
		makerRole, takerRole, err := take.Direction.ProtocolRoles()

		if err != nil {
			return nil, err
		}

		if take.MakerRole != makerRole {
			return nil, fmt.Errorf("market take maker role mismatch")
		}

		if take.TakerRole != takerRole {
			return nil, fmt.Errorf("market take taker role mismatch")
		}
	}

	// The taker is the event's author, not a field the author can choose.
	if take.Version == 2 && take.Taker != e.Pubkey {
		return nil, fmt.Errorf("market take taker is not the event author")
	}

	return &take, nil
}

// signEvent sets the time, signs and bounds a coordination-shaped event.
func signEvent(
	key *nostr.Key, content []byte, tags [][]string, createdAt int64,
) (event.Event, error) {
	built, err := event.Build(key.Public, event.WithContent(content), event.WithTags(tags))

	if err != nil {
		return event.Event{}, err
	}

	built.CreatedAt = createdAt

	id, err := built.ComputeID()

	if err != nil {
		return event.Event{}, err
	}

	built.ID = id

	signer, err := keys.FromSecretHex(key.Secret)

	if err != nil {
		return event.Event{}, err
	}

	if err := event.Sign(&built, signer); err != nil {
		return event.Event{}, err
	}

	return built, nil
}

// countString renders a BLAKE amount the way the wire spells it.
func countString(value amount.Blake) string { return value.Format() }
