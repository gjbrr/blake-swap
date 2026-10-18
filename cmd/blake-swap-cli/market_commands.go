package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/platform"
)

// The market commands read relays, not nodes.
//
// They are separated from the engine commands for that reason: a person looking for an
// offer should not need a regtest datadir, two nodes and a wallet before they can see
// the market, and the reference's market commands do not require them either.
const (
	marketCommand      = "market"
	marketStateCommand = "market-state"
)

// isMarketCommand reports whether a command reads the market rather than a chain.
//
// The one switch that needs this is main's, which must decide whether building an
// engine is worth failing over. run's own dispatch lists the same two names; a test
// keeps the two from drifting.
func isMarketCommand(command string) bool {
	return command == marketCommand || command == marketStateCommand
}

// runMarketCommand dispatches a market read.
func runMarketCommand(ctx context.Context, command string, args []string) error {
	switch command {
	case marketCommand:
		return runMarket(ctx, args)
	case marketStateCommand:
		return runMarketState(ctx, args)
	}

	return fmt.Errorf("unsupported market command: %s", command)
}

// runMarket lists the offers that are open now.
func runMarket(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet(marketCommand, flag.ContinueOnError)

	limit := flags.Int("limit", 50, "how many offers to ask each relay for")

	if err := flags.Parse(args); err != nil {
		return err
	}

	report, err := readMarket(ctx, *limit)

	if err != nil {
		return err
	}

	fmt.Printf("Nostr market read: %d unique event(s) from %d/%d relay(s)\n",
		len(report.events), report.answered, report.relays)

	entries, err := marketflow.Book(report.events, time.Now())

	if err != nil {
		return err
	}

	fmt.Print(renderMarket(marketflow.OpenOffers(entries), time.Now().Unix()))

	return nil
}

// runMarketState emits the canonical state of every offer the evidence describes.
func runMarketState(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet(marketStateCommand, flag.ContinueOnError)

	limit := flags.Int("limit", 50, "how many offers to ask each relay for")
	asJSON := flags.Bool("json", false, "emit machine-readable JSON")

	if err := flags.Parse(args); err != nil {
		return err
	}

	report, err := readMarket(ctx, *limit)

	if err != nil {
		return err
	}

	entries, err := marketflow.Book(report.events, time.Now())

	if err != nil {
		return err
	}

	if *asJSON {
		text, err := marketStateJSON(entries)

		if err != nil {
			return err
		}

		fmt.Print(text)

		return nil
	}

	fmt.Print(renderMarketState(entries))

	return nil
}

// marketRead is one read's evidence and how much of the network answered.
type marketRead struct {
	events   []event.Event
	answered int
	relays   int
}

// readMarket fetches the market subscriptions, caches what it saw, and merges the
// cache back in.
//
// The merge is the point: relay retention is not the protocol's history, so an offer
// still inside the discovery window can lose the cancellation that settled it. Cached
// events are re-verified by Book, so a tampered file cannot change a state.
func readMarket(ctx context.Context, limit int) (marketRead, error) {
	client := marketClient()
	report, err := client.FetchReport(ctx, marketflow.MarketFilters(limit)...)

	if err != nil {
		return marketRead{}, err
	}

	// Keep what the read found. A failure to cache is reported rather than ignored:
	// silently losing evidence is how a client starts disagreeing with itself.
	for _, e := range report.Events {
		if err := client.Cache.Put(ctx, e); err != nil {
			return marketRead{}, err
		}
	}

	events := append([]event.Event{}, report.Events...)
	events = append(events, cachedEvents(ctx, client.Cache)...)

	return marketRead{events: events, answered: report.Answered, relays: report.Relays}, nil
}

// marketClient builds the relay client the market commands use.
func marketClient() *nostr.Client {
	return &nostr.Client{
		Dialer: dialRelay,
		Relays: relayList(),
		Cache:  dirCache{dir: filepath.Join(stateDirectory(), "nostr-cache")},
	}
}

// stateDirectory is where the reference keeps its records, and where this keeps its
// relay cache.
func stateDirectory() string {
	if dir := os.Getenv("BLAKE_SWAP_STATE_DIR"); dir != "" {
		return dir
	}

	home, err := os.UserHomeDir()

	if err != nil {
		return ".blake-swaps-v2"
	}

	return filepath.Join(home, ".blake-swaps-v2")
}

// dialRelay adapts the platform's socket to the relay client's Conn.
//
// The two types are deliberately separate: platform knows how to open a socket, the
// relay client knows what to send on one, and neither imports the other's vocabulary.
func dialRelay(ctx context.Context, url string) (nostr.Conn, error) {
	return platform.DialWebSocket(ctx, url)
}

// relayList reads the relays from the reference's two environment variables.
func relayList() []string {
	raw := os.Getenv("BLAKE_SWAP_NOSTR_RELAYS")

	if raw == "" {
		raw = os.Getenv("BLAKE_SWAP_NOSTR_RELAY")
	}

	if raw == "" {
		raw = "wss://nos.lol"
	}

	relays := make([]string, 0, 4)

	for _, part := range strings.Split(raw, ",") {
		if relay := strings.TrimSpace(part); relay != "" {
			relays = append(relays, relay)
		}
	}

	return relays
}
