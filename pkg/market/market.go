// Package market is the marketplace message vocabulary and its structural
// rules.
//
// It is the port of the market direction mapping and the message shapes in the
// Python engine. The engine's event-level parsers also verify Nostr signatures
// and tags; those belong to pkg/event and pkg/nostr. What lives here is
// everything that can be decided from the message alone, plus the one thing
// that is easy to get wrong: the inversion between market roles (maker/taker)
// and protocol roles (alice/bob).
//
// Protocol Alice always funds BLAKE; protocol Bob always funds BTC. A
// `blake-for-btc` maker is therefore Alice, and a `btc-for-blake` maker is Bob.
// The original single-direction implementation hardcoded the first mapping and
// mislabelled every reverse negotiation.
package market

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/decimal"
)

// Direction is which asset the maker is giving.
type Direction string

// The two market directions.
const (
	// BlakeForBTC means the maker gives BLAKE and wants BTC.
	BlakeForBTC Direction = "blake-for-btc"
	// BTCForBlake means the maker gives BTC and wants BLAKE.
	BTCForBlake Direction = "btc-for-blake"
)

// Directions is every accepted direction.
var Directions = []Direction{BlakeForBTC, BTCForBlake}

// ParseDirection validates a direction string.
func ParseDirection(s string) (Direction, error) {
	for _, d := range Directions {
		if string(d) == s {
			return d, nil
		}
	}

	return "", fmt.Errorf("unknown market direction: %q", s)
}

// ProtocolRoles returns the protocol role each market role binds.
//
// This is the inversion the engine must not get wrong: the maker is the party
// giving the direction's asset, and protocol Alice is always the BLAKE side.
func (d Direction) ProtocolRoles() (maker, taker string, err error) {
	switch d {
	case BlakeForBTC:
		return "alice", "bob", nil
	case BTCForBlake:
		return "bob", "alice", nil
	default:
		return "", "", fmt.Errorf("unknown market direction: %q", d)
	}
}

// GiveWant renders what each side gives and wants, for display.
func (d Direction) GiveWant(blake amount.Blake, sats amount.Sats) (give, want string, err error) {
	blakeText := blake.Format()
	satsText := withThousands(int64(sats))

	switch d {
	case BlakeForBTC:
		return blakeText + " BLAKE", satsText + " sats", nil
	case BTCForBlake:
		return satsText + " sats", blakeText + " BLAKE", nil
	default:
		return "", "", fmt.Errorf("unknown market direction: %q", d)
	}
}

// withThousands formats an integer with comma separators, as Python's `:,`
// does.
func withThousands(n int64) string {
	negative := n < 0

	if negative {
		n = -n
	}

	digits := fmt.Sprintf("%d", n)
	var parts []string

	for len(digits) > 3 {
		parts = append([]string{digits[len(digits)-3:]}, parts...)
		digits = digits[:len(digits)-3]
	}

	parts = append([]string{digits}, parts...)
	out := strings.Join(parts, ",")

	if negative {
		return "-" + out
	}

	return out
}

// Base and Quote are canonical: the economic pair does not change with the
// direction.
const (
	Base  = "BLAKE"
	Quote = "BTC"
)

// forbidden are settlement-bearing fields that must never appear in a market
// message: the market negotiates terms, not chain data.
var forbidden = map[string]bool{
	"secret_hex":         true,
	"secret_hash":        true,
	"blake_alice_pub":    true,
	"blake_bob_pub":      true,
	"btc_alice_pub":      true,
	"btc_bob_pub":        true,
	"blake_funding_txid": true,
	"btc_funding_txid":   true,
}

func checkForbidden(message map[string]any) error {
	var found []string

	for field := range message {
		if forbidden[field] {
			found = append(found, field)
		}
	}

	if len(found) > 0 {
		sort.Strings(found)

		return fmt.Errorf("market message contains forbidden settlement data: %s",
			strings.Join(found, ", "))
	}

	return nil
}

func checkPair(message map[string]any) error {
	base, _ := message["base"].(string)

	if base != Base {
		return fmt.Errorf("market message base is not BLAKE")
	}

	quote, _ := message["quote"].(string)

	if quote != Quote {
		return fmt.Errorf("market message quote is not BTC")
	}

	return nil
}

// Offer is a `market-offer` message.
type Offer struct {
	// Version is 1 (implicit blake-for-btc, `side: sell`) or 2 (explicit
	// direction).
	Version int
	// Side is only part of version 1.
	Side string

	Direction Direction

	BlakeAmount amount.Blake
	BTCSats     int64

	CreatedAt int64
	ExpiresAt int64
	Nonce     string
}

// Kind is the message type string.
func (o Offer) Kind() string { return "market-offer" }

// Expired reports whether the offer's lifetime has passed at now (unix seconds).
func (o Offer) Expired(now int64) bool { return now >= o.ExpiresAt }

// ParseOffer reads an offer message, validating everything decidable from the
// message alone.
func ParseOffer(message map[string]any) (Offer, error) {
	if kind, _ := message["type"].(string); kind != "market-offer" {
		return Offer{}, fmt.Errorf("event is not a market offer")
	}

	version, err := intField(message, "version")

	if err != nil {
		return Offer{}, err
	}

	if version != 1 && version != 2 {
		return Offer{}, fmt.Errorf("unsupported market offer version")
	}

	if err := checkPair(message); err != nil {
		return Offer{}, err
	}

	if err := checkForbidden(message); err != nil {
		return Offer{}, err
	}

	offer := Offer{Version: int(version)}

	switch version {
	case 1:
		// Version 1 had one direction, named by `side`, and no direction
		// field. The canonical direction is therefore implied.
		side, _ := message["side"].(string)

		if side != "sell" {
			return Offer{}, fmt.Errorf("market offer side is not sell")
		}

		offer.Side = side
		offer.Direction = BlakeForBTC

	case 2:
		direction, _ := message["direction"].(string)

		parsed, err := ParseDirection(direction)

		if err != nil {
			return Offer{}, fmt.Errorf("market offer has invalid direction")
		}

		offer.Direction = parsed
	}

	blakeText, ok := message["blake_amount"].(string)

	if !ok {
		return Offer{}, fmt.Errorf("market offer contains invalid economic fields")
	}

	blake, err := amount.ParseBlake(blakeText)

	if err != nil {
		//
		// A zero or negative amount is a *value* problem and has its own message,
		// which is what the reference reports; anything else is a shape problem.
		//
		if value, decimalErr := decimal.Parse(blakeText); decimalErr == nil && value.Sign() <= 0 {
			return Offer{}, fmt.Errorf("market offer BLAKE amount must be positive")
		}

		return Offer{}, fmt.Errorf("market offer contains invalid economic fields")
	}

	sats, err := intField(message, "btc_sats")

	if err != nil {
		return Offer{}, fmt.Errorf("market offer contains invalid economic fields")
	}

	created, err := intField(message, "created_at")

	if err != nil {
		return Offer{}, fmt.Errorf("market offer contains invalid economic fields")
	}

	expires, err := intField(message, "expires_at")

	if err != nil {
		return Offer{}, fmt.Errorf("market offer contains invalid economic fields")
	}

	if blake <= 0 {
		return Offer{}, fmt.Errorf("market offer BLAKE amount must be positive")
	}

	if sats <= 0 {
		return Offer{}, fmt.Errorf("market offer BTC amount must be positive")
	}

	if expires <= created {
		return Offer{}, fmt.Errorf("market offer expiry must be after creation")
	}

	offer.BlakeAmount = blake
	offer.BTCSats = sats
	offer.CreatedAt = created
	offer.ExpiresAt = expires

	if nonce, ok := message["nonce"].(string); ok {
		offer.Nonce = nonce
	}

	return offer, nil
}

// Take is a `market-take` message.
type Take struct {
	Version int

	OfferID string
	Maker   string
	Taker   string

	Direction Direction
	MakerRole string
	TakerRole string

	BlakeAmount amount.Blake
	BTCSats     int64

	CreatedAt int64
	Nonce     string
}

// Kind is the message type string.
func (t Take) Kind() string { return "market-take" }

// ParseTake reads a take message without binding it to an offer.
func ParseTake(message map[string]any) (Take, error) {
	if kind, _ := message["type"].(string); kind != "market-take" {
		return Take{}, fmt.Errorf("event is not a market take")
	}

	version, err := intField(message, "version")

	if err != nil {
		return Take{}, err
	}

	if version != 1 && version != 2 {
		return Take{}, fmt.Errorf("unsupported market take version")
	}

	if err := checkPair(message); err != nil {
		return Take{}, err
	}

	if err := checkForbidden(message); err != nil {
		return Take{}, err
	}

	take := Take{Version: int(version)}

	take.OfferID, _ = message["offer_id"].(string)

	if take.OfferID == "" {
		return Take{}, fmt.Errorf("market take references no offer")
	}

	take.Maker, _ = message["maker"].(string)
	take.Taker, _ = message["taker"].(string)

	blakeText, _ := message["blake_amount"].(string)

	blake, err := amount.ParseBlake(blakeText)

	if err != nil {
		return Take{}, fmt.Errorf("market take contains invalid economic fields")
	}

	sats, err := intField(message, "btc_sats")

	if err != nil {
		return Take{}, fmt.Errorf("market take contains invalid economic fields")
	}

	created, err := intField(message, "created_at")

	if err != nil {
		return Take{}, fmt.Errorf("market take contains invalid economic fields")
	}

	take.BlakeAmount = blake
	take.BTCSats = sats
	take.CreatedAt = created

	if nonce, ok := message["nonce"].(string); ok {
		take.Nonce = nonce
	}

	if version == 2 {
		direction, _ := message["direction"].(string)

		parsed, err := ParseDirection(direction)

		if err != nil {
			return Take{}, fmt.Errorf("market take has invalid direction")
		}

		take.Direction = parsed
		take.MakerRole, _ = message["maker_role"].(string)
		take.TakerRole, _ = message["taker_role"].(string)
	}

	return take, nil
}

// ValidateAgainstOffer checks a take against the offer it claims to answer.
//
// The negotiation version must move together: a version 2 offer cannot be
// downgraded by answering it with an old version 1 take. Version 2 also commits
// both parties to the direction and the protocol roles derived from it, so a
// taker cannot choose roles independently.
func (t Take) ValidateAgainstOffer(offer Offer, offerID, makerPub string) error {
	if t.Version != offer.Version {
		return fmt.Errorf("market take version does not match market offer version")
	}

	if t.OfferID != offerID {
		return fmt.Errorf("market take references another offer")
	}

	if t.Maker != makerPub {
		return fmt.Errorf("market take maker mismatch")
	}

	if t.BlakeAmount != offer.BlakeAmount {
		return fmt.Errorf("market take changed BLAKE terms")
	}

	if t.BTCSats != offer.BTCSats {
		return fmt.Errorf("market take changed BTC terms")
	}

	if t.Version == 2 {
		if t.Direction != offer.Direction {
			return fmt.Errorf("market take changed direction")
		}

		makerRole, takerRole, err := offer.Direction.ProtocolRoles()

		if err != nil {
			return err
		}

		if t.MakerRole != makerRole {
			return fmt.Errorf("market take maker role mismatch")
		}

		if t.TakerRole != takerRole {
			return fmt.Errorf("market take taker role mismatch")
		}

		if t.Taker == "" {
			return fmt.Errorf("market take taker identity is missing")
		}
	}

	return nil
}

// Match is a `market-match` message: the terminal event that binds an offer, a
// take and a swap.
type Match struct {
	Version int

	OfferID string
	TakeID  string
	SwapID  string

	Maker string
	Taker string

	Direction Direction
	MakerRole string
	TakerRole string

	BlakeAmount amount.Blake
	BTCSats     int64

	CreatedAt int64
}

// Kind is the message type string.
func (m Match) Kind() string { return "market-match" }

// ParseMatch reads a match message.
func ParseMatch(message map[string]any) (Match, error) {
	if kind, _ := message["type"].(string); kind != "market-match" {
		return Match{}, fmt.Errorf("event is not a market match")
	}

	version, err := intField(message, "version")

	if err != nil {
		return Match{}, err
	}

	if version != 1 && version != 2 {
		return Match{}, fmt.Errorf("unsupported market match version")
	}

	if err := checkPair(message); err != nil {
		return Match{}, err
	}

	if err := checkForbidden(message); err != nil {
		return Match{}, err
	}

	match := Match{Version: int(version)}

	for field, target := range map[string]*string{
		"offer_id": &match.OfferID,
		"take_id":  &match.TakeID,
		"swap_id":  &match.SwapID,
		"maker":    &match.Maker,
		"taker":    &match.Taker,
	} {
		value, _ := message[field].(string)

		if value == "" {
			return Match{}, fmt.Errorf("market match is missing %s", field)
		}

		*target = value
	}

	blakeText, _ := message["blake_amount"].(string)

	blake, err := amount.ParseBlake(blakeText)

	if err != nil {
		return Match{}, fmt.Errorf("market match contains invalid economic fields")
	}

	sats, err := intField(message, "btc_sats")

	if err != nil {
		return Match{}, fmt.Errorf("market match contains invalid economic fields")
	}

	created, err := intField(message, "created_at")

	if err != nil {
		return Match{}, fmt.Errorf("market match contains invalid economic fields")
	}

	match.BlakeAmount = blake
	match.BTCSats = sats
	match.CreatedAt = created

	if version == 2 {
		direction, _ := message["direction"].(string)

		parsed, err := ParseDirection(direction)

		if err != nil {
			return Match{}, fmt.Errorf("market match has invalid direction")
		}

		match.Direction = parsed
		match.MakerRole, _ = message["maker_role"].(string)
		match.TakerRole, _ = message["taker_role"].(string)
	}

	return match, nil
}

// Cancel is a `market-cancel` message.
type Cancel struct {
	Version   int
	OfferID   string
	CreatedAt int64
}

// Kind is the message type string.
func (c Cancel) Kind() string { return "market-cancel" }

// ParseCancel reads a cancel message.
func ParseCancel(message map[string]any) (Cancel, error) {
	if kind, _ := message["type"].(string); kind != "market-cancel" {
		return Cancel{}, fmt.Errorf("event is not a market cancel")
	}

	version, err := intField(message, "version")

	if err != nil {
		return Cancel{}, err
	}

	if version != 1 && version != 2 {
		return Cancel{}, fmt.Errorf("unsupported market cancel version")
	}

	offerID, _ := message["offer_id"].(string)

	if offerID == "" {
		return Cancel{}, fmt.Errorf("market cancel references no offer")
	}

	created, err := intField(message, "created_at")

	if err != nil {
		return Cancel{}, fmt.Errorf("market cancel contains invalid fields")
	}

	return Cancel{Version: int(version), OfferID: offerID, CreatedAt: created}, nil
}

// intField reads a JSON integer that may have arrived as float64 (plain
// json.Unmarshal) or json.Number (decoder with UseNumber).
func intField(message map[string]any, field string) (int64, error) {
	switch value := message[field].(type) {
	case float64:
		if math.Trunc(value) != value {
			return 0, fmt.Errorf("%s must be an integer", field)
		}

		return int64(value), nil
	case int64:
		return value, nil
	case int:
		return int64(value), nil
	case interface{ Int64() (int64, error) }:
		return value.Int64()
	default:
		return 0, fmt.Errorf("%s must be an integer", field)
	}
}
