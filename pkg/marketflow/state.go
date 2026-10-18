package marketflow

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/gjbrr/blake-swap/pkg/event"
)

// OfferState is what a marketplace can say about one advertisement.
type OfferState string

// The four states. MATCHED and CANCELLED are terminal; OPEN and EXPIRED are the
// states of an advertisement that has not been sealed.
const (
	StateOpen      OfferState = "OPEN"
	StateMatched   OfferState = "MATCHED"
	StateCancelled OfferState = "CANCELLED"
	StateExpired   OfferState = "EXPIRED"
)

// StateResult is the derived state of one offer.
type StateResult struct {
	State    OfferState
	Offer    ListedOffer
	CancelID string
	MatchID  string
}

// CancellationApplies reports whether an event cancels this exact offer.
//
// It answers *false* rather than erroring for an event that is not a cancel: a
// marketplace is a stream of events from many offers, and "this one is not about
// that offer" is the normal case, not a failure. Authenticity is still required —
// an unverifiable event is not evidence of anything.
func CancellationApplies(e event.Event, offerID, maker string, now time.Time) bool {
	if err := event.Verify(e, now); err != nil {
		return false
	}

	if e.Kind != event.KindTextNote {
		return false
	}

	if !e.HasTag("t", CancelTag) {
		return false
	}

	var message map[string]any

	if err := json.Unmarshal([]byte(e.Content), &message); err != nil {
		return false
	}

	if message["type"] != "market-cancel" {
		return false
	}

	if version, ok := message["version"]; !ok || toInt(version) != 1 {
		return false
	}

	if createdAt, ok := message["created_at"]; !ok || toInt(createdAt) != e.CreatedAt {
		return false
	}

	if message["offer_id"] != offerID {
		return false
	}

	if !e.HasTag("e", offerID) {
		return false
	}

	//
	// Only the signer of the original offer may cancel it. A cancellation signed by
	// anyone else is a valid event about nothing.
	//
	return e.Pubkey == maker
}

// terminal is one candidate terminal event.
type terminal struct {
	createdAt int64
	id        string
	state     OfferState
	matchID   string
	cancelID  string
}

// DeriveState reports what is true about one offer, given the market events seen.
//
// The rule that matters is that the states are decided from *evidence*, not from
// timing: a match or a cancellation that happened while the offer was live makes it
// MATCHED or CANCELLED forever, however much wall-clock time has passed since — the
// marketplace's history is not rewritten by expiry. A maker-signed but economically
// inconsistent match is not evidence of anything, so it cannot hide a live offer.
func DeriveState(
	offerEvent event.Event, marketEvents []event.Event, now int64,
) (*StateResult, error) {
	if now == 0 {
		now = time.Now().Unix()
	}

	asOf := time.Unix(now, 0)

	offer, err := ParseOffer(offerEvent, asOf)

	if err != nil {
		return nil, err
	}

	byID := map[string]event.Event{}

	for _, e := range marketEvents {
		if e.ID != "" {
			byID[e.ID] = e
		}
	}

	var terminalEvents []terminal

	for _, e := range marketEvents {
		if e.CreatedAt >= offer.ExpiresAt {
			continue
		}

		if CancellationApplies(e, offer.EventID, offer.Maker, asOf) {
			terminalEvents = append(terminalEvents, terminal{
				createdAt: e.CreatedAt,
				id:        e.ID,
				state:     StateCancelled,
				cancelID:  e.ID,
			})

			continue
		}

		var message map[string]any

		if err := json.Unmarshal([]byte(e.Content), &message); err != nil {
			continue
		}

		if message["type"] != "market-match" {
			continue
		}

		takeID, _ := message["take_id"].(string)

		takeEvent, ok := byID[takeID]

		if !ok {
			continue
		}

		if _, err := ParseMatch(e, offerEvent, takeEvent, asOf); err != nil {
			continue
		}

		terminalEvents = append(terminalEvents, terminal{
			createdAt: e.CreatedAt,
			id:        e.ID,
			state:     StateMatched,
			matchID:   e.ID,
		})
	}

	if len(terminalEvents) > 0 {
		//
		// The first valid terminal event wins, and the event id breaks a tie between
		// two that share a timestamp — otherwise the answer would depend on the order
		// a relay happened to deliver them in.
		//
		sort.Slice(terminalEvents, func(i, j int) bool {
			if terminalEvents[i].createdAt != terminalEvents[j].createdAt {
				return terminalEvents[i].createdAt < terminalEvents[j].createdAt
			}

			return terminalEvents[i].id < terminalEvents[j].id
		})

		first := terminalEvents[0]

		return &StateResult{
			State:    first.state,
			Offer:    *offer,
			CancelID: first.cancelID,
			MatchID:  first.matchID,
		}, nil
	}

	state := StateOpen

	if offer.ExpiresAt <= now {
		state = StateExpired
	}

	return &StateResult{State: state, Offer: *offer}, nil
}

// toInt converts a decoded JSON number to an integer, or -1 when it is not one.
func toInt(value any) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int64:
		return typed
	case float64:
		if typed != float64(int64(typed)) {
			return -1
		}

		return int64(typed)
	}

	return -1
}
