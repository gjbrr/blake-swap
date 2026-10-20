//go:build !js

package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gjbrr/blake-swap/internal/rigtest"
	"github.com/gjbrr/blake-swap/pkg/relaystub"
)

// Parity: tests/swap_market_flow_tests.py, tests/swap_market_takes_json_tests.py
//
// The market commands talk to a relay rather than a swap record, so each
// implementation gets its own loopback relay: a shared one would let one side see
// the other's events and the offer counts would not be comparable.
//
// The walk is the whole negotiation rather than the read commands alone, because the
// relay chatter and the publish confirmations are most of what these commands print:
// an event is only comparable if the lines that say where it went are compared too.

// marketWalk runs the market command group for one implementation.
func marketWalk(t *testing.T, r *rigtest.Rig, impl string) map[string]string {
	t.Helper()

	stub, err := relaystub.New()

	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(stub.Close)

	participant := func(role string) *rigtest.Side {
		dir := t.TempDir()
		key := filepath.Join(dir, role+".key")

		secret := make([]byte, 32)

		if _, err := rand.Read(secret); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(key, []byte(hex.EncodeToString(secret)), 0o600); err != nil {
			t.Fatal(err)
		}

		return &rigtest.Side{Name: impl, Role: role, State: dir, KeyFile: key}
	}

	alice := participant("alice")
	spool := t.TempDir()

	env := []string{"BLAKE_SWAP_NOSTR_RELAYS=" + stub.URL}

	outputs := map[string]string{}

	run := func(name string, side *rigtest.Side, args ...string) string {
		output, exit, err := r.Exec(t, side, spool, env, args...)

		if err != nil {
			t.Fatalf("%s %v: %v", impl, args, err)
		}

		if exit != 0 {
			t.Fatalf("%s %v failed with exit %d:\n%s", impl, args, exit, output)
		}

		outputs[name] = output

		return output
	}

	published := run("publish-market-offer", alice,
		"publish-market-offer", "--blake", "10", "--sats", "50000", "--minutes", "30")

	// The offer id is read out to prove the command produced one; the reads below
	// discover it from the relay, which is what a participant does.
	offerID := marketValue(t, published, "offer id")

	if len(offerID) != 64 {
		t.Fatalf("%s: offer id is %q", impl, offerID)
	}

	run("market", alice, "market", "--limit", "10")
	run("market-state", alice, "market-state", "--limit", "10")
	run("market-state json", alice, "market-state", "--limit", "10", "--json")

	//
	// The negotiation, in the order the two participants would run it: Bob takes the
	// offer, Alice inspects the take, reads her inbox, and accepts; Bob reads the match;
	// and the maker withdraws what is left of the offer. Every step is compared,
	// including the relay chatter, because that chatter is what tells an operator which
	// relays answered.
	//
	bob := participant("bob")

	taken := run("take-market-offer", bob, "take-market-offer", offerID)
	takeID := marketValue(t, taken, "take id")

	if len(takeID) != 64 {
		t.Fatalf("%s: take id is %q", impl, takeID)
	}

	run("inspect-market-take", bob, "inspect-market-take", offerID)
	run("market-takes", alice, "market-takes", offerID)
	run("market-takes json", alice, "market-takes", offerID, "--json")
	accepted := run("accept-market-take", alice, "accept-market-take", takeID)
	matchID := marketValue(t, accepted[bridgeOffset(t, accepted):], "market match")

	run("accept-market-match", bob, "accept-market-match", matchID)
	run("market-matches", bob, "market-matches")
	run("cancel-market-offer", alice, "cancel-market-offer", offerID)

	//
	// The reverse direction, with a second pair of participants. Here the offer's maker is
	// protocol Bob — the side that funds BTC — so he accepts the take, reserves the swap id
	// and publishes the match without ever creating the secret; the taker is protocol Alice,
	// who creates the swap and publishes her protocol offer; and Bob then imports that offer
	// and sends his accept. Every step is compared, including the duplicate import, because
	// the retry is the path a crash lands in.
	//
	reverseBob := participant("bob")
	reverseAlice := participant("alice")

	reverseOffer := run("reverse publish-market-offer", reverseBob,
		"publish-market-offer", "--blake", "5", "--sats", "25000", "--minutes", "30",
		"--direction", "btc-for-blake")

	reverseOfferID := marketValue(t, reverseOffer, "offer id")

	if len(reverseOfferID) != 64 {
		t.Fatalf("%s: reverse offer id is %q", impl, reverseOfferID)
	}

	run("reverse market", reverseBob, "market", "--limit", "10")

	reverseTaken := run("reverse take-market-offer", reverseAlice,
		"take-market-offer", reverseOfferID)

	reverseTakeID := marketValue(t, reverseTaken, "take id")

	run("reverse market-takes", reverseBob, "market-takes", reverseOfferID)

	reverseAccepted := run("reverse accept-market-take", reverseBob,
		"accept-market-take", reverseTakeID)

	reverseMatchID := marketValue(t, reverseAccepted, "match id")
	reverseSwapID := marketValue(t, reverseAccepted, "swap id")

	run("reverse accept-reverse-market-match", reverseAlice,
		"accept-reverse-market-match", reverseMatchID)
	run("reverse publish-offer", reverseAlice, "publish-offer", reverseSwapID)
	run("reverse accept-reverse-swap-offer", reverseBob,
		"accept-reverse-swap-offer", reverseMatchID)
	run("reverse accept-reverse-swap-offer again", reverseBob,
		"accept-reverse-swap-offer", reverseMatchID)
	//
	// The listing itself is compared in the forward phase, where both implementations
	// cache the same events. A reverse `market-matches` would compare which events each
	// client had already cached rather than what the command does: the port caches every
	// event it publishes, so it resolves the match's offer and take locally while the
	// reference fetches them and reports where from.
	//

	return outputs
}

// bridgeOffset is where the market-to-swap bridge block starts, which is where the match
// id is reported.
func bridgeOffset(t *testing.T, output string) int {
	t.Helper()

	at := indexOf(output, "MARKET -> SWAP BRIDGE CREATED")

	if at < 0 {
		t.Fatalf("no bridge block in output:\n%s", output)
	}

	return at
}

// marketValue reads one "label: value" line from a command's output.
func marketValue(t *testing.T, output, label string) string {
	t.Helper()

	for _, line := range splitLines(output) {
		trimmed := trimSpace(line)

		if len(trimmed) > len(label) && trimmed[:len(label)+1] == label+":" {
			return trimSpace(trimmed[len(label)+1:])
		}
	}

	t.Fatalf("no %q in output:\n%s", label, output)

	return ""
}

// TestCLIParityMarketWalk compares the market command group's output.
func TestCLIParityMarketWalk(t *testing.T) {
	r := rigtest.Load(t)

	reference := marketWalk(t, r, "python")
	port := marketWalk(t, r, "go")

	for name, want := range reference {
		got, present := port[name]

		if !present {
			t.Errorf("%s: the port produced no output for this step", name)

			continue
		}

		if normalise(want) != normalise(got) {
			t.Errorf("%s differs\n--- reference ---\n%s\n--- port ---\n%s",
				name, normalise(want), normalise(got))
		}
	}
}

// splitLines and trimSpace keep this file's imports to what it uses.
func splitLines(text string) []string {
	var lines []string

	start := 0

	for index := 0; index < len(text); index++ {
		if text[index] == '\n' {
			lines = append(lines, text[start:index])
			start = index + 1
		}
	}

	if start < len(text) {
		lines = append(lines, text[start:])
	}

	return lines
}

// trimSpace trims the ASCII whitespace a command line uses.
func trimSpace(text string) string {
	for len(text) > 0 && (text[0] == ' ' || text[0] == '\t') {
		text = text[1:]
	}

	for len(text) > 0 && (text[len(text)-1] == ' ' || text[len(text)-1] == '\t' ||
		text[len(text)-1] == '\r') {
		text = text[:len(text)-1]
	}

	return text
}

// unusedFmt keeps the fmt import honest if the file stops needing it.
var _ = fmt.Sprint

// indexOf is strings.Index, kept here so this file's imports stay to what it uses.
func indexOf(haystack, needle string) int {
	for at := 0; at+len(needle) <= len(haystack); at++ {
		if haystack[at:at+len(needle)] == needle {
			return at
		}
	}

	return -1
}
