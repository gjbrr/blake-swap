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
	"strconv"
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

	r := &rig{
		btcDatadir:   env.BTCDatadir,
		blakeDatadir: env.BlakeDatadir,
		btcCLI:       env.BTCCLI,
		blakeCLI:     env.BlakeCLI,
		repo:         repo,
		python:       python,
		cli:          cli,
	}

	ensureWallets(t, r)
	ensureCapital(t, r)

	return r
}

// ensureCapital gives both participants enough on both chains to swap.
//
// A fresh rig mines its coinbase into one wallet and has nothing else. The Python
// suites bootstrap capital in their own setup; an interop test has to do the same,
// and doing it here also leaves the rig in the state the suites expect.
func ensureCapital(t *testing.T, r *rig) {
	t.Helper()

	// Bitcoin can always be minted.
	for _, wallet := range []string{"alice", "bob"} {
		if balanceOf(t, r.btcCLI, r.btcDatadir, wallet) >= 10 {
			continue
		}

		address := strings.TrimSpace(cliOutput(t, r.btcCLI, r.btcDatadir,
			"-rpcwallet="+wallet, "getnewaddress"))

		cliOutput(t, r.btcCLI, r.btcDatadir, "-rpcwallet="+wallet,
			"generatetoaddress", "101", address)
	}

	// BLAKE issuance is finite: the rig's mining wallet is the only source on a new
	// chain, so it is swept once into Alice and recycled from there.
	if balanceOf(t, r.blakeCLI, r.blakeDatadir, "alice") >= 0.05 {
		return
	}

	aliceAddress := strings.TrimSpace(cliOutput(t, r.blakeCLI, r.blakeDatadir,
		"-rpcwallet=alice", "getnewaddress"))

	wallets := cliOutput(t, r.blakeCLI, r.blakeDatadir, "listwallets")

	if !strings.Contains(wallets, `"rig"`) {
		return
	}

	if balanceOf(t, r.blakeCLI, r.blakeDatadir, "rig") <= 0 {
		return
	}

	cliOutput(t, r.blakeCLI, r.blakeDatadir, "-rpcwallet=rig", "sendall", aliceAddress)
	cliOutput(t, r.blakeCLI, r.blakeDatadir, "generatetoaddress", "1", aliceAddress)
}

// balanceOf reads one wallet's trusted balance.
func balanceOf(t *testing.T, cli, datadir, wallet string) float64 {
	t.Helper()

	output := strings.TrimSpace(cliOutput(t, cli, datadir,
		"-rpcwallet="+wallet, "getbalance", "*", "1"))

	value, err := strconv.ParseFloat(output, 64)

	if err != nil {
		return 0
	}

	return value
}

// ensureWallets makes sure both participant wallets exist on both chains.
//
// A fresh rig has only its mining wallet. The Python suites create these wallets
// themselves; an interop test has to do the same, or it fails on a runner before it
// has tested anything.
func ensureWallets(t *testing.T, r *rig) {
	t.Helper()

	for _, chain := range []struct {
		cli     string
		datadir string
	}{
		{r.btcCLI, r.btcDatadir},
		{r.blakeCLI, r.blakeDatadir},
	} {
		loaded := cliOutput(t, chain.cli, chain.datadir, "listwallets")

		for _, wallet := range []string{"alice", "bob"} {
			if strings.Contains(loaded, `"`+wallet+`"`) {
				continue
			}

			if _, err := exec.Command(chain.cli, "-datadir="+chain.datadir,
				"loadwallet", wallet).CombinedOutput(); err == nil {
				continue
			}

			// A descriptor wallet, because the HTLC is a descriptor and a legacy
			// wallet cannot import one.
			cliOutput(t, chain.cli, chain.datadir, "createwallet",
				wallet, "false", "false", "", "false", "true")
		}
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

	for _, dir := range directions() {
		t.Run(dir.name, func(t *testing.T) {
			alice, bob, messages, id := setupSwap(t, r, dir)

			fundBothLegs(t, r, alice, bob, messages, id)

			assertReconcileAgrees(t, r, alice, bob, messages, id)

			run(t, r, alice, messages, "claim-btc", id)
			run(t, r, alice, messages, "publish-btc-claimed", id)

			run(t, r, bob, messages, "receive-btc-claimed", id)
			run(t, r, bob, messages, "extract-secret", id)
			run(t, r, bob, messages, "claim-blake", id)
			run(t, r, bob, messages, "publish-blake-claimed", id)

			run(t, r, alice, messages, "receive-blake-claimed", id)
			run(t, r, alice, messages, "complete", id)

			assertState(t, r, alice, messages, id, "COMPLETE")
			assertState(t, r, bob, messages, id, "BLAKE_CLAIMED")
		})
	}
}

// TestCrossImplRefundBothLegs runs the abortive path in both directions: Bob
// refunds Bitcoin once its timelock is reached, and Alice refunds BLAKE once hers
// is. This is the path that matters when the secret never moves, so it is the one
// where a message-shape disagreement would strand real money.
func TestCrossImplRefundBothLegs(t *testing.T) {
	r := loadRig(t)

	for _, dir := range directions() {
		t.Run(dir.name, func(t *testing.T) {
			alice, bob, messages, id := setupSwap(t, r, dir)

			fundBothLegs(t, r, alice, bob, messages, id)

			// Bob can only refund once Bitcoin's CLTV is reached, and the node
			// refuses a non-final transaction rather than reporting a protocol
			// error — so the test mines to the deadline the way an operator would.
			btcTimeout := fieldInt(t, r, alice, messages, id, "btc_timeout")
			mineTo(t, r, "btc", bob.role, btcTimeout)

			run(t, r, bob, messages, "refund-btc", id)
			run(t, r, bob, messages, "publish-btc-refunded", id)
			run(t, r, alice, messages, "receive-btc-refunded", id)

			blakeTimeout := fieldInt(t, r, alice, messages, id, "blake_timeout")
			mineTo(t, r, "blake", alice.role, blakeTimeout)

			run(t, r, alice, messages, "refund-blake", id)

			assertState(t, r, alice, messages, id, "BLAKE_REFUNDED")
			assertState(t, r, bob, messages, id, "BTC_REFUNDED")
		})
	}
}

// assertReconcileAgrees compares the two implementations' protocol reports.
//
// Each side reports on its own record, but the *facts* about the chains and the
// checkpoint must agree — that is the whole point of a shared protocol. The
// recommended action is role-specific, so it is checked against what the role can
// legitimately do rather than against the other side.
func assertReconcileAgrees(t *testing.T, r *rig, alice, bob *side, messages, id string) {
	t.Helper()

	aliceReport := reconcileJSON(t, r, alice, messages, id)
	bobReport := reconcileJSON(t, r, bob, messages, id)

	// The checkpoint and the chains are role-independent facts, so the two
	// implementations must agree about them.
	if aliceReport["checkpoint_state"] != bobReport["checkpoint_state"] {
		t.Fatalf(
			"the two implementations disagree about the checkpoint: go=%v python=%v",
			aliceReport["checkpoint_state"], bobReport["checkpoint_state"],
		)
	}

	if aliceReport["checkpoint_state"] != "BTC_FUNDED" {
		t.Fatalf("the checkpoint is %v, want BTC_FUNDED", aliceReport["checkpoint_state"])
	}

	for _, leg := range []string{"blake", "btc"} {
		aliceLeg, _ := aliceReport[leg].(map[string]any)
		bobLeg, _ := bobReport[leg].(map[string]any)

		if aliceLeg["status"] != bobLeg["status"] {
			t.Fatalf(
				"the two implementations disagree about %s: go=%v python=%v",
				leg, aliceLeg["status"], bobLeg["status"],
			)
		}

		if aliceLeg["status"] != "funded" {
			t.Fatalf("%s is %v, want funded at this point", leg, aliceLeg["status"])
		}
	}

	aliceHeights, _ := aliceReport["heights"].(map[string]any)
	bobHeights, _ := bobReport["heights"].(map[string]any)

	if aliceHeights["blake"] != bobHeights["blake"] || aliceHeights["btc"] != bobHeights["btc"] {
		t.Fatalf("heights disagree: go=%v python=%v", aliceHeights, bobHeights)
	}

	// The recommendation is role-specific, so it is checked against what each role
	// may legitimately do rather than against the other side: Alice claims Bitcoin,
	// and Bob waits for her to.
	aliceAction, _ := aliceReport["next_action"].(string)
	aliceClass, _ := aliceReport["decision_class"].(string)
	bobAction, _ := bobReport["next_action"].(string)
	bobClass, _ := bobReport["decision_class"].(string)

	if aliceAction != "claim-btc" || aliceClass != "actionable" {
		t.Fatalf("alice: action %q class %q, want claim-btc/actionable", aliceAction, aliceClass)
	}

	if bobAction != "wait" || bobClass != "waiting" {
		t.Fatalf("bob: action %q class %q, want wait/waiting", bobAction, bobClass)
	}
}

// reconcileJSON runs `reconcile --json` on whichever implementation this side uses.
func reconcileJSON(t *testing.T, r *rig, s *side, messages, id string) map[string]any {
	t.Helper()

	output := run(t, r, s, messages, "reconcile", id, "--json")

	start := strings.Index(output, "{")

	if start < 0 {
		t.Fatalf("no report in output:\n%s", output)
	}

	var report map[string]any

	if err := json.Unmarshal([]byte(output[start:]), &report); err != nil {
		t.Fatalf("report is not valid JSON: %v\n%s", err, output)
	}

	return report
}

// directions is the two participant arrangements.
func directions() []direction {
	return []direction{
		{name: "go is alice", goRole: "alice", pyRole: "bob"},
		{name: "go is bob", goRole: "bob", pyRole: "alice"},
	}
}

// sides maps a direction onto Alice's and Bob's configurations.
func sides(t *testing.T, dir direction) (*side, *side) {
	t.Helper()

	goSide := newSide(t, "go", dir.goRole)
	pySide := newSide(t, "python", dir.pyRole)

	if dir.goRole == "alice" {
		return goSide, pySide
	}

	return pySide, goSide
}

// setupSwap runs the handshake and returns both sides, the shared spool and the
// swap id.
func setupSwap(t *testing.T, r *rig, dir direction) (*side, *side, string, string) {
	t.Helper()

	messages := t.TempDir()

	alice, bob := sides(t, dir)

	output := run(t, r, alice, messages, "new", "--blake", "0.001", "--sats", "25000")
	id := swapID(t, output)

	run(t, r, alice, messages, "publish-offer", id)
	run(t, r, bob, messages, "accept-offer", id, "--expected-alice-pub", alice.public)
	run(t, r, alice, messages, "receive-accept", id)

	return alice, bob, messages, id
}

// fundBothLegs funds and announces both legs.
func fundBothLegs(t *testing.T, r *rig, alice, bob *side, messages, id string) {
	t.Helper()

	run(t, r, alice, messages, "fund-blake", id)
	run(t, r, alice, messages, "publish-blake-funded", id)
	run(t, r, bob, messages, "receive-blake-funded", id)

	run(t, r, bob, messages, "fund-btc", id)
	run(t, r, bob, messages, "publish-btc-funded", id)
	run(t, r, alice, messages, "receive-btc-funded", id)
}

// run invokes one command on whichever implementation this participant uses.
func run(t *testing.T, r *rig, s *side, messages string, args ...string) string {
	t.Helper()

	if s.name == "go" {
		return r.runGo(t, s, messages, args...)
	}

	// The reference CLI requires --role on the commands that mutate a record from
	// either side; the Go CLI derives it from the environment like the rest.
	switch args[0] {
	case "complete":
		return r.runPy(t, s, messages, append([]string{args[0], "--role", s.role}, args[1:]...)...)
	}

	return r.runPy(t, s, messages, args...)
}

// fieldInt reads an integer field from one participant's record.
func fieldInt(t *testing.T, r *rig, s *side, messages, id, field string) int64 {
	t.Helper()

	output := run(t, r, s, messages, "show", id)

	label := regexp.QuoteMeta(strings.ReplaceAll(field, "_", " "))
	pattern := regexp.MustCompile("(?i)" + label + `:\s+(\d+)`)
	match := pattern.FindStringSubmatch(output)

	if len(match) != 2 {
		t.Fatalf("no %s in record output:\n%s", field, output)
	}

	value, err := strconv.ParseInt(match[1], 10, 64)

	if err != nil {
		t.Fatal(err)
	}

	return value
}

// mineTo mines one chain up to a height, to the given wallet's address.
func mineTo(t *testing.T, r *rig, which, wallet string, height int64) {
	t.Helper()

	cli := r.btcCLI
	datadir := r.btcDatadir

	if which == "blake" {
		cli = r.blakeCLI
		datadir = r.blakeDatadir
	}

	tip := heightOf(t, cli, datadir)

	if tip >= height {
		return
	}

	address := cliOutput(t, cli, datadir, "-rpcwallet="+wallet, "getnewaddress")

	cliOutput(t, cli, datadir, "-rpcwallet="+wallet, "generatetoaddress",
		strconv.FormatInt(height-tip, 10), strings.TrimSpace(address))
}

// heightOf returns one chain's tip.
func heightOf(t *testing.T, cli, datadir string) int64 {
	t.Helper()

	output := cliOutput(t, cli, datadir, "getblockcount")

	value, err := strconv.ParseInt(strings.TrimSpace(output), 10, 64)

	if err != nil {
		t.Fatalf("getblockcount: %v", err)
	}

	return value
}

// cliOutput runs a node CLI and returns its output.
func cliOutput(t *testing.T, cli, datadir string, args ...string) string {
	t.Helper()

	command := exec.Command(cli, append([]string{"-datadir=" + datadir}, args...)...)

	output, err := command.CombinedOutput()

	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", filepath.Base(cli), args, err, output)
	}

	return string(output)
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
