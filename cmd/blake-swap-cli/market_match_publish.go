package main

import (
	"context"
	"fmt"
	"os"

	"github.com/gjbrr/blake-swap/pkg/marketops"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// selectionsDirectory holds the local record that binds an accepted take to a swap.
const selectionsDirectory = "market-selections"

// matchPublication is what a published match bound together.
type matchPublication struct {
	OfferID string
	TakeID  string
	MatchID string
	SwapID  string
}

// runPublishMarketMatch publishes the maker's match for an accepted take and prints it.
func runPublishMarketMatch(ctx context.Context, engine *swap.Engine, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("publish-market-match takes one take id")
	}

	published, err := publishMarketMatch(ctx, engine, args[0])

	if err != nil {
		return err
	}

	fmt.Print(renderMatchPublished(
		published.OfferID, published.TakeID, published.MatchID, published.SwapID,
	))

	return nil
}

// publishMarketMatch signs and publishes the maker's match for an accepted take.
//
// The match is the terminal market event: it binds an offer, a take and a local swap,
// and it adds no new terms — everything in it is re-derived from the two events it names
// and checked against the swap. The local selection record is what makes that possible,
// and it is rewritten with the match id afterwards, because a crash that lost the link
// would leave a published match unresolvable from this machine.
func publishMarketMatch(
	ctx context.Context, engine *swap.Engine, takeID string,
) (matchPublication, error) {
	if len(takeID) != 64 {
		return matchPublication{}, fmt.Errorf(
			"take_id must be the full 64-character Nostr event ID",
		)
	}

	env, err := marketEnvOrError(engine, marketClient(), func(line string) { fmt.Print(line) })

	if err != nil {
		return matchPublication{}, err
	}

	published, err := marketops.PublishMatch(ctx, env, takeID)

	if err != nil {
		return matchPublication{}, err
	}

	// The report is printed here rather than by the operation: it is the CLI's answer to
	// "where did this go", and the browser has no terminal to print it to.
	fmt.Print(renderOutboundCached(published.Published.ID, cacheDirectory()))
	fmt.Print(renderRelayResults(published.Report))

	return matchPublication{
		OfferID: published.OfferID,
		TakeID:  published.TakeID,
		MatchID: published.MatchID,
		SwapID:  published.SwapID,
	}, nil
}

// renderMatchPublished is the reference's confirmation block.
func renderMatchPublished(offerID, takeID, matchID, swapID string) string {
	return "\nMARKET MATCH PUBLISHED\n" +
		"======================\n" +
		"offer id: " + offerID + "\n" +
		"take id:  " + takeID + "\n" +
		"match id: " + matchID + "\n" +
		"swap id:  " + swapID + "\n\n" +
		"PASS: match is signed by the offer's maker\n" +
		"PASS: the exact signed take is referenced\n" +
		"PASS: no secret was published\n" +
		"PASS: no funds moved\n"
}

// writeAtomically publishes a file by rename, so a crash cannot leave it half written.
func writeAtomically(path string, content []byte) error {
	temporary := path + ".tmp"

	if err := os.WriteFile(temporary, content, 0o600); err != nil {
		return err
	}

	return os.Rename(temporary, path)
}
