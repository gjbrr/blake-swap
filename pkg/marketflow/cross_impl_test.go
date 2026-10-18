//go:build !js

package marketflow_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/nostr"
)

// Market advertisements are the one thing two implementations exchange with no
// chain in between: an offer is a signed event, and the other side must be able to
// read it or the trade never starts. So both directions of the event contract are
// tested against the Python reference — Go builds and Python parses, then Python
// builds and Go parses.

// repoRoot finds the checkout.
func repoRoot(t *testing.T) string {
	t.Helper()

	root, err := filepath.Abs(filepath.Join("..", ".."))

	if err != nil {
		t.Fatal(err)
	}

	return root
}

// pythonPath returns the reference interpreter, or skips.
func pythonPath(t *testing.T) string {
	t.Helper()

	python := filepath.Join(repoRoot(t), ".venv", "bin", "python")

	if _, err := os.Stat(python); err != nil {
		t.Skip("the Python virtual environment is not present")
	}

	return python
}

// runPython executes a script with the reference engine importable.
func runPython(t *testing.T, script string, env []string) string {
	t.Helper()

	command := exec.Command(pythonPath(t), "-c", script)
	command.Dir = repoRoot(t)
	command.Env = append(os.Environ(), env...)

	output, err := command.CombinedOutput()

	if err != nil {
		t.Fatalf("python failed: %v\n%s", err, output)
	}

	return string(output)
}

// keyFile writes a participant key and returns its path and hex.
func keyFile(t *testing.T) (string, string) {
	t.Helper()

	secret := strings.Repeat("11", 32)
	path := filepath.Join(t.TempDir(), "participant.key")

	if err := os.WriteFile(path, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}

	return path, secret
}

// TestGoOfferIsReadableByPython builds an offer in Go and parses it with the
// reference implementation.
func TestGoOfferIsReadableByPython(t *testing.T) {
	keyPath, secret := keyFile(t)

	key, err := nostr.NewKey(secret)

	if err != nil {
		t.Fatal(err)
	}

	built, _, err := marketflow.BuildOffer(key, marketflow.BuildOfferOptions{
		BlakeAmount: amount.Blake(100_000),
		BTCSats:     25_000,
		Minutes:     30,
		Direction:   market.BTCForBlake,
		CreatedAt:   time.Now().Unix(),
	})

	if err != nil {
		t.Fatal(err)
	}

	encoded, err := json.Marshal(built)

	if err != nil {
		t.Fatal(err)
	}

	script := fmt.Sprintf(`
import json, sys
sys.path.insert(0, %q)
import swap_v2
event = json.loads(%q)
message, offer = swap_v2.nostr_parse_market_offer_event(event)
print("parsed", offer["direction"], offer["blake_amount"], offer["btc_sats"], offer["maker_role"])
`, filepath.Join(repoRoot(t), "src"), string(encoded))

	output := runPython(t, script, []string{
		"BLAKE_SWAP_PARTICIPANT_KEY_FILE=" + keyPath,
		"BLAKE_SWAP_ROLE=alice",
	})

	if !strings.Contains(output, "parsed btc-for-blake") {
		t.Fatalf("Python did not read the Go offer:\n%s", output)
	}

	if !strings.Contains(output, "100000") && !strings.Contains(output, "0.001") {
		t.Fatalf("Python read different economics:\n%s", output)
	}

	if !strings.Contains(output, "25000") {
		t.Fatalf("Python read a different Bitcoin amount:\n%s", output)
	}
}

// TestPythonOfferIsReadableByGo builds an offer with the reference implementation
// and parses it here.
func TestPythonOfferIsReadableByGo(t *testing.T) {
	keyPath, secret := keyFile(t)

	script := fmt.Sprintf(`
import json, sys
sys.path.insert(0, %q)
import swap_v2
event, message = swap_v2.build_market_offer_v2_event(0.002, 30000, 15, "blake-for-btc")
print(json.dumps(event))
`, filepath.Join(repoRoot(t), "src"))

	output := runPython(t, script, []string{
		"BLAKE_SWAP_PARTICIPANT_KEY_FILE=" + keyPath,
		"BLAKE_SWAP_ROLE=alice",
	})

	start := strings.Index(output, "{")
	end := strings.LastIndex(output, "}")

	if start < 0 || end <= start {
		t.Fatalf("no event in Python output:\n%s", output)
	}

	var built event.Event

	if err := json.Unmarshal([]byte(output[start:end+1]), &built); err != nil {
		t.Fatalf("Python event is not decodable: %v\n%s", err, output)
	}

	key, err := nostr.NewKey(secret)

	if err != nil {
		t.Fatal(err)
	}

	if built.Pubkey != key.Public {
		t.Fatalf("the reference signed as %s, the key is %s", built.Pubkey, key.Public)
	}

	offer, err := marketflow.ParseOffer(built, time.Now())

	if err != nil {
		t.Fatalf("the Go parser refused a reference offer: %v", err)
	}

	if offer.Direction != market.BlakeForBTC {
		t.Fatalf("direction = %q", offer.Direction)
	}

	if offer.BTCSats != 30_000 {
		t.Fatalf("btc_sats = %d", offer.BTCSats)
	}

	if offer.MakerRole != "alice" || offer.TakerRole != "bob" {
		t.Fatalf("roles = %s/%s", offer.MakerRole, offer.TakerRole)
	}
}
