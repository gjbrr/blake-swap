//go:build !js

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/internal/rigtest"
	"github.com/gjbrr/blake-swap/pkg/relaystub"
)

// This file mirrors the reference's `tests/swap_market_flow_tests.py`: the marketplace path
// driven end to end, by two participants, over a relay.
//
// The market is where most of the acceptance machinery lives, and it is the one path that
// needs no chain until the trade is agreed: publish an offer, take it, accept the take, import
// the match. Reproducing it here means the market half of that suite runs in Go, against this
// implementation on both sides.
//
// Settlement after the match is the direct path, which `pkg/swap`'s cross-implementation tests
// already walk against the reference.

// marketParticipant is one side of the exchange: a state root, a key file, a role and an
// identity.
//
// The role matters because the reference's market flow has the maker act as protocol Alice
// and the taker as protocol Bob, and the chain wallets are named after the role.
type marketParticipant struct {
	name   string
	role   string
	state  string
	key    string
	public string
}

// newMarketParticipant writes one participant's key and state root.
func newMarketParticipant(t *testing.T, name, role, secret string) marketParticipant {
	t.Helper()

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "participant.key")

	if err := os.WriteFile(keyFile, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}

	return marketParticipant{name: name, role: role, state: dir, key: keyFile, public: secret}
}

// runParticipant runs one CLI command as one participant.
//
// The chain configuration comes from internal/rigtest, which is also what proves the rig is
// running and that the wallets this path reads exist. Only the relay and the message spool
// are specific to the market.
func runParticipant(
	t *testing.T, r *rigtest.Rig, part marketParticipant, relays string, messages string,
	args ...string,
) string {
	t.Helper()

	side := &rigtest.Side{
		Name: "go", Role: part.role, State: part.state, KeyFile: part.key,
	}

	env := append(r.Env(side, messages), "BLAKE_SWAP_NOSTR_RELAYS="+relays)

	command := exec.Command(r.CLI, args...)
	command.Dir = r.Repo
	command.Env = env

	out, err := command.CombinedOutput()

	if err != nil {
		t.Fatalf("%s %v: %v\n%s", part.name, args, err, out)
	}

	return string(out)
}

// field reads one "label: value" line from a command's output.
func marketField(t *testing.T, output, label string) string {
	t.Helper()

	pattern := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(label) + `:\s*(\S+)\s*$`)
	match := pattern.FindStringSubmatch(output)

	if len(match) != 2 {
		t.Fatalf("no %q in output:\n%s", label, output)
	}

	return match[1]
}

// TestSwapMarketFlow runs the marketplace path between two participants over a local relay.
// Parity: tests/swap_market_flow_tests.py, tests/swap_reverse_market_flow_tests.py
func TestSwapMarketFlow(t *testing.T) {
	r := rigtest.Load(t)

	stub, err := relaystub.New()

	if err != nil {
		t.Fatal(err)
	}

	defer stub.Close()

	messages := t.TempDir()

	// Two identities, both long enough to be keys and different from each other.
	alice := newMarketParticipant(t, "maker", "alice", strings.Repeat("31", 32))
	bob := newMarketParticipant(t, "taker", "bob", strings.Repeat("42", 32))

	// Step 1: the maker advertises.
	published := runParticipant(t, r, alice, stub.URL, messages,
		"publish-market-offer", "--blake", "10", "--sats", "50000", "--minutes", "30")

	offerID := marketField(t, published, "offer id")

	if len(offerID) != 64 {
		t.Fatalf("offer id looks wrong: %q", offerID)
	}

	// The offer is on the relay, which is the only shared place it needs to be.
	var onRelay bool

	for _, stored := range stub.Events() {
		if stored.ID == offerID {
			onRelay = true
		}
	}

	if !onRelay {
		t.Fatal("the offer was not published to the relay")
	}

	// Step 2: the taker reads the market and takes the offer. Reading first is what fills the
	// taker's cache, which is how a prefix resolves — the same order a person uses.
	runParticipant(t, r, bob, stub.URL, messages, "market", "--limit", "10")

	taken := runParticipant(t, r, bob, stub.URL, messages, "take-market-offer", offerID)

	takeID := marketField(t, taken, "take id")

	if len(takeID) != 64 {
		t.Fatalf("take id looks wrong: %q", takeID)
	}

	// Step 3: the maker reads its inbox and accepts the take, which creates the swap and
	// publishes both the protocol offer and the match.
	runParticipant(t, r, alice, stub.URL, messages, "market-takes", offerID)

	accepted := runParticipant(t, r, alice, stub.URL, messages, "accept-market-take", takeID)

	if !strings.Contains(accepted, "MARKET TAKE ACCEPTED") {
		t.Fatalf("the take was not accepted:\n%s", accepted)
	}

	swapID := marketField(t, accepted, "swap id")

	matchOutput := accepted[strings.Index(accepted, "MARKET -> SWAP BRIDGE CREATED"):]
	matchID := marketField(t, matchOutput, "market match")

	// Step 4: the taker imports the match, which binds the negotiation to its own swap.
	imported := runParticipant(t, r, bob, stub.URL, messages, "accept-market-match", matchID)

	if !strings.Contains(imported, "MARKET MATCH IMPORTED") {
		t.Fatalf("the match was not imported:\n%s", imported)
	}

	if !strings.Contains(imported, swapID) {
		t.Fatalf("the imported swap is not %s:\n%s", swapID, imported)
	}

	// Both sides now hold a record for the same swap, and neither holds the other's secret.
	for _, part := range []marketParticipant{alice, bob} {
		raw, err := os.ReadFile(filepath.Join(part.state, swapID+".json"))

		if err != nil {
			t.Fatalf("%s has no record for %s: %v", part.name, swapID, err)
		}

		var record map[string]any

		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatal(err)
		}

		if record["state"] != "OFFERED" {
			t.Errorf("%s: state = %v, want OFFERED", part.name, record["state"])
		}

		if part.name == "taker" && record["secret_hex"] != nil {
			t.Error("the taker holds a secret it must not have")
		}
	}
}

// TestSwapMarketFlowRefusesAnUnopenedOffer: a take of something that is not open is refused,
// and the refusal is the reference's.
func TestSwapMarketFlowRefusesAnUnopenedOffer(t *testing.T) {
	r := rigtest.Load(t)

	stub, err := relaystub.New()

	if err != nil {
		t.Fatal(err)
	}

	defer stub.Close()

	messages := t.TempDir()
	bob := newMarketParticipant(t, "taker", "bob", strings.Repeat("43", 32))

	// A full-length id that is not on the relay: the refusal must come from the market read,
	// not from a validation shortcut.
	missing := strings.Repeat("ab", 32)

	side := &rigtest.Side{Name: "go", Role: bob.role, State: bob.state, KeyFile: bob.key}

	command := exec.Command(r.CLI, "take-market-offer", missing)
	command.Dir = r.Repo
	command.Env = append(r.Env(side, messages), "BLAKE_SWAP_NOSTR_RELAYS="+stub.URL)

	out, err := command.CombinedOutput()

	if err == nil {
		t.Fatalf("a take of nothing was accepted:\n%s", out)
	}

	if !strings.Contains(string(out), "Offer is not currently open") {
		t.Fatalf("the refusal is not the reference's:\n%s", out)
	}
}

// unused keeps fmt imported for the helpers above.
var _ = fmt.Sprint
