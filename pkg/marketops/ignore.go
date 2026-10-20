package marketops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/store"
)

// tombstonesDirectory holds the local records of matches a participant has dismissed.
//
// A tombstone is local on purpose. Nothing is deleted, no Nostr event is touched and no swap
// state is created: a match is somebody else's signed statement, and the only thing this
// participant can truthfully say is "not this one, not now".
const tombstonesDirectory = "ignored-market-matches"

// TombstoneStore is what the ignore path needs beyond reading and writing one document: the
// tombstones are a directory whose *existence* is the suppression, so a store that cannot
// enumerate them cannot implement this. An expired tombstone is removed rather than left on
// disk, because a file that no longer suppresses anything is a file an operator will
// eventually misread.
type TombstoneStore interface {
	Files
	// Names returns the document names under a directory, sorted.
	Names(ctx context.Context, directory string) ([]string, error)
	// Remove deletes one document.
	Remove(ctx context.Context, name string) error
}

// tombstoneName names one tombstone.
func tombstoneName(matchID string) string { return tombstonesDirectory + "/" + matchID + ".json" }

// IgnoredMatch is what ignoring a match produced.
type IgnoredMatch struct {
	MatchID string
	SwapID  string
	// Repeated reports that the tombstone already existed.
	Repeated bool
	// AlreadyIgnored is the same fact under the name the operator reads.
	AlreadyIgnored bool
}

// AmbiguousMatchError reports a prefix that matched more than one valid match.
//
// It carries the candidates so a caller can show them: an operator told only "ambiguous" has no
// way to choose more characters.
type AmbiguousMatchError struct {
	Requested  string
	Candidates []marketflow.MatchEntry
}

// Error implements error.
func (e *AmbiguousMatchError) Error() string { return "Match prefix is ambiguous; use more characters" }

// IsAmbiguous reports whether an error is an ambiguous match prefix.
func IsAmbiguous(err error) (*AmbiguousMatchError, bool) {
	var ambiguous *AmbiguousMatchError

	if errors.As(err, &ambiguous) {
		return ambiguous, true
	}

	return nil, false
}

// ReadMatches reads the valid matches addressed to this participant as the taker.
//
// It is the CLI's inbox: the reference's listing is the taker's, and its validator refuses a
// match "for another taker".
func ReadMatches(ctx context.Context, env *Env, limit int) ([]marketflow.MatchEntry, error) {
	matches, err := MatchesNaming(ctx, env, limit)

	if err != nil {
		return nil, err
	}

	return addressedToTaker(env.Identity.Public, matches), nil
}

// MatchesNaming reads the verified matches that name this participant, as maker or taker.
//
// The two references are immutable, so they are satisfied from the verified cache when
// possible and only fetched when this machine has never seen them: a relay may hold a match and
// not its offer, and a match is not evidence until both are present. That is why the read is
// not one query — the match subscription carries the matches, and the takes of an offer are
// only reachable by the offer's id.
func MatchesNaming(ctx context.Context, env *Env, limit int) ([]marketflow.MatchEntry, error) {
	if err := env.checkMarket(); err != nil {
		return nil, err
	}

	me := env.Identity.Public

	// Two filters, because a match is addressed to its taker and signed by its maker: the
	// inbox query only carries one of those roles, and in the reverse direction the maker is
	// the side that has work left.
	report, err := env.Client.FetchReport(ctx,
		nostr.Filter{
			Kinds:  []int{1},
			Topics: []string{marketflow.MatchTag},
			PTags:  []string{me},
			Limit:  limit,
		},
		nostr.Filter{
			Kinds:   []int{1},
			Topics:  []string{marketflow.MatchTag},
			Authors: []string{me},
			Limit:   limit,
		},
	)

	if err != nil {
		return nil, err
	}

	evidence := append([]event.Event{}, report.Events...)
	evidence = append(evidence, cachedEvents(ctx, env.Client.Cache)...)

	for _, found := range report.Events {
		if err := env.Client.Cache.Put(ctx, found); err != nil {
			return nil, err
		}
	}

	missing := missingReferences(evidence)

	if len(missing) > 0 {
		dependencies, err := env.Client.FetchMany(ctx, nostr.Filter{Kinds: []int{1}, Ids: missing})

		if err != nil {
			return nil, err
		}

		for _, found := range dependencies {
			if err := env.Client.Cache.Put(ctx, found); err != nil {
				return nil, err
			}
		}

		evidence = append(evidence, dependencies...)
	}

	return marketflow.MatchesFor(me, evidence, env.now()), nil
}

// addressedToTaker keeps the matches this participant is the taker of.
//
// The reference's inbox is the taker's, and its validator refuses a match whose taker is not
// the participant. A match this participant *made* is their own receipt rather than a decision
// waiting for them.
func addressedToTaker(me string, matches []marketflow.MatchEntry) []marketflow.MatchEntry {
	kept := make([]marketflow.MatchEntry, 0, len(matches))

	for _, entry := range matches {
		if entry.Match.Taker != me {
			continue
		}

		kept = append(kept, entry)
	}

	return kept
}

// missingReferences lists the offer and take ids that match events name but the evidence does
// not contain.
func missingReferences(evidence []event.Event) []string {
	have := make(map[string]bool, len(evidence))

	for _, e := range evidence {
		have[e.ID] = true
	}

	seen := map[string]bool{}
	var missing []string

	for _, e := range evidence {
		if !e.HasTag("t", marketflow.MatchTag) {
			continue
		}

		var message map[string]any

		if err := json.Unmarshal([]byte(e.Content), &message); err != nil {
			continue
		}

		for _, field := range []string{"offer_id", "take_id"} {
			id, _ := message[field].(string)

			if id == "" || have[id] || seen[id] {
				continue
			}

			seen[id] = true

			missing = append(missing, id)
		}
	}

	return missing
}

// IgnoreMatch records that a valid, unimported match should stop being offered.
//
// It is local and durable, and it changes nothing else: no event is published, no record is
// created, no funds move. request is a full match id or a prefix of at least eight characters,
// and it must name exactly one valid match — guessing between two counterparties is the
// participant's decision, not a client's.
func IgnoreMatch(
	ctx context.Context, env *Env, request string, ttl time.Duration,
) (*IgnoredMatch, error) {
	if err := env.checkMarket(); err != nil {
		return nil, err
	}

	if env.Records == nil {
		return nil, fmt.Errorf("the market operations need the swap store")
	}

	if _, ok := env.Files.(TombstoneStore); !ok {
		return nil, fmt.Errorf("this store cannot hold local match tombstones")
	}

	requested := strings.ToLower(strings.TrimSpace(request))

	if len(requested) < 8 {
		return nil, fmt.Errorf("match ID prefix is too short; use at least 8 characters")
	}

	matches, err := ReadMatches(ctx, env, 200)

	if err != nil {
		return nil, err
	}

	var candidates []marketflow.MatchEntry

	for _, entry := range matches {
		if strings.HasPrefix(entry.Event.ID, requested) {
			candidates = append(candidates, entry)
		}
	}

	switch len(candidates) {
	case 0:
		return nil, fmt.Errorf("No valid market match begins with '%s'", requested)
	case 1:
	default:
		return nil, &AmbiguousMatchError{Requested: requested, Candidates: candidates}
	}

	entry := candidates[0]
	matchID := entry.Event.ID
	swapID := entry.Match.SwapID

	// A match that already has local swap state is being negotiated here; ignoring it would
	// hide a trade in progress. It is the record store that answers this, not a file's
	// existence: the worker keeps its swaps sealed and the CLI keeps them on disk, and the
	// question is the same in both.
	if swapID != "" {
		if _, err := env.Records.Load(ctx, swapID); err == nil {
			return nil, fmt.Errorf("Refusing to ignore: this match already has local swap state")
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
	}

	name := tombstoneName(matchID)

	existing, err := env.readDocument(ctx, name)

	if err != nil {
		return nil, err
	}

	if existing != nil {
		return &IgnoredMatch{
			MatchID: matchID, SwapID: swapID, Repeated: true, AlreadyIgnored: true,
		}, nil
	}

	document := map[string]any{
		"match_id":     matchID,
		"swap_id":      nilIfEmpty(swapID),
		"offer_id":     entry.Match.OfferID,
		"take_id":      entry.Match.TakeID,
		"maker":        entry.Match.Maker,
		"taker":        entry.Match.Taker,
		"direction":    string(entry.Match.Direction),
		"blake_amount": json.Number(entry.Match.BlakeAmount.Format()),
		"btc_sats":     entry.Match.BTCSats,
		"ignored_at":   env.now().Unix(),
		"local_only":   true,
	}

	// One write, one document: a half-written tombstone would be worse than none, because the
	// next run would see it, call the match ignored and never write the real one.
	if err := env.writeDocument(ctx, name, document); err != nil {
		return nil, err
	}

	return &IgnoredMatch{MatchID: matchID, SwapID: swapID}, nil
}

// IgnoredMatchIDs is the set of match ids this participant has dismissed, given a TTL.
//
// A tombstone only counts when it is named after the match it records and that name is 64
// characters — a document that is not a tombstone is not a suppression. A positive TTL bounds
// it: an expired tombstone stops suppressing and is removed here, so a match dismissed by
// mistake comes back rather than disappearing for good. A TTL of zero means forever, which is
// the default.
func IgnoredMatchIDs(ctx context.Context, env *Env, ttl time.Duration) (map[string]bool, error) {
	tombstones, ok := env.Files.(TombstoneStore)

	if !ok {
		return nil, fmt.Errorf("this store cannot hold local match tombstones")
	}

	names, err := tombstones.Names(ctx, tombstonesDirectory)

	if err != nil {
		return nil, err
	}

	ignored := make(map[string]bool, len(names))
	now := env.now().Unix()
	seconds := int64(ttl / time.Second)

	for _, name := range names {
		matchID := strings.TrimPrefix(name, tombstonesDirectory+"/")
		matchID = strings.TrimSuffix(matchID, ".json")

		if len(matchID) != 64 {
			continue
		}

		document, err := env.readDocument(ctx, name)

		if err != nil || document == nil {
			continue
		}

		if value, _ := document["match_id"].(string); value != matchID {
			continue
		}

		if seconds > 0 {
			ignoredAt := int64(0)

			if value, ok := document["ignored_at"].(float64); ok {
				ignoredAt = int64(value)
			}

			// An undated tombstone cannot be aged, so it is treated as expired rather than
			// suppressing a match forever.
			if now-ignoredAt >= seconds {
				_ = tombstones.Remove(ctx, name)

				continue
			}
		}

		ignored[matchID] = true
	}

	return ignored, nil
}

// nilIfEmpty renders an absent swap id as JSON null, which is what the reference writes.
func nilIfEmpty(value string) any {
	if value == "" {
		return nil
	}

	return value
}
