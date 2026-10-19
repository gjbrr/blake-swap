// Package marketops is the market's accept path: what turns a signed take into an atomic
// swap, and what imports the swap a signed match binds.
//
// It exists because two front ends need it. The CLI drives it from a state directory and the
// browser worker drives it from the sealed vault, and neither may own the state machine: a
// second copy that drifted would create a second swap from one offer on one client and not
// the other. The differences between the two are the durable store, the relay client and
// where the human-readable output goes — all of which are parameters here.
//
// The operations do not render. They call the caller's hooks at the points the reference
// prints at, so the CLI's output stays byte-identical and the worker builds its views from
// the same facts.
package marketops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/store"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// Files is the durable side of the accept path: the selection records that bind an offer and
// a take to a swap, and the match bindings that bind a negotiation to the taker's swap.
//
// Both are plain JSON documents. The CLI keeps them as files under the state directory; the
// worker keeps them sealed in the vault. A name is a slash-separated path such as
// "selections/offer-<id>.json", and reading an absent name returns fs.ErrNotExist.
type Files interface {
	Read(ctx context.Context, name string) ([]byte, error)
	Write(ctx context.Context, name string, data []byte) error
}

// Engine is the part of the swap engine the accept path uses.
//
// It is the engine itself in both front ends. The interface exists so the operations can be
// tested against a recording double, and so this package does not have to build one.
type Engine interface {
	New(ctx context.Context, req swap.NewRequest) (*swap.NewResult, error)
	AcceptOffer(ctx context.Context, req swap.AcceptRequest) (*swap.AcceptResult, error)
	PublishOffer(ctx context.Context, swapID string) (*swap.MessageResult, error)
}

// Env is everything an operation needs from its caller.
type Env struct {
	// Client is the relay connection the read and the publications use.
	Client *nostr.Client
	// Identity is this participant's signing key.
	Identity *nostr.Key
	// Records is where swaps are stored.
	Records store.Store
	// Files is where the selection records and match bindings are stored.
	Files Files
	// Engine performs the swap-level work.
	Engine Engine
	// Random is the entropy for a new swap. Nil is refused by the engine, so a caller must
	// supply one.
	Random io.Reader
	// Now is the clock. Zero means time.Now.
	Now func() time.Time
	// Print receives the scan lines the reference prints while it reads. Nil discards them,
	// which is what the worker wants: its page shows a view, not a transcript.
	Print func(string)
}

// printf writes one line of progress, when the caller wants one.
func (e *Env) printf(format string, args ...any) {
	if e.Print == nil {
		return
	}

	e.Print(fmt.Sprintf(format, args...))
}

// now is the clock, defaulted.
func (e *Env) now() time.Time {
	if e.Now == nil {
		return time.Now()
	}

	return e.Now()
}

// check validates the environment before anything touches a relay.
func (e *Env) check() error {
	switch {
	case e.Client == nil:
		return fmt.Errorf("the market operations need a relay client")
	case e.Identity == nil:
		return fmt.Errorf("the market operations need an identity")
	case e.Files == nil:
		return fmt.Errorf("the market operations need a durable store")
	case e.Engine == nil:
		return fmt.Errorf("the market operations need the swap engine")
	}

	return nil
}

// TakeSet is one offer together with the valid takes of it.
type TakeSet struct {
	// Offer is the exact signed offer event.
	Offer *event.Event
	// Listed is its parsed terms.
	Listed *marketflow.ListedOffer
	// Takes are the takes that passed the canonical validator, newest first.
	Takes []marketflow.TakeEntry
	// Candidates, Answered and Relays are what the scan saw, for a view's scan line.
	Candidates int
	Answered   int
	Relays     int
}

// TakeAcceptance is what accepting a take produced.
type TakeAcceptance struct {
	OfferID string
	TakeID  string
	SwapID  string
	Alice   string
	Bob     string

	Blake   amount.Blake
	BTCSats int64

	BTCTimeout   int64
	BlakeTimeout int64

	// Repeated reports that the selection records already existed and this call finished or
	// repaired them rather than creating a second swap.
	Repeated bool
	// MatchID is the published match, empty on the repeated path.
	MatchID string
	// OfferLocation is where the protocol offer was written or published.
	OfferLocation string
}

// MatchPublication is what publishing a match bound together, with the evidence a caller
// needs to report it: the signed event and the relays' answers.
type MatchPublication struct {
	OfferID string
	TakeID  string
	MatchID string
	SwapID  string

	Published event.Event
	Report    nostr.Report
}

// MatchImport is what importing a match produced.
type MatchImport struct {
	MatchID string
	OfferID string
	TakeID  string
	SwapID  string
	Maker   string
	Taker   string

	Blake   amount.Blake
	BTCSats int64

	// Repeated reports that Bob's swap already existed and this call finished or repaired
	// the binding rather than creating a second one.
	Repeated bool
	// Accept is the accept-offer result, nil on the repeated path.
	Accept *swap.AcceptResult
}

// TakeHooks are the points at which the caller renders.
//
// A nil hook is not called. The order is the reference's: the acceptance is reported once the
// selections are durable and before anything is announced, because the selections are what
// stop a second take creating a second swap.
type TakeHooks struct {
	// Accepted reports the acceptance, repeated or fresh.
	Accepted func(TakeAcceptance)
	// BeforeOfferPublish and AfterOfferPublish bracket the protocol offer publication.
	BeforeOfferPublish func()
	AfterOfferPublish  func(location string)
	// BeforeMatchPublish and AfterMatchPublish bracket the match publication.
	BeforeMatchPublish func()
	AfterMatchPublish  func(MatchPublication)
}

// MatchHooks are the caller's rendering points for an import.
type MatchHooks struct {
	BeforeAccept func()
	AfterAccept  func(*swap.AcceptResult)
	AfterImport  func(MatchImport)
}

// ReverseTakeError reports a take in the reverse direction, which is a different market flow
// with its own accept path.
//
// It carries the parsed evidence so a caller that does implement the reverse direction can
// proceed without reading the relay again, which is what keeps the CLI's output unchanged.
type ReverseTakeError struct {
	Offer   *event.Event
	Terms   *marketflow.ListedOffer
	Take    *event.Event
	OfferID string
	TakeID  string
}

// Error implements error.
func (e *ReverseTakeError) Error() string {
	return "the reverse market direction has its own accept path"
}

// IsReverse reports whether an error is a reverse-direction take.
func IsReverse(err error) (*ReverseTakeError, bool) {
	var reverse *ReverseTakeError

	if errors.As(err, &reverse) {
		return reverse, true
	}

	return nil, false
}

// selectionPath names one selection record.
func selectionPath(name string) string { return "selections/" + name + ".json" }

// bindingPath names one match binding.
func bindingPath(swapID string) string { return "bindings/" + swapID + ".json" }

// readDocument reads one JSON document, or nil when it does not exist.
func (e *Env) readDocument(ctx context.Context, name string) (map[string]any, error) {
	raw, err := e.Files.Read(ctx, name)

	if errors.Is(err, errNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	if len(raw) == 0 {
		return nil, nil
	}

	var document map[string]any

	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("Existing market record is unreadable")
	}

	return document, nil
}

// writeDocument writes one JSON document.
func (e *Env) writeDocument(ctx context.Context, name string, document map[string]any) error {
	encoded, err := json.MarshalIndent(document, "", "  ")

	if err != nil {
		return err
	}

	return e.Files.Write(ctx, name, append(encoded, '\n'))
}

// cacheMarketTakes retains the authentic take events one read saw.
//
// The reference's take validator caches an event as soon as its signature and take tag
// verify, before it judges protocol validity, and that ordering is deliberate: authenticity
// is immutable evidence, validity is a conclusion about it. Skipping this is what makes a
// take prefix unresolvable on the very next command even though the take was read a moment
// ago.
func cacheMarketTakes(ctx context.Context, client *nostr.Client, events []event.Event) {
	for _, candidate := range events {
		if candidate.Kind != event.KindTextNote || !candidate.HasTag("t", marketflow.TakeTag) {
			continue
		}

		// A cache write failure is not a read failure: the evidence is still in hand, and
		// losing it from the cache costs one refetch, not a decision.
		_ = client.Cache.Put(ctx, candidate)
	}
}

// MarketEvidence fetches the market subscriptions, caches what it saw, and merges the cache
// back in.
//
// The merge is the point: relay retention is not the protocol's history, so an offer still
// inside the discovery window can lose the cancellation that settled it. Cached events are
// re-verified by Book, so a tampered file cannot change a state.
func MarketEvidence(ctx context.Context, env *Env, limit int) ([]event.Event, error) {
	report, err := env.Client.FetchReport(ctx, marketflow.MarketFilters(limit)...)

	if err != nil {
		return nil, err
	}

	for _, seen := range report.Events {
		if err := env.Client.Cache.Put(ctx, seen); err != nil {
			return nil, err
		}
	}

	span := len(report.Events)
	events := make([]event.Event, 0, span)
	seen := make(map[string]bool, span)

	for _, found := range report.Events {
		if seen[found.ID] {
			continue
		}

		seen[found.ID] = true

		events = append(events, found)
	}

	for _, cached := range cachedEvents(ctx, env.Client.Cache) {
		if seen[cached.ID] {
			continue
		}

		seen[cached.ID] = true

		events = append(events, cached)
	}

	return events, nil
}

// cachedEvents reads every cached event.
func cachedEvents(ctx context.Context, cache nostr.Cache) []event.Event {
	ids, err := cache.IDs(ctx)

	if err != nil {
		return nil
	}

	events := make([]event.Event, 0, len(ids))

	for _, id := range ids {
		stored, ok, err := cache.Get(ctx, id)

		if err != nil || !ok {
			continue
		}

		events = append(events, stored)
	}

	return events
}

// FetchEvent asks the relays for one event and returns it, saying where it came from.
//
// The fallback to the verified local cache is what makes a negotiation survive a relay that
// has forgotten it: the event's signature is its own proof, so a cached copy is as good as a
// fresh one, and the operator is told which of the two they got.
func FetchEvent(ctx context.Context, env *Env, id string) (*event.Event, error) {
	report, err := env.Client.FetchReport(ctx, nostr.Filter{Kinds: []int{1}, Ids: []string{id}})

	if err == nil {
		for index := range report.Events {
			if report.Events[index].ID != id {
				continue
			}

			found := report.Events[index]

			// Cache only what verified: the cache is evidence, not a spool.
			if env.Client.Cache != nil {
				_ = env.Client.Cache.Put(ctx, found)
			}

			env.printf("Nostr event found on: %s\n", joinLocations(report.Locations[id]))

			return &found, nil
		}
	}

	if env.Client.Cache != nil {
		cached, ok, cacheErr := env.Client.Cache.Get(ctx, id)

		if cacheErr == nil && ok {
			if verifyErr := event.Verify(cached, env.now()); verifyErr == nil {
				env.printf("Nostr event unavailable on configured relays; using verified local cache:\n")
				env.printf("%s\n", id)

				return &cached, nil
			}
		}
	}

	return nil, fmt.Errorf(
		"Nostr event not found on any configured relay or verified local cache: %s", id,
	)
}

// joinLocations renders the relays that served one event.
func joinLocations(locations []string) string {
	out := ""

	for index, location := range locations {
		if index > 0 {
			out += ", "
		}

		out += location
	}

	return out
}

// ReadTakeSet reads the takes of one offer through the canonical validator.
//
// The offer itself is fetched by id and is authoritative for terms and identity: a broad
// relay scan is asked only for the state evidence a single event cannot carry, such as a
// cancellation or a match that already settled the offer.
func ReadTakeSet(
	ctx context.Context,
	env *Env,
	offerID string,
	limit int,
	extra []event.Event,
	allowExpired, allowMatched bool,
) (*TakeSet, error) {
	now := env.now()

	offer, err := FetchEvent(ctx, env, offerID)

	if err != nil {
		return nil, err
	}

	if err := event.Verify(*offer, now); err != nil {
		return nil, err
	}

	listed, err := marketflow.ParseOffer(*offer, now)

	if err != nil {
		return nil, fmt.Errorf("Referenced event is not a market offer")
	}

	// One canonical definition of market state, from evidence rather than from the clock:
	// expiry is a fact about the offer, but a cancellation or a match is a fact about its
	// history.
	marketEvents, err := MarketEvidence(ctx, env, 200)

	if err != nil {
		return nil, err
	}

	derived, err := marketflow.DeriveState(*offer, marketEvents, now.Unix())

	if err != nil {
		return nil, err
	}

	if derived.State == marketflow.StateExpired && !allowExpired {
		return nil, fmt.Errorf("Original offer is expired")
	}

	if derived.State == marketflow.StateCancelled {
		return nil, fmt.Errorf("Original offer was cancelled by maker: %s", derived.CancelID)
	}

	matchedOK := allowMatched && derived.State == marketflow.StateMatched
	expiredOK := allowExpired && derived.State == marketflow.StateExpired

	if derived.State != marketflow.StateOpen && !matchedOK && !expiredOK {
		return nil, fmt.Errorf("Unknown market offer state: %s", derived.State)
	}

	takeReport, err := env.Client.FetchReport(ctx, nostr.Filter{
		Kinds:  []int{1},
		Topics: []string{marketflow.TakeTag},
		ETags:  []string{offerID},
		Limit:  limit,
	})

	if err != nil {
		return nil, err
	}

	// Every relay, cached event and explicit candidate stays untrusted until the validator
	// says otherwise; this merge only decides what to look at.
	candidates := make([]event.Event, 0, len(takeReport.Events)+len(extra))
	seen := map[string]bool{}

	add := func(candidate event.Event) {
		if candidate.ID == "" || seen[candidate.ID] {
			return
		}

		seen[candidate.ID] = true

		candidates = append(candidates, candidate)
	}

	cacheMarketTakes(ctx, env.Client, takeReport.Events)

	for _, found := range takeReport.Events {
		add(found)
	}

	for _, supplied := range extra {
		add(supplied)
	}

	for _, cached := range cachedEvents(ctx, env.Client.Cache) {
		add(cached)
	}

	if takeReport.Answered == 0 && len(candidates) == 0 {
		return nil, fmt.Errorf(
			"Could not query any configured Nostr relay for market takes and no local " +
				"candidate events were supplied",
		)
	}

	env.printf("Nostr take read: %d unique event(s) from %d/%d relay(s)\n",
		len(candidates), takeReport.Answered, takeReport.Relays)

	return &TakeSet{
		Offer:      offer,
		Listed:     listed,
		Takes:      marketflow.TakesFor(listed, candidates, now),
		Candidates: len(candidates),
		Answered:   takeReport.Answered,
		Relays:     takeReport.Relays,
	}, nil
}
