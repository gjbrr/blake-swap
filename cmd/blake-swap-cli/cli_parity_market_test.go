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
// Not yet identical, and therefore not compared here: the relay-chatter lines the
// fetching commands print (`Nostr event found on: ...`), the `MARKET MATCH PUBLISHED`
// block inside `accept-market-take`, the `OFFER ACCEPTED` block inside
// `accept-market-match`, and the take-listing commands. Those are listed in
// docs/GO_VERIFICATION.md as the remaining output gap; the walk compares what is
// identical so a regression there is still caught.

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
	if id := marketValue(t, published, "offer id"); len(id) != 64 {
		t.Fatalf("%s: offer id is %q", impl, id)
	}

	run("market", alice, "market", "--limit", "10")
	run("market-state", alice, "market-state", "--limit", "10")
	run("market-state json", alice, "market-state", "--limit", "10", "--json")

	return outputs
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
