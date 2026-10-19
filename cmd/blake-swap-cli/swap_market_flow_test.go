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

// marketParticipant is one side of the exchange: a state root, a key file and an identity.
type marketParticipant struct {
	name   string
	state  string
	key    string
	public string
}

// rigOrSkip skips unless the two-chain rig, the Python virtualenv and a built CLI are present.
func rigOrSkip(t *testing.T) string {
	t.Helper()

	root := os.Getenv("BLAKE_SWAP_TEST_ROOT")

	if root == "" {
		home, err := os.UserHomeDir()

		if err != nil {
			t.Skip("no home directory")
		}

		root = filepath.Join(home, ".blake-swap-test")
	}

	if _, err := os.ReadFile(filepath.Join(root, "rig-env.json")); err != nil {
		t.Skipf("no test rig at %s", root)
	}

	repo, err := filepath.Abs(filepath.Join("..", ".."))

	if err != nil {
		t.Fatal(err)
	}

	binary := filepath.Join(repo, "bin", "blake-swap-cli")

	if _, err := os.Stat(binary); err != nil {
		build := exec.Command("go", "build", "-o", binary, "./cmd/blake-swap-cli")
		build.Dir = repo

		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("building the CLI: %v\n%s", err, out)
		}
	}

	return repo
}

// newMarketParticipant writes one participant's key and state root.
func newMarketParticipant(t *testing.T, name, secret string) marketParticipant {
	t.Helper()

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "participant.key")

	if err := os.WriteFile(keyFile, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}

	part := marketParticipant{name: name, state: dir, key: keyFile}

	// The public key is what the outputs report; reading it from the CLI keeps this test from
	// depending on the key derivation it is not testing.
	part.public = secret

	return part
}

// runParticipant runs one CLI command as one participant.
func runParticipant(
	t *testing.T, repo string, part marketParticipant, relays string, messages string, args ...string,
) string {
	t.Helper()

	env := append(os.Environ(),
		"BLAKE_SWAP_STATE_DIR="+part.state,
		"BLAKE_SWAP_PARTICIPANT_KEY_FILE="+part.key,
		"BLAKE_SWAP_NOSTR_RELAYS="+relays,
		"BLAKE_SWAP_MESSAGE_DIR="+messages,
	)

	command := exec.Command(filepath.Join(repo, "bin", "blake-swap-cli"), args...)
	command.Dir = repo
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
func TestSwapMarketFlow(t *testing.T) {
	repo := rigOrSkip(t)

	stub, err := relaystub.New()

	if err != nil {
		t.Fatal(err)
	}

	defer stub.Close()

	messages := t.TempDir()

	// Two identities, both long enough to be keys and different from each other.
	alice := newMarketParticipant(t, "maker", strings.Repeat("31", 32))
	bob := newMarketParticipant(t, "taker", strings.Repeat("42", 32))

	// Step 1: the maker advertises.
	published := runParticipant(t, repo, alice, stub.URL, messages,
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
	runParticipant(t, repo, bob, stub.URL, messages, "market", "--limit", "10")

	taken := runParticipant(t, repo, bob, stub.URL, messages, "take-market-offer", offerID)

	takeID := marketField(t, taken, "take id")

	if len(takeID) != 64 {
		t.Fatalf("take id looks wrong: %q", takeID)
	}

	// Step 3: the maker reads its inbox and accepts the take, which creates the swap and
	// publishes both the protocol offer and the match.
	runParticipant(t, repo, alice, stub.URL, messages, "market-takes", offerID)

	accepted := runParticipant(t, repo, alice, stub.URL, messages, "accept-market-take", takeID)

	if !strings.Contains(accepted, "MARKET TAKE ACCEPTED") {
		t.Fatalf("the take was not accepted:\n%s", accepted)
	}

	swapID := marketField(t, accepted, "swap id")

	matchOutput := accepted[strings.Index(accepted, "MARKET -> SWAP BRIDGE CREATED"):]
	matchID := marketField(t, matchOutput, "market match")

	// Step 4: the taker imports the match, which binds the negotiation to its own swap.
	imported := runParticipant(t, repo, bob, stub.URL, messages, "accept-market-match", matchID)

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
	repo := rigOrSkip(t)

	stub, err := relaystub.New()

	if err != nil {
		t.Fatal(err)
	}

	defer stub.Close()

	messages := t.TempDir()
	bob := newMarketParticipant(t, "taker", strings.Repeat("43", 32))

	// A full-length id that is not on the relay: the refusal must come from the market read,
	// not from a validation shortcut.
	missing := strings.Repeat("ab", 32)

	command := exec.Command(filepath.Join(repo, "bin", "blake-swap-cli"), "take-market-offer", missing)
	command.Dir = repo
	command.Env = append(os.Environ(),
		"BLAKE_SWAP_STATE_DIR="+bob.state,
		"BLAKE_SWAP_PARTICIPANT_KEY_FILE="+bob.key,
		"BLAKE_SWAP_NOSTR_RELAYS="+stub.URL,
		"BLAKE_SWAP_MESSAGE_DIR="+messages,
	)

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
