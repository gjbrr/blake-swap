// Package marketflow carries the market conversation: the signed events that
// announce, take, match and cancel a trade.
//
// It sits above pkg/market, which owns the message vocabulary and the rules a
// message must satisfy, and below the engine, which decides what to do with a
// match. The split matters because the two halves fail differently: a message that
// violates the vocabulary is malformed by construction, while an event that violates
// its tags is a forgery or a mix-up — and the second kind is only detectable once the
// signature and the tags are checked against each other.
package marketflow

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/keys"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/nostr"
)

// MarketTag identifies an event as belonging to this protocol's market.
const MarketTag = "blake-swap-market"

// BuildOfferOptions is what a maker decides.
type BuildOfferOptions struct {
	// BlakeAmount is the trade size in base units.
	BlakeAmount amount.Blake
	// BTCSats is the trade size in satoshis.
	BTCSats int64
	// Minutes is the offer's lifetime.
	Minutes int64
	// Direction is which asset the maker gives.
	Direction market.Direction
	// CreatedAt pins the event time. Zero means now.
	CreatedAt int64
	// Nonce is 8 random bytes as 16 hex characters. Empty means generated.
	Nonce string
}

// BuildOffer builds and signs a version 2 market offer event.
//
// Version 2 makes the economic direction explicit, which version 1 could not: v1
// assumed the maker was selling BLAKE, so a taker had to infer the roles from a
// message that never said them.
func BuildOffer(key *nostr.Key, opts BuildOfferOptions) (event.Event, market.Offer, error) {
	if key == nil {
		return event.Event{}, market.Offer{}, fmt.Errorf("a market offer needs an identity")
	}

	if opts.BlakeAmount <= 0 {
		return event.Event{}, market.Offer{}, fmt.Errorf("BLAKE amount must be positive")
	}

	if opts.BTCSats <= 0 {
		return event.Event{}, market.Offer{}, fmt.Errorf("BTC sat amount must be positive")
	}

	if opts.Minutes <= 0 {
		return event.Event{}, market.Offer{}, fmt.Errorf("offer lifetime must be positive")
	}

	if _, _, err := opts.Direction.ProtocolRoles(); err != nil {
		return event.Event{}, market.Offer{}, err
	}

	createdAt := opts.CreatedAt

	if createdAt == 0 {
		createdAt = time.Now().Unix()
	}

	expiresAt := createdAt + opts.Minutes*60

	nonce := opts.Nonce

	if nonce == "" {
		generated, err := randomNonce()

		if err != nil {
			return event.Event{}, market.Offer{}, err
		}

		nonce = generated
	}

	message := map[string]any{
		"type":         "market-offer",
		"version":      2,
		"base":         "BLAKE",
		"quote":        "BTC",
		"direction":    string(opts.Direction),
		"blake_amount": opts.BlakeAmount.Decimal().String(),
		"btc_sats":     opts.BTCSats,
		"created_at":   createdAt,
		"expires_at":   expiresAt,
		"nonce":        nonce,
	}

	canonical, err := event.CanonicalJSON(message)

	if err != nil {
		return event.Event{}, market.Offer{}, err
	}

	tags := [][]string{
		{"t", MarketTag},
		{"base", "BLAKE"},
		{"quote", "BTC"},
		{"direction", string(opts.Direction)},
	}

	built, err := event.Build(key.Public, event.WithContent(canonical), event.WithTags(tags))

	if err != nil {
		return event.Event{}, market.Offer{}, err
	}

	// The event time and the message time must be the same number: a parser that
	// compares them is what stops a relay from redating a signed advertisement.
	built.CreatedAt = createdAt

	id, err := built.ComputeID()

	if err != nil {
		return event.Event{}, market.Offer{}, err
	}

	built.ID = id

	signer, err := keys.FromSecretHex(key.Secret)

	if err != nil {
		return event.Event{}, market.Offer{}, err
	}

	if err := event.Sign(&built, signer); err != nil {
		return event.Event{}, market.Offer{}, err
	}

	offer := market.Offer{
		Version:     2,
		Direction:   opts.Direction,
		BlakeAmount: opts.BlakeAmount,
		BTCSats:     opts.BTCSats,
		CreatedAt:   createdAt,
		ExpiresAt:   expiresAt,
		Nonce:       nonce,
	}

	return built, offer, nil
}

// ListedOffer is a parsed offer with the event facts a caller needs.
type ListedOffer struct {
	market.Offer
	// EventID is the signed event's id.
	EventID string
	// Maker is the signer's x-only public key.
	Maker string
	// MakerRole and TakerRole are the protocol roles the direction implies.
	MakerRole string
	TakerRole string
}

// Expired reports whether the offer's lifetime has passed.
func (o ListedOffer) ExpiredAt(now int64) bool { return o.Offer.Expired(now) }

// ParseOffer verifies one market offer event and returns the offer it carries.
//
// Every check here is about the event *proving* what it claims: the signature
// proves who wrote it, and the tags must agree with the content. A valid signature
// proves authorship, not that the event belongs to this protocol or that its
// direction tag matches its direction field — and a mismatch there is how a
// mis-signed advertisement would change which side funds which chain.
func ParseOffer(e event.Event, now time.Time) (*ListedOffer, error) {
	if err := event.Verify(e, now); err != nil {
		return nil, err
	}

	if e.Kind != event.KindTextNote {
		return nil, fmt.Errorf("market offer Nostr kind is not 1")
	}

	if !e.HasTag("t", MarketTag) {
		return nil, fmt.Errorf("market offer is missing expected Nostr market tag")
	}

	var message map[string]any

	if err := json.Unmarshal([]byte(e.Content), &message); err != nil {
		return nil, fmt.Errorf("market offer content is invalid JSON: %w", err)
	}

	offer, err := market.ParseOffer(message)

	if err != nil {
		return nil, err
	}

	if offer.CreatedAt != e.CreatedAt {
		return nil, fmt.Errorf("market offer created_at mismatch")
	}

	if offer.Version == 2 {
		if value, ok := e.TagValue("direction"); !ok || value != string(offer.Direction) {
			return nil, fmt.Errorf("market offer direction does not match Nostr direction tag")
		}
	}

	maker, taker, err := offer.Direction.ProtocolRoles()

	if err != nil {
		return nil, err
	}

	return &ListedOffer{
		Offer:     offer,
		EventID:   e.ID,
		Maker:     e.Pubkey,
		MakerRole: maker,
		TakerRole: taker,
	}, nil
}

// randomNonce returns 8 random bytes as hex.
//
// The nonce makes two otherwise identical advertisements distinct events, which the
// protocol needs because an event's id is its content: re-publishing the same trade
// at the same second would otherwise be the same event.
func randomNonce() (string, error) {
	var buffer [8]byte

	if _, err := rand.Read(buffer[:]); err != nil {
		return "", err
	}

	return hex.EncodeToString(buffer[:]), nil
}
