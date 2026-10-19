package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/marketops"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/store"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// runAcceptMarketTake turns one signed take into an atomic swap.
//
// This is the maker's one irreversible market act. Everything before it is an
// advertisement or an expression of interest; accepting creates the swap, pins both
// authenticated identities, and publishes both the ordinary swap offer and the market
// match that binds offer, take and swap together.
//
// Three rules shape the order of the work here, and all three are the reference's:
//
//   - the take is re-validated through the same canonical validator everybody else
//     uses, so a take that reached this command through a hand-edited file or a relay
//     that served something else cannot create a swap;
//   - the durable selection records are written before anything is announced, because
//     they are what stops a second take creating a second swap from one offer; and
//   - the duplicate path is not an error. A crash between publishing and recording, or
//     an operator running the command twice, must end in "already accepted" rather than
//     in a second secret.
func runAcceptMarketTake(ctx context.Context, engine *swap.Engine, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("accept-market-take takes one take id")
	}

	key, err := engineKey(engine)

	if err != nil {
		return err
	}

	client := marketClient()

	takeID, err := resolveCachedPrefix(ctx, client, args[0], "market-take", "market take", true)

	if err != nil {
		return err
	}

	//
	// The take is fetched here rather than inside the operation, because the fetch is what
	// prints where the event came from and the CLI has already announced the read. The
	// operation re-validates it either way.
	//
	takeEvent, err := fetchEventByID(ctx, client, takeID)

	if err != nil {
		return err
	}

	env, err := marketEnvOrError(engine, client, func(line string) { fmt.Print(line) })

	if err != nil {
		return err
	}

	_, err = marketops.AcceptTake(ctx, env, takeID, takeEvent, marketops.TakeHooks{
		Accepted: func(acceptance marketops.TakeAcceptance) {
			if acceptance.Repeated {
				fmt.Print(renderMarketTakeAlreadyAccepted(
					acceptance.OfferID, acceptance.TakeID, acceptance.SwapID,
				))

				return
			}

			fmt.Print(renderMarketTakeAccepted(
				acceptance.OfferID, acceptance.TakeID, acceptance.SwapID,
				acceptance.Alice, acceptance.Bob,
				acceptance.Blake, acceptance.BTCSats,
				acceptance.BTCTimeout, acceptance.BlakeTimeout,
			))
		},
		BeforeOfferPublish: func() {
			fmt.Print("\nPublishing normal authenticated atomic-swap offer...\n")
		},
		AfterOfferPublish: func(location string) {
			fmt.Print(renderOfferPublished(
				selectionSwapID(ctx, env, takeID), messageLocation(location),
			))
		},
		BeforeMatchPublish: func() {
			fmt.Print("\nPublishing signed market match...\n")
		},
		AfterMatchPublish: func(publication marketops.MatchPublication) {
			fmt.Print(renderOutboundCached(publication.Published.ID, cacheDirectory()))
			fmt.Print(renderRelayResults(publication.Report))
			fmt.Print(renderMatchPublished(
				publication.OfferID, publication.TakeID, publication.MatchID, publication.SwapID,
			))
			fmt.Print(renderMarketBridge(
				publication.OfferID, publication.TakeID, publication.MatchID, publication.SwapID,
			))
		},
	})

	if reverse, ok := marketops.IsReverse(err); ok {
		return acceptReverseMarketTake(ctx, engine, key, client, takeAcceptInput{
			Offer:      reverse.Offer,
			OfferTerms: reverse.Terms,
			OfferID:    reverse.OfferID,
			TakeEvent:  reverse.Take,
			TakeID:     reverse.TakeID,
			Directory:  filepath.Join(stateDirectory(), selectionsDirectory),
		})
	}

	return err
}

// selectionSwapID reads back the swap id the acceptance recorded.
//
// The publish announcement names the swap, and the acceptance has just written it down; this
// reads the record rather than carrying a second copy of the same fact through the hooks.
func selectionSwapID(ctx context.Context, env *marketops.Env, takeID string) string {
	record, err := env.Files.Read(ctx, "selections/"+takeID+".json")

	if err != nil {
		return ""
	}

	var selection map[string]any

	if err := json.Unmarshal(record, &selection); err != nil {
		return ""
	}

	swapID, _ := selection["swap_id"].(string)

	return swapID
}

// takeAcceptInput is everything the reverse path needs from the forward one.
type takeAcceptInput struct {
	Offer      *event.Event
	OfferTerms *marketflow.ListedOffer
	OfferID    string
	TakeEvent  *event.Event
	TakeID     string
	Directory  string
}

// repeatAccept describes a selection that already exists.
type repeatAccept struct {
	OfferID          string
	TakeID           string
	Alice            string
	Bob              string
	OfferSelection   map[string]any
	TakeSelection    map[string]any
	TakePath         string
	OfferPath        string
	OfferSelectionOn bool
	TakeSelectionOn  bool
}

// repeatAcceptMarketTake finishes a negotiation that was already recorded.
//
// Two records describe one accepted take. If an earlier run stopped between the two
// writes, one of them is missing, and this repairs it rather than refusing to work —
// but only after both records and the swap agree on every identity and on the swap id,
// because repairing a disagreement would be inventing a fact.
func repeatAcceptMarketTake(ctx context.Context, req repeatAccept) error {
	canonical := req.TakeSelection

	if canonical == nil {
		canonical = req.OfferSelection
	}

	swapID, _ := canonical["swap_id"].(string)

	if req.OfferSelectionOn && req.TakeSelectionOn {
		offerSwap, _ := req.OfferSelection["swap_id"].(string)
		takeSwap, _ := req.TakeSelection["swap_id"].(string)

		if offerSwap != takeSwap {
			return fmt.Errorf("SECURITY FAILURE: market selection records disagree about swap_id")
		}
	}

	if v, _ := canonical["nostr_alice_pub"].(string); v != req.Alice {
		return fmt.Errorf("SECURITY FAILURE: existing market selection Alice identity mismatch")
	}

	if v, _ := canonical["nostr_bob_pub"].(string); v != req.Bob {
		return fmt.Errorf("SECURITY FAILURE: existing market selection Bob identity mismatch")
	}

	record, err := store.NewFile(stateDirectory()).Load(ctx, swapID)

	if err != nil {
		return err
	}

	if record.NostrAlicePub == nil || *record.NostrAlicePub != req.Alice {
		return fmt.Errorf("SECURITY FAILURE: existing swap Alice identity mismatch")
	}

	if record.NostrBobPub == nil || *record.NostrBobPub != req.Bob {
		return fmt.Errorf("SECURITY FAILURE: existing swap Bob identity mismatch")
	}

	if !req.TakeSelectionOn {
		if err := writeSelection(req.TakePath, canonical); err != nil {
			return err
		}
	}

	if !req.OfferSelectionOn {
		if err := writeSelection(req.OfferPath, canonical); err != nil {
			return err
		}
	}

	fmt.Print(renderMarketTakeAlreadyAccepted(req.OfferID, req.TakeID, swapID))

	return nil
}

// marketTakeSet is one offer together with the valid takes of it.
type marketTakeSet struct {
	offer  *event.Event
	listed *marketflow.ListedOffer
	takes  []marketflow.TakeEntry
}

// readTakeSet reads the takes of one offer through the canonical validator.
//
// The offer itself is fetched by id and is authoritative for terms and identity: a
// broad relay scan is asked only for the state evidence a single event cannot carry, such
// as a cancellation or a match that already settled the offer.
func readTakeSet(
	ctx context.Context,
	client *nostr.Client,
	offerID string,
	limit int,
	extra []event.Event,
	allowExpired, allowMatched bool,
) (*marketTakeSet, error) {
	now := time.Now()

	offer, err := fetchEventByID(ctx, client, offerID)

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

	// One canonical definition of market state, from evidence rather than from the
	// clock: expiry is a fact about the offer, but a cancellation or a match is a fact
	// about its history.
	marketReport, err := readMarket(ctx, 200)

	if err != nil {
		return nil, err
	}

	derived, err := marketflow.DeriveState(*offer, marketReport.events, now.Unix())

	if err != nil {
		return nil, err
	}

	if derived.State == marketflow.StateExpired && !allowExpired {
		return nil, fmt.Errorf("Original offer is expired")
	}

	if derived.State == marketflow.StateCancelled {
		return nil, fmt.Errorf(
			"Original offer was cancelled by maker: %s", derived.CancelID,
		)
	}

	matchedOK := allowMatched && derived.State == marketflow.StateMatched
	expiredOK := allowExpired && derived.State == marketflow.StateExpired

	if derived.State != marketflow.StateOpen && !matchedOK && !expiredOK {
		return nil, fmt.Errorf("Unknown market offer state: %s", derived.State)
	}

	takeReport, err := client.FetchReport(ctx, nostr.Filter{
		Kinds:  []int{1},
		Topics: []string{marketflow.TakeTag},
		ETags:  []string{offerID},
		Limit:  limit,
	})

	if err != nil {
		return nil, err
	}

	// Every relay, cached event and explicit candidate stays untrusted until the
	// validator below says otherwise; this merge only decides what to look at.
	candidates := make([]event.Event, 0, len(takeReport.Events)+len(extra))

	seen := map[string]bool{}

	add := func(candidate event.Event) {
		if candidate.ID == "" || seen[candidate.ID] {
			return
		}

		seen[candidate.ID] = true

		candidates = append(candidates, candidate)
	}

	// Authentic takes are kept the moment they are seen, so the next command can still
	// resolve them after a relay has forgotten the negotiation.
	cacheMarketTakes(ctx, client, takeReport.Events)

	for _, found := range takeReport.Events {
		add(found)
	}

	for _, supplied := range extra {
		add(supplied)
	}

	for _, cached := range cachedEvents(ctx, client.Cache) {
		add(cached)
	}

	if takeReport.Answered == 0 && len(candidates) == 0 {
		return nil, fmt.Errorf(
			"Could not query any configured Nostr relay for market takes and no local " +
				"candidate events were supplied",
		)
	}

	takes := marketflow.TakesFor(listed, candidates, now)

	fmt.Printf("Nostr take read: %d unique event(s) from %d/%d relay(s)\n",
		len(candidates), takeReport.Answered, takeReport.Relays)

	return &marketTakeSet{offer: offer, listed: listed, takes: takes}, nil
}

// readSelection reads one selection record, or nil when it does not exist.
func readSelection(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)

	if os.IsNotExist(err) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	var selection map[string]any

	if err := json.Unmarshal(raw, &selection); err != nil {
		return nil, err
	}

	return selection, nil
}

// writeSelection writes a selection record atomically.
func writeSelection(path string, selection map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	encoded, err := json.MarshalIndent(selection, "", "  ")

	if err != nil {
		return err
	}

	return writeAtomically(path, append(encoded, '\n'))
}

// fileExists reports whether a path names an existing entry.
func fileExists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}

// renderMarketTakeAccepted is the reference's confirmation block for an accepted take.
func renderMarketTakeAccepted(
	offerID, takeID, swapID, alice, bob string,
	blake amount.Blake,
	sats, btcTimeout, blakeTimeout int64,
) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("MARKET TAKE ACCEPTED\n")
	out.WriteString("====================\n")
	fmt.Fprintf(&out, "offer id: %s\n", offerID)
	fmt.Fprintf(&out, "take id:  %s\n", takeID)
	fmt.Fprintf(&out, "swap id:  %s\n", swapID)
	out.WriteString("\n")
	fmt.Fprintf(&out, "Alice Nostr: %s\n", alice)
	fmt.Fprintf(&out, "Bob Nostr:   %s\n", bob)
	out.WriteString("\n")
	fmt.Fprintf(&out, "BLAKE: %s\n", pythonFloat(blakeFloat(blake)))
	fmt.Fprintf(&out, "BTC sats: %d\n", sats)
	out.WriteString("\n")
	fmt.Fprintf(&out, "BTC timeout:   %d\n", btcTimeout)
	fmt.Fprintf(&out, "BLAKE timeout: %d\n", blakeTimeout)
	out.WriteString("\n")
	out.WriteString("PASS: fresh secret/hashlock created\n")
	out.WriteString("PASS: Bob pinned from exact signed take\n")
	out.WriteString("PASS: Alice pinned from exact signed offer\n")
	out.WriteString("PASS: no funds have moved\n")

	return out.String()
}

// renderMarketTakeAlreadyAccepted is the block the duplicate command prints.
func renderMarketTakeAlreadyAccepted(offerID, takeID, swapID string) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("MARKET TAKE ALREADY ACCEPTED\n")
	out.WriteString("============================\n")
	fmt.Fprintf(&out, "offer id: %s\n", offerID)
	fmt.Fprintf(&out, "take id:  %s\n", takeID)
	fmt.Fprintf(&out, "swap id:  %s\n", swapID)
	out.WriteString("\n")
	out.WriteString("PASS: duplicate command did not create another swap\n")
	out.WriteString("PASS: offer/take selection records are mutually consistent\n")
	out.WriteString("PASS: missing selection record, if any, was repaired\n")

	return out.String()
}

// renderMarketBridge is the block that closes the market-to-swap handover.
func renderMarketBridge(offerID, takeID, matchID, swapID string) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("MARKET -> SWAP BRIDGE CREATED\n")
	out.WriteString("=============================\n")
	fmt.Fprintf(&out, "market offer: %s\n", offerID)
	fmt.Fprintf(&out, "market take:  %s\n", takeID)
	fmt.Fprintf(&out, "market match: %s\n", matchID)
	fmt.Fprintf(&out, "atomic swap:  %s\n", swapID)
	out.WriteString("\n")
	out.WriteString("PASS: settlement engine was not modified\n")
	out.WriteString("PASS: HTLC funding remains a separate explicit action\n")

	return out.String()
}
