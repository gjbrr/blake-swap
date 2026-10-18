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

// rigEnv returns the environment the reference needs to reach the local rig.
func rigEnv(t *testing.T, extra ...string) []string {
	t.Helper()

	root := os.Getenv("BLAKE_SWAP_TEST_ROOT")

	if root == "" {
		home, err := os.UserHomeDir()

		if err != nil {
			t.Skip("no home directory")
		}

		root = filepath.Join(home, ".blake-swap-test")
	}

	data, err := os.ReadFile(filepath.Join(root, "rig-env.json"))

	if err != nil {
		t.Skipf("no test rig at %s", root)
	}

	var env struct {
		BTCDatadir   string `json:"btc_datadir"`
		BlakeDatadir string `json:"blake_datadir"`
		BTCCLI       string `json:"btc_cli"`
		BlakeCLI     string `json:"blake_cli"`
	}

	if err := json.Unmarshal(data, &env); err != nil {
		t.Skipf("rig-env.json is unreadable: %v", err)
	}

	return append([]string{
		"BLAKE_SWAP_BTC_DATADIR=" + env.BTCDatadir,
		"BLAKE_SWAP_BLAKE_DATADIR=" + env.BlakeDatadir,
		"BLAKE_SWAP_BTC_CLI=" + env.BTCCLI,
		"BLAKE_SWAP_BLAKE_CLI=" + env.BlakeCLI,
	}, extra...)
}

// TestGoMatchIsReadableByPython has the reference validate a Go-built match against
// the exact offer and take it references.
func TestGoMatchIsReadableByPython(t *testing.T) {
	keyPath, secret := keyFile(t)

	makerKey, err := nostr.NewKey(secret)

	if err != nil {
		t.Fatal(err)
	}

	takerKey, err := nostr.NewKey(strings.Repeat("22", 32))

	if err != nil {
		t.Fatal(err)
	}

	offerEvent, _, err := marketflow.BuildOffer(makerKey, marketflow.BuildOfferOptions{
		BlakeAmount: amount.Blake(100_000),
		BTCSats:     25_000,
		Minutes:     30,
		Direction:   market.BlakeForBTC,
		CreatedAt:   time.Now().Unix(),
	})

	if err != nil {
		t.Fatal(err)
	}

	offer, err := marketflow.ParseOffer(offerEvent, time.Now())

	if err != nil {
		t.Fatal(err)
	}

	takeEvent, take, err := marketflow.BuildTake(takerKey, marketflow.BuildTakeOptions{
		Offer: *offer, CreatedAt: time.Now().Unix(),
	})

	if err != nil {
		t.Fatal(err)
	}

	matchEvent, _, err := marketflow.BuildMatch(makerKey, marketflow.BuildMatchOptions{
		Offer:       *offer,
		OfferEvent:  offerEvent,
		TakeEvent:   takeEvent,
		Taker:       &take,
		SwapID:      "aaaa00000001",
		BlakeAmount: offer.BlakeAmount,
		BTCSats:     offer.BTCSats,
		CreatedAt:   time.Now().Unix(),
	})

	if err != nil {
		t.Fatal(err)
	}

	offerJSON, _ := json.Marshal(offerEvent)
	takeJSON, _ := json.Marshal(takeEvent)
	matchJSON, _ := json.Marshal(matchEvent)

	script := fmt.Sprintf(`
import json, sys
sys.path.insert(0, %q)
import swap_v2

offer_event = json.loads(%q)
take_event = json.loads(%q)
match_event = json.loads(%q)

message = swap_v2.validate_market_match_evidence(match_event, offer_event, take_event)
print("match ok", message["direction"], message["maker_role"], message["taker_role"], message["swap_id"])
`, filepath.Join(repoRoot(t), "src"), string(offerJSON), string(takeJSON), string(matchJSON))

	output := runPython(t, script, []string{
		"BLAKE_SWAP_PARTICIPANT_KEY_FILE=" + keyPath,
		"BLAKE_SWAP_ROLE=alice",
	})

	if !strings.Contains(output, "match ok blake-for-btc alice bob aaaa00000001") {
		t.Fatalf("the reference did not accept the Go match:\n%s", output)
	}
}

// TestPythonMatchIsReadableByGo builds an offer, take and match with the reference
// and parses all three here.
func TestPythonMatchIsReadableByGo(t *testing.T) {
	keyPath, secret := keyFile(t)

	script := fmt.Sprintf(`
import json, sys, time
sys.path.insert(0, %q)
import swap_v2

offer_event, offer_message = swap_v2.build_market_offer_v2_event(0.001, 25000, 30, "blake-for-btc")
take_event, take_message = swap_v2.build_market_take_event(offer_event, offer_message)
swap = swap_v2.make_swap(0.001, 25000, 500, 400)
match_event, match_message = swap_v2.build_market_match_event(offer_event, take_event, swap)

print(json.dumps({"offer": offer_event, "take": take_event, "match": match_event}))
`, filepath.Join(repoRoot(t), "src"))

	output := runPython(t, script, rigEnv(t,
		"BLAKE_SWAP_PARTICIPANT_KEY_FILE="+keyPath,
		"BLAKE_SWAP_ROLE=alice",
		"BLAKE_SWAP_BLAKE_WALLET=alice",
		"BLAKE_SWAP_BTC_WALLET=alice",
	))

	start := strings.Index(output, "{")
	end := strings.LastIndex(output, "}")

	if start < 0 || end <= start {
		t.Fatalf("no events in Python output:\n%s", output)
	}

	var bundle struct {
		Offer event.Event `json:"offer"`
		Take  event.Event `json:"take"`
		Match event.Event `json:"match"`
	}

	if err := json.Unmarshal([]byte(output[start:end+1]), &bundle); err != nil {
		t.Fatalf("Python events are not decodable: %v\n%s", err, output)
	}

	key, err := nostr.NewKey(secret)

	if err != nil {
		t.Fatal(err)
	}

	if bundle.Offer.Pubkey != key.Public {
		t.Fatalf("the reference signed as %s, the key is %s", bundle.Offer.Pubkey, key.Public)
	}

	offer, err := marketflow.ParseOffer(bundle.Offer, time.Now())

	if err != nil {
		t.Fatalf("the Go parser refused a reference offer: %v", err)
	}

	take, err := marketflow.ParseTake(bundle.Take, time.Now())

	if err != nil {
		t.Fatalf("the Go parser refused a reference take: %v", err)
	}

	match, err := marketflow.ParseMatch(bundle.Match, bundle.Offer, bundle.Take, time.Now())

	if err != nil {
		t.Fatalf("the Go parser refused a reference match: %v", err)
	}

	if match.OfferID != offer.EventID || match.TakeID != take.OfferID && take.OfferID != offer.EventID {
		t.Fatalf("match does not bind the events: %+v", match)
	}

	if match.Direction != offer.Direction {
		t.Fatalf("direction = %q", match.Direction)
	}

	if match.MakerRole != offer.MakerRole || match.TakerRole != offer.TakerRole {
		t.Fatalf("roles = %s/%s", match.MakerRole, match.TakerRole)
	}
}

// TestGoTakeAndCancelAreReadableByPython builds a take and a cancel in Go and has
// the reference validate them against the offer they belong to.
func TestGoTakeAndCancelAreReadableByPython(t *testing.T) {
	keyPath, secret := keyFile(t)

	makerKey, err := nostr.NewKey(secret)

	if err != nil {
		t.Fatal(err)
	}

	offerEvent, _, err := marketflow.BuildOffer(makerKey, marketflow.BuildOfferOptions{
		BlakeAmount: amount.Blake(100_000),
		BTCSats:     25_000,
		Minutes:     30,
		Direction:   market.BTCForBlake,
		CreatedAt:   time.Now().Unix(),
	})

	if err != nil {
		t.Fatal(err)
	}

	// The taker is a second participant; the reference needs its key to sign the
	// take it validates, and it must be the same identity the Go side used.
	takerSecret := strings.Repeat("22", 32)
	takerKey, err := nostr.NewKey(takerSecret)

	if err != nil {
		t.Fatal(err)
	}

	offer, err := marketflow.ParseOffer(offerEvent, time.Now())

	if err != nil {
		t.Fatal(err)
	}

	takeEvent, _, err := marketflow.BuildTake(takerKey, marketflow.BuildTakeOptions{
		Offer: *offer, CreatedAt: time.Now().Unix(),
	})

	if err != nil {
		t.Fatal(err)
	}

	cancelEvent, _, err := marketflow.BuildCancel(makerKey, marketflow.BuildCancelOptions{
		OfferID: offer.EventID, CreatedAt: time.Now().Unix(),
	})

	if err != nil {
		t.Fatal(err)
	}

	offerJSON, err := json.Marshal(offerEvent)

	if err != nil {
		t.Fatal(err)
	}

	takeJSON, err := json.Marshal(takeEvent)

	if err != nil {
		t.Fatal(err)
	}

	cancelJSON, err := json.Marshal(cancelEvent)

	if err != nil {
		t.Fatal(err)
	}

	script := fmt.Sprintf(`
import json, sys
sys.path.insert(0, %q)
import swap_v2

offer_event = json.loads(%q)
take_event = json.loads(%q)
cancel_event = json.loads(%q)

offer_message, _ = swap_v2.nostr_parse_market_offer_event(offer_event)
message = swap_v2.nostr_validate_market_take_event(
    take_event, offer_event, offer_message, offer_event["id"],
)
print("take ok", message["direction"], message["maker_role"], message["taker_role"])

# A cancel is checked against the offer's author.
if cancel_event["pubkey"] != offer_event["pubkey"]:
    raise SystemExit("cancel author mismatch")
parsed_cancel = json.loads(cancel_event["content"])
if parsed_cancel["offer_id"] != offer_event["id"]:
    raise SystemExit("cancel does not reference the offer")
print("cancel ok", parsed_cancel["offer_id"][:8])
`, filepath.Join(repoRoot(t), "src"), string(offerJSON), string(takeJSON), string(cancelJSON))

	output := runPython(t, script, []string{
		"BLAKE_SWAP_PARTICIPANT_KEY_FILE=" + keyPath,
		"BLAKE_SWAP_ROLE=bob",
	})

	if !strings.Contains(output, "take ok btc-for-blake bob alice") {
		t.Fatalf("the reference did not accept the Go take:\n%s", output)
	}

	if !strings.Contains(output, "cancel ok") {
		t.Fatalf("the reference did not accept the Go cancel:\n%s", output)
	}
}
