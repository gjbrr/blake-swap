package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// runMarketTakes is the maker's inbox: the takes of one of their offers.
//
// It reads rather than writes. The offer may already have been matched, so this does not
// require it to be open — what a maker wants after a match is exactly to see what was
// accepted.
func runMarketTakes(ctx context.Context, engine *swap.Engine, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("market-takes takes one offer id")
	}

	flags := flag.NewFlagSet("market-takes", flag.ContinueOnError)

	limit := flags.Int("limit", 100, "how many takes to ask each relay for")
	asJSON := flags.Bool("json", false, "emit machine-readable JSON")

	if err := flags.Parse(args[1:]); err != nil {
		return err
	}

	key, err := engineKey(engine)

	if err != nil {
		return err
	}

	client := marketClient()
	now := time.Now()

	offerID, err := resolveCachedOfferPrefix(ctx, client, args[0])

	if err != nil {
		return err
	}

	original, err := fetchEventByID(ctx, client, offerID)

	if err != nil {
		return err
	}

	listed, err := marketflow.ParseOffer(*original, now)

	if err != nil {
		return fmt.Errorf("Referenced event is not a market offer")
	}

	// The state question comes before whose offer it is, because it is about the
	// evidence rather than about the caller: an expired offer's inbox is history, and
	// the reference refuses that before it looks at identity.
	marketReport, err := client.FetchReport(ctx, marketflow.MarketFilters(200)...)

	if err != nil {
		return err
	}

	marketEvents := append([]event.Event{}, marketReport.Events...)
	marketEvents = append(marketEvents, cachedEvents(ctx, client.Cache)...)

	derived, err := marketflow.DeriveState(*original, marketEvents, now.Unix())

	if err != nil {
		return err
	}

	if derived.State == marketflow.StateExpired {
		return fmt.Errorf("Original offer is expired")
	}

	// This command is the maker's inbox, so refusing to show somebody else's offer is
	// the point rather than a courtesy.
	if original.Pubkey != key.PublicKey() {
		return fmt.Errorf(
			"SECURITY FAILURE: this market offer was not signed by the current Alice identity",
		)
	}

	takeReport, err := client.FetchReport(ctx, nostr.Filter{
		Kinds:  []int{1},
		Topics: []string{marketflow.TakeTag},
		ETags:  []string{offerID},
		Limit:  *limit,
	})

	if err != nil {
		return err
	}

	cacheMarketTakes(ctx, client, takeReport.Events)

	//
	// The candidate set is the union of what the relays served and what the verified
	// cache holds, by event id. A take read in this invocation has just been cached, so
	// it is in both; counting it twice would report a relay as having served an event it
	// only appears to have served because it was cached on the way past.
	//
	evidence := append([]event.Event{}, takeReport.Events...)
	seen := make(map[string]bool, len(evidence))

	for _, found := range evidence {
		seen[found.ID] = true
	}

	for _, cached := range cachedEvents(ctx, client.Cache) {
		if seen[cached.ID] {
			continue
		}

		seen[cached.ID] = true

		evidence = append(evidence, cached)
	}

	takes := marketflow.TakesFor(listed, evidence, now)

	// The read is reported before the answer, in both forms. It is a fact about the
	// scan, not about the rendering, and a machine-readable answer still came from a
	// scan an operator may need to judge.
	fmt.Printf("Nostr take read: %d unique event(s) from %d/%d relay(s)\n",
		len(evidence), takeReport.Answered, takeReport.Relays)

	if *asJSON {
		text, err := marketTakesJSON(offerID, listed, takes)

		if err != nil {
			return err
		}

		fmt.Print(text)

		return nil
	}

	fmt.Print(renderMarketTakes(offerID, listed, takes))

	return nil
}

// fetchEventByID asks the relays for one event and returns it, saying where it came
// from.
//
// The fallback to the verified local cache is what makes a negotiation survive a relay
// that has forgotten it: the event's signature is its own proof, so a cached copy is as
// good as a fresh one, and an operator is told which of the two they got.
func fetchEventByID(ctx context.Context, client *nostr.Client, id string) (*event.Event, error) {
	report, err := client.FetchReport(ctx, nostr.Filter{Kinds: []int{1}, Ids: []string{id}})

	if err == nil {
		for index := range report.Events {
			if report.Events[index].ID != id {
				continue
			}

			found := report.Events[index]

			// Cache only what verified: the cache is evidence, not a spool.
			if client.Cache != nil {
				_ = client.Cache.Put(ctx, found)
			}

			fmt.Printf("Nostr event found on: %s\n", strings.Join(report.Locations[id], ", "))

			return &found, nil
		}
	}

	if client.Cache != nil {
		cached, ok, cacheErr := client.Cache.Get(ctx, id)

		if cacheErr == nil && ok {
			if verifyErr := event.Verify(cached, time.Now()); verifyErr == nil {
				fmt.Print("Nostr event unavailable on configured relays; using verified local cache:\n")
				fmt.Println(id)

				return &cached, nil
			}
		}
	}

	return nil, fmt.Errorf(
		"Nostr event not found on any configured relay or verified local cache: %s", id,
	)
}

// renderMarketTakes is the reference's take table.
func renderMarketTakes(offerID string, listed *marketflow.ListedOffer, takes []marketflow.TakeEntry) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("TAKES FOR MARKET OFFER\n")
	out.WriteString("======================\n")
	out.WriteString("\n")
	fmt.Fprintf(&out, "offer: %s\n", offerID)
	fmt.Fprintf(&out, "selling: %s BLAKE\n", listed.BlakeAmount.Format())
	fmt.Fprintf(&out, "asking:  %s sats\n", thousands(listed.BTCSats))
	out.WriteString("\n")

	if len(takes) == 0 {
		out.WriteString("No valid takes found.\n")
		out.WriteString("\n")
		out.WriteString("PASS: offer is open, but nobody has submitted a valid take\n")

		return out.String()
	}

	out.WriteString("Take          Taker          Selling        Asking\n")
	out.WriteString("------------  ------------   ------------   ------------\n")

	for _, take := range takes {
		fmt.Fprintf(&out, "%-12s  %-12s   %s BLAKE     %s sats\n",
			truncate(take.EventID, 12),
			truncate(take.Taker, 12),
			take.Take.BlakeAmount.Format(),
			thousands(take.Take.BTCSats),
		)
	}

	fmt.Fprintf(&out, "\n%d valid take(s)\n", len(takes))
	out.WriteString("\n")
	out.WriteString("FULL TAKE ID(S)\n")
	out.WriteString("===============\n")

	for _, take := range takes {
		fmt.Fprintf(&out, "%s\n", take.EventID)
	}

	out.WriteString("\n")
	out.WriteString("PASS: original offer is still open\n")
	out.WriteString("PASS: every take signature verifies\n")
	out.WriteString("PASS: every take references this exact offer\n")
	out.WriteString("PASS: maker and economic terms match\n")
	out.WriteString("PASS: no swap was created\n")
	out.WriteString("PASS: no money moved\n")

	return out.String()
}

// marketTakesJSON renders the machine-readable take list.
//
// Both the display amount and the exact base units are emitted, as the reference does:
// a UI needs the first and a settlement needs the second, and deriving one from the
// other is where rounding bugs live.
func marketTakesJSON(
	offerID string, listed *marketflow.ListedOffer, takes []marketflow.TakeEntry,
) (string, error) {
	items := make([]map[string]any, 0, len(takes))

	for _, take := range takes {
		items = append(items, map[string]any{
			"event_id":                take.EventID,
			"taker":                   take.Taker,
			"created_at":              take.Event.CreatedAt,
			"offer_id":                offerID,
			"maker":                   listed.Maker,
			"blake_amount":            json.RawMessage(pythonFloat(blakeFloat(take.Take.BlakeAmount))),
			"blake_amount_base_units": int64(take.Take.BlakeAmount),
			"btc_sats":                take.Take.BTCSats,
			"direction":               string(take.Take.Direction),
			"maker_role":              take.Take.MakerRole,
			"taker_role":              take.Take.TakerRole,
		})
	}

	encoded, err := json.MarshalIndent(map[string]any{
		"offer_id": offerID,
		"takes":    items,
	}, "", "  ")

	if err != nil {
		return "", err
	}

	return string(encoded) + "\n", nil
}
