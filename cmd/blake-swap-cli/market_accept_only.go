package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// runAcceptTake accepts the only valid take of an offer, or refuses to guess.
//
// Choosing which counterparty to trade with is a decision, and this command must not
// make it: with more than one valid take it lists them and stops. The single-take case is
// the one where there is nothing to decide, so it hands the take to the ordinary
// acceptance path rather than reimplementing it — and the reference re-reads the take
// there, announcing the same evidence twice, which is what this does too.
func runAcceptTake(ctx context.Context, engine *swap.Engine, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("accept-take takes one offer id")
	}

	client := marketClient()

	offerID, err := resolveCachedOfferPrefix(ctx, client, args[0])

	if err != nil {
		return err
	}

	set, err := readTakeSet(ctx, client, offerID, 200, nil, false, false)

	if err != nil {
		return err
	}

	if len(set.takes) == 0 {
		return fmt.Errorf("No valid takes currently exist for this offer")
	}

	if len(set.takes) > 1 {
		fmt.Print(renderMultipleValidTakes(offerID, set.takes))

		return nil
	}

	fmt.Print(renderOneValidTake(offerID, set.takes[0].EventID))

	return runAcceptMarketTake(ctx, engine, []string{set.takes[0].EventID})
}

// renderMultipleValidTakes is the reference's refusal to choose, with every candidate.
func renderMultipleValidTakes(offerID string, takes []marketflow.TakeEntry) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("MULTIPLE VALID TAKES\n")
	out.WriteString("====================\n")
	fmt.Fprintf(&out, "offer: %s\n", offerID)
	out.WriteString("\n")
	out.WriteString("More than one counterparty wants this offer. Refusing to choose automatically.\n")
	out.WriteString("\n")

	for index, take := range takes {
		fmt.Fprintf(&out, "%d. %s  taker=%s\n",
			index+1, truncate(take.EventID, 12), truncate(take.Taker, 12),
		)
	}

	out.WriteString("\n")
	out.WriteString("Use accept-market-take with the chosen take ID/prefix.\n")

	return out.String()
}

// renderOneValidTake announces the one take there is nothing to decide about.
func renderOneValidTake(offerID, takeID string) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("ONE VALID TAKE FOUND\n")
	out.WriteString("====================\n")
	fmt.Fprintf(&out, "offer: %s\n", offerID)
	fmt.Fprintf(&out, "take:  %s\n", takeID)
	out.WriteString("\n")
	out.WriteString("Accepting the only valid take.\n")

	return out.String()
}
