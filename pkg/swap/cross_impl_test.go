//go:build !js

package swap_test

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/keys"
)

// These tests run the Go engine against the Python reference as the counterparty,
// in both directions, over a shared file-transport spool and the local regtest rig.
//
// They are the honest answer to "is this a port or a rewrite?". A shared spool means
// every message the Go side writes is parsed by the Python engine and vice versa,
// and every record each side writes is loaded by the other — so a field name, an
// amount rendering, or a sender identity that only agrees with itself shows up here
// as a failed swap rather than as a passing unit test.

// direction is one participant arrangement.
type direction struct {
	name   string
	goRole string
	pyRole string
}

// rig is the local two-chain regtest environment.
type rig struct {
	btcDatadir   string
	blakeDatadir string
	btcCLI       string
	blakeCLI     string
	repo         string
	python       string
	cli          string
}

// loadRig skips unless the rig, the Python venv and a built Go CLI are available.
func loadRig(t *testing.T) *rig {
	t.Helper()

	repo, err := filepath.Abs(filepath.Join("..", ".."))

	if err != nil {
		t.Fatal(err)
	}

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

	python := filepath.Join(repo, ".venv", "bin", "python")

	if _, err := os.Stat(python); err != nil {
		t.Skip("the Python virtual environment is not present")
	}

	cli := filepath.Join(t.TempDir(), "blake-swap-cli")

	build := exec.Command("go", "build", "-o", cli, "./cmd/blake-swap-cli")
	build.Dir = repo

	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the Go CLI failed: %v\n%s", err, output)
	}

	return &rig{
		btcDatadir:   env.BTCDatadir,
		blakeDatadir: env.BlakeDatadir,
		btcCLI:       env.BTCCLI,
		blakeCLI:     env.BlakeCLI,
		repo:         repo,
		python:       python,
		cli:          cli,
	}
}

// side is one participant's configuration.
type side struct {
	name    string
	role    string
	state   string
	keyFile string
	public  string
}

// newSide writes a participant key and returns its configuration.
func newSide(t *testing.T, name, role string) *side {
	t.Helper()

	dir := t.TempDir()
	keyFile := filepath.Join(dir, role+".key")

	secret := make([]byte, 32)

	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(keyFile, []byte(hex.EncodeToString(secret)), 0o600); err != nil {
		t.Fatal(err)
	}

	public, err := publicKeyOf(secret)

	if err != nil {
		t.Fatal(err)
	}

	return &side{
		name: name, role: role, state: dir, keyFile: keyFile, public: public,
	}
}

// env is the environment one participant's engine reads.
func (r *rig) env(s *side, messages string) []string {
	env := append([]string{}, os.Environ()...)

	env = append(env,
		"BLAKE_SWAP_STATE_DIR="+s.state,
		"BLAKE_SWAP_MESSAGE_DIR="+messages,
		"BLAKE_SWAP_ROLE="+s.role,
		"BLAKE_SWAP_TRANSPORT=file",
		"BLAKE_SWAP_PARTICIPANT_KEY_FILE="+s.keyFile,
		"BLAKE_SWAP_BTC_DATADIR="+r.btcDatadir,
		"BLAKE_SWAP_BLAKE_DATADIR="+r.blakeDatadir,
		"BLAKE_SWAP_BTC_CLI="+r.btcCLI,
		"BLAKE_SWAP_BLAKE_CLI="+r.blakeCLI,
		"BLAKE_SWAP_BTC_WALLET="+s.role,
		"BLAKE_SWAP_BLAKE_WALLET="+s.role,
		"BLAKE_SWAP_AUTO_MINE=1",
	)

	return env
}

// runGo runs the Go CLI as this participant.
func (r *rig) runGo(t *testing.T, s *side, messages string, args ...string) string {
	t.Helper()

	return r.run(t, r.cli, args, r.env(s, messages))
}

// runPy runs the Python engine as this participant.
func (r *rig) runPy(t *testing.T, s *side, messages string, args ...string) string {
	t.Helper()

	command := append([]string{filepath.Join(r.repo, "src", "swap_v2.py")}, args...)

	env := r.env(s, messages)

	return r.run(t, r.python, command, env)
}

// run executes one engine invocation and returns its output.
func (r *rig) run(t *testing.T, binary string, args, env []string) string {
	t.Helper()

	command := exec.Command(binary, args...)
	command.Dir = r.repo
	command.Env = env

	output, err := command.CombinedOutput()

	if err != nil {
		t.Fatalf("%s %s failed: %v\n%s", filepath.Base(binary), strings.Join(args, " "), err, output)
	}

	return string(output)
}

// publicKeyOf derives the x-only public key of a secret.
func publicKeyOf(secret []byte) (string, error) {
	key, err := keys.FromSecret(secret)

	if err != nil {
		return "", err
	}

	return key.PublicHex(), nil
}

// swapIDPattern matches the identifier each CLI prints.
var swapIDPattern = regexp.MustCompile(`swap id:\s+([0-9a-f]{12})`)

// swapID extracts the swap identifier from a command's output.
func swapID(t *testing.T, output string) string {
	t.Helper()

	match := swapIDPattern.FindStringSubmatch(output)

	if len(match) != 2 {
		t.Fatalf("no swap id in output:\n%s", output)
	}

	return match[1]
}

// TestCrossImplHandshakeAndSettlement runs both directions of the protocol with one
// implementation on each side.
func TestCrossImplHandshakeAndSettlement(t *testing.T) {
	r := loadRig(t)

	t.Run("go is alice", func(t *testing.T) {
		r.runDirection(t, direction{name: "go-alice", goRole: "alice", pyRole: "bob"})
	})

	t.Run("go is bob", func(t *testing.T) {
		r.runDirection(t, direction{name: "go-bob", goRole: "bob", pyRole: "alice"})
	})
}

// runDirection runs one full swap with the roles arranged as requested.
func (r *rig) runDirection(t *testing.T, dir direction) {
	t.Helper()

	messages := t.TempDir()

	goSide := newSide(t, "go", dir.goRole)
	pySide := newSide(t, "python", dir.pyRole)

	alice := goSide
	bob := pySide

	if dir.goRole == "bob" {
		alice = pySide
		bob = goSide
	}

	// Alice creates the swap.
	var createOutput string

	if alice == goSide {
		createOutput = r.runGo(t, alice, messages, "new", "--blake", "0.001", "--sats", "25000")
	} else {
		createOutput = r.runPy(t, alice, messages, "new", "--blake", "0.001", "--sats", "25000")
	}

	id := swapID(t, createOutput)

	// Alice publishes the offer.
	if alice == goSide {
		r.runGo(t, alice, messages, "publish-offer", id)
	} else {
		r.runPy(t, alice, messages, "publish-offer", id)
	}

	// Bob accepts it, pinned to Alice's identity.
	if bob == goSide {
		r.runGo(t, bob, messages, "accept-offer", id, "--expected-alice-pub", alice.public)
	} else {
		r.runPy(t, bob, messages, "accept-offer", id, "--expected-alice-pub", alice.public)
	}

	// Alice receives Bob's acceptance.
	if alice == goSide {
		r.runGo(t, alice, messages, "receive-accept", id)
	} else {
		r.runPy(t, alice, messages, "receive-accept", id)
	}

	// Alice funds BLAKE and announces it.
	if alice == goSide {
		r.runGo(t, alice, messages, "fund-blake", id)
		r.runGo(t, alice, messages, "publish-blake-funded", id)
	} else {
		r.runPy(t, alice, messages, "fund-blake", id)
		r.runPy(t, alice, messages, "publish-blake-funded", id)
	}

	// Bob verifies and records it.
	if bob == goSide {
		r.runGo(t, bob, messages, "receive-blake-funded", id)
	} else {
		r.runPy(t, bob, messages, "receive-blake-funded", id)
	}

	// Bob funds Bitcoin and announces it.
	if bob == goSide {
		r.runGo(t, bob, messages, "fund-btc", id)
		r.runGo(t, bob, messages, "publish-btc-funded", id)
	} else {
		r.runPy(t, bob, messages, "fund-btc", id)
		r.runPy(t, bob, messages, "publish-btc-funded", id)
	}

	// Alice verifies and records it.
	if alice == goSide {
		r.runGo(t, alice, messages, "receive-btc-funded", id)
	} else {
		r.runPy(t, alice, messages, "receive-btc-funded", id)
	}

	// Alice claims Bitcoin, which reveals the preimage on-chain.
	if alice == goSide {
		r.runGo(t, alice, messages, "claim-btc", id)
		r.runGo(t, alice, messages, "publish-btc-claimed", id)
	} else {
		r.runPy(t, alice, messages, "claim-btc", id)
		r.runPy(t, alice, messages, "publish-btc-claimed", id)
	}

	// Bob records the claim, extracts the secret from the chain, and claims BLAKE.
	if bob == goSide {
		r.runGo(t, bob, messages, "receive-btc-claimed", id)
		r.runGo(t, bob, messages, "extract-secret", id)
		r.runGo(t, bob, messages, "claim-blake", id)
		r.runGo(t, bob, messages, "publish-blake-claimed", id)
	} else {
		r.runPy(t, bob, messages, "receive-btc-claimed", id)
		r.runPy(t, bob, messages, "extract-secret", id)
		r.runPy(t, bob, messages, "claim-blake", id)
		r.runPy(t, bob, messages, "publish-blake-claimed", id)
	}

	// Alice records Bob's claim and completes the swap.
	if alice == goSide {
		r.runGo(t, alice, messages, "receive-blake-claimed", id)
		r.runGo(t, alice, messages, "complete", id)
	} else {
		r.runPy(t, alice, messages, "receive-blake-claimed", id)
		r.runPy(t, alice, messages, "complete", "--role", alice.role, id)
	}

	// Both records must agree about the outcome, whichever engine wrote them.
	assertState(t, r, alice, messages, id, "COMPLETE")
	assertState(t, r, bob, messages, id, "BLAKE_CLAIMED")
}

// assertState checks one participant's record state.
func assertState(t *testing.T, r *rig, s *side, messages, id, want string) {
	t.Helper()

	var output string

	if s.name == "go" {
		output = r.runGo(t, s, messages, "show", id)
	} else {
		output = r.runPy(t, s, messages, "show", id)
	}

	if !strings.Contains(output, "state:") || !strings.Contains(output, want) {
		t.Fatalf("%s record does not say %s:\n%s", s.name, want, output)
	}
}

// TestCrossImplRecordIsReadableByPython is the cheapest interop check, and the one
// that fails first when a field name drifts: a record written by the Go engine must
// load in the Python engine.
func TestCrossImplRecordIsReadableByPython(t *testing.T) {
	r := loadRig(t)

	messages := t.TempDir()
	goSide := newSide(t, "go", "alice")

	output := r.runGo(t, goSide, messages, "new", "--blake", "0.002", "--sats", "30000")
	id := swapID(t, output)

	// The Python engine holds no record for this swap, so it re-reads the file the
	// Go engine wrote under the same state directory.
	env := r.env(goSide, messages)
	env = append(env, "BLAKE_SWAP_ROLE=alice")

	script := fmt.Sprintf(`
import json, pathlib, sys
sys.path.insert(0, %q)
import swap_v2
record = swap_v2.load_swap(%q, role="alice")
print("python loaded:", record.swap_id)
print("state:", record.state)
print("blake base units:", record.blake_amount_base_units)
`, filepath.Join(r.repo, "src"), id)

	command := exec.Command(r.python, "-c", script)
	command.Dir = r.repo
	command.Env = env

	loaded, err := command.CombinedOutput()

	if err != nil {
		t.Fatalf("the Python engine could not read a Go record: %v\n%s", err, loaded)
	}

	if !strings.Contains(string(loaded), "python loaded: "+id) {
		t.Fatalf("Python did not load the record:\n%s", loaded)
	}
}
