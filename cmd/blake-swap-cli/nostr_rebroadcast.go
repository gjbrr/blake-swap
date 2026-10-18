package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gjbrr/blake-swap/pkg/event"
)

// runRebroadcastNostrEvent republishes one cached event, byte for byte.
//
// The point is that nothing is rebuilt: the cached event is re-verified and sent again
// exactly as it was signed. Re-signing would produce a different event with a different
// id, which is a different statement — and a relay that already has the first one would
// hold two.
func runRebroadcastNostrEvent(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("rebroadcast-nostr-event takes one event id")
	}

	requested := strings.ToLower(strings.TrimSpace(args[0]))

	cache := dirCache{dir: cacheDirectory()}
	cached, ok, err := cache.Get(ctx, requested)

	if err != nil {
		return err
	}

	if !ok {
		return fmt.Errorf("No verified cached Nostr event: %s", requested)
	}

	// The cache is re-verified rather than trusted: a file on disk is not evidence.
	if err := event.Verify(cached, time.Now()); err != nil {
		return fmt.Errorf("No verified cached Nostr event: %s", requested)
	}

	if cached.ID != requested {
		return fmt.Errorf("SECURITY FAILURE: cached event ID does not match requested event ID")
	}

	fmt.Print(renderRebroadcasting(requested))

	client := marketClient()

	report, err := client.Publish(ctx, cached)

	if err != nil {
		return err
	}

	if report.EventID != requested {
		return fmt.Errorf("SECURITY FAILURE: rebroadcast changed the Nostr event ID")
	}

	fmt.Print(renderRebroadcast(requested))

	return nil
}

// renderRebroadcasting is the block printed before the republish.
func renderRebroadcasting(id string) string {
	return "\nREBROADCASTING CACHED NOSTR EVENT\n" +
		"=================================\n" +
		"event id: " + id + "\n\n" +
		"PASS: cached event re-verified\n" +
		"PASS: existing signature will be reused\n" +
		"PASS: event will NOT be rebuilt or re-signed\n"
}

// renderRebroadcast is the block printed once a relay has the event.
func renderRebroadcast(id string) string {
	return "\nNOSTR EVENT REBROADCAST\n" +
		"=======================\n" +
		"event id: " + id + "\n\n" +
		"PASS: exact signed event was rebroadcast\n" +
		"PASS: event ID remained unchanged\n"
}
