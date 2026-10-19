//go:build !js

// Package rigtest is the shared harness for the Go integration suites.
//
// The reference suites are not one test file with one setup: they are sixty
// files that import a handful of shared helpers. The Go replacement has the same
// shape, so the helpers live here once rather than being copied into every
// package that wants a two-chain swap.
//
// What is shared is deliberately narrow. It is the regtest rig, the Python
// reference driven as a counterparty, and the process plumbing to run either
// implementation as one participant. It is *not* the assertions: a suite owns
// what it proves, and a helper that asserted anything about a swap would make
// every suite that used it prove the same thing.
package rigtest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gjbrr/blake-swap/pkg/keys"
)

// Rig is the local two-chain regtest environment.
type Rig struct {
	BTCDatadir   string
	BlakeDatadir string
	BTCCLI       string
	BlakeCLI     string
	// Repo is the checkout root. Every process this harness starts runs there,
	// because the reference engine is a script inside it.
	Repo string
	// Python is the virtual environment's interpreter, which is the only one
	// that has the reference engine's dependencies.
	Python string
	// CLI is the Go CLI built from this checkout.
	CLI string
}

// Direction is one participant arrangement for a cross-implementation run.
type Direction struct {
	Name   string
	GoRole string
	PyRole string
}

// Directions is the two participant arrangements.
func Directions() []Direction {
	return []Direction{
		{Name: "go is alice", GoRole: "alice", PyRole: "bob"},
		{Name: "go is bob", GoRole: "bob", PyRole: "alice"},
	}
}

// Side is one participant's configuration.
type Side struct {
	// Name selects the implementation: "go" or "python".
	Name string
	// Role is the protocol role the participant believes it has.
	Role string
	// State is the directory its records are written to.
	State string
	// KeyFile holds this participant's Nostr identity.
	KeyFile string
	// Public is its x-only public key.
	Public string
}

// The CLI is built once per test binary. Every suite used to rebuild it, which
// cost more than the test it was building for.
var (
	buildOnce sync.Once
	builtCLI  string
	buildErr  error
)

// Load skips unless the rig, the Python virtual environment and a built Go CLI
// are all available, and otherwise returns them ready to use.
//
// A rig is only usable when its nodes answer. A stale rig-env.json after a
// stopped or crashed node is common — `scripts/test-nodes start` rewrites it and
// nothing removes it — so presence is proved by reading a block height rather
// than by the file existing. Without that probe a stopped rig fails the suite
// instead of skipping it, which is the difference between "not run" and "broken".
func Load(t *testing.T) *Rig {
	t.Helper()

	repo := repoRoot(t)

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
		Running      bool   `json:"running"`
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

	r := &Rig{
		BTCDatadir:   env.BTCDatadir,
		BlakeDatadir: env.BlakeDatadir,
		BTCCLI:       env.BTCCLI,
		BlakeCLI:     env.BlakeCLI,
		Repo:         repo,
		Python:       python,
	}

	// Probe the nodes. A skip here names the reason, so a suite that did not run
	// cannot be misread as one that passed.
	for _, chain := range []struct {
		cli     string
		datadir string
	}{{r.BTCCLI, r.BTCDatadir}, {r.BlakeCLI, r.BlakeDatadir}} {
		if _, err := r.node(chain.cli, chain.datadir, "getblockcount"); err != nil {
			t.Skipf("the test rig at %s is not running: %v", root, err)
		}
	}

	r.CLI = buildCLI(t, repo)

	EnsureWallets(t, r)
	EnsureCapital(t, r)

	return r
}

// repoRoot walks up from the working directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)

		if parent == dir {
			t.Fatal("no go.mod above the working directory")
		}

		dir = parent
	}
}

// buildCLI builds the Go CLI once and returns its path.
//
// BLAKE_SWAP_TEST_CLI lets a caller reuse a binary it already built, which is
// what the browser check and CI do.
func buildCLI(t *testing.T, repo string) string {
	t.Helper()

	if override := os.Getenv("BLAKE_SWAP_TEST_CLI"); override != "" {
		if _, err := os.Stat(override); err != nil {
			t.Fatalf("BLAKE_SWAP_TEST_CLI does not exist: %v", err)
		}

		return override
	}

	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "blake-swap-cli-")
		if err != nil {
			buildErr = err

			return
		}

		builtCLI = filepath.Join(dir, "blake-swap-cli")

		build := exec.Command("go", "build", "-o", builtCLI, "./cmd/blake-swap-cli")
		build.Dir = repo

		if output, err := build.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("%w\n%s", err, output)
		}
	})

	if buildErr != nil {
		t.Fatalf("building the Go CLI failed: %v", buildErr)
	}

	return builtCLI
}

// EnsureWallets makes sure every participant wallet exists and is loaded on both
// chains, including the watch-only wallet each settlement observation reads.
//
// A fresh rig has only its mining wallet; a rig that has been restarted has
// whatever was loaded when it started, which is not the same set. Both are
// bootstrapped here rather than in each suite, because a suite that skipped this
// would fail on the runner before it had tested anything — and the failure would
// look like an engine bug.
func EnsureWallets(t *testing.T, r *Rig) {
	t.Helper()

	for _, chain := range []struct {
		cli     string
		datadir string
	}{
		{r.BTCCLI, r.BTCDatadir},
		{r.BlakeCLI, r.BlakeDatadir},
	} {
		loaded, err := r.node(chain.cli, chain.datadir, "listwallets")
		if err != nil {
			t.Fatalf("listwallets: %v", err)
		}

		for _, wallet := range []string{"alice", "bob", "alice-watch", "bob-watch"} {
			if strings.Contains(loaded, `"`+wallet+`"`) {
				continue
			}

			if _, err := r.node(chain.cli, chain.datadir, "loadwallet", wallet); err == nil {
				continue
			}

			// A descriptor wallet, because the HTLC is a descriptor and a legacy
			// wallet cannot import one.
			if _, err := r.node(chain.cli, chain.datadir, "createwallet",
				wallet, "false", "false", "", "false", "true"); err != nil {
				t.Fatalf("creating wallet %s: %v", wallet, err)
			}
		}
	}
}

// EnsureCapital gives both participants enough on both chains to swap.
//
// A fresh rig mines its coinbase into one wallet and has nothing else. The Python
// suites bootstrap capital in their own setup; an integration suite has to do the
// same, and doing it here also leaves the rig in the state the suites expect.
func EnsureCapital(t *testing.T, r *Rig) {
	t.Helper()

	// Bitcoin can always be minted.
	for _, wallet := range []string{"alice", "bob"} {
		if balanceOf(t, r, "btc", wallet) >= 10 {
			continue
		}

		address, err := r.node(r.BTCCLI, r.BTCDatadir, "-rpcwallet="+wallet, "getnewaddress")
		if err != nil {
			t.Fatalf("getnewaddress: %v", err)
		}

		if _, err := r.node(r.BTCCLI, r.BTCDatadir, "-rpcwallet="+wallet,
			"generatetoaddress", "101", strings.TrimSpace(address)); err != nil {
			t.Fatalf("generatetoaddress: %v", err)
		}
	}

	// BLAKE issuance is finite: the rig's mining wallet is the only source on a
	// new chain, so it is swept once into Alice and recycled from there.
	if balanceOf(t, r, "blake", "alice") >= 0.05 {
		return
	}

	aliceAddress, err := r.node(r.BlakeCLI, r.BlakeDatadir, "-rpcwallet=alice", "getnewaddress")
	if err != nil {
		t.Fatalf("getnewaddress: %v", err)
	}

	aliceAddress = strings.TrimSpace(aliceAddress)

	wallets, err := r.node(r.BlakeCLI, r.BlakeDatadir, "listwallets")
	if err != nil {
		t.Fatalf("listwallets: %v", err)
	}

	if !strings.Contains(wallets, `"rig"`) {
		return
	}

	if balanceOf(t, r, "blake", "rig") <= 0 {
		return
	}

	// sendall takes a JSON recipient list, the same shape the Python suites pass.
	recipients := `["` + aliceAddress + `"]`

	if _, err := r.node(r.BlakeCLI, r.BlakeDatadir, "-rpcwallet=rig", "sendall", recipients); err != nil {
		t.Fatalf("sendall: %v", err)
	}

	if _, err := r.node(r.BlakeCLI, r.BlakeDatadir, "generatetoaddress", "1", aliceAddress); err != nil {
		t.Fatalf("generatetoaddress: %v", err)
	}
}

// balanceOf reads one wallet's trusted balance.
func balanceOf(t *testing.T, r *Rig, chain, wallet string) float64 {
	t.Helper()

	cli, datadir := r.chain(chain)

	output, err := r.node(cli, datadir, "-rpcwallet="+wallet, "getbalance", "*", "1")
	if err != nil {
		return 0
	}

	value, err := strconv.ParseFloat(strings.TrimSpace(output), 64)
	if err != nil {
		return 0
	}

	return value
}

// chain returns one chain's CLI and datadir.
func (r *Rig) chain(name string) (string, string) {
	if name == "blake" {
		return r.BlakeCLI, r.BlakeDatadir
	}

	return r.BTCCLI, r.BTCDatadir
}

// node runs one node CLI command.
func (r *Rig) node(cli, datadir string, args ...string) (string, error) {
	command := exec.Command(cli, append([]string{"-datadir=" + datadir}, args...)...)

	output, err := command.CombinedOutput()

	if err != nil {
		return string(output), fmt.Errorf("%s %v: %w\n%s", filepath.Base(cli), args, err, output)
	}

	return string(output), nil
}

// Node runs one node CLI command and fails the test if it does not succeed.
func (r *Rig) Node(t *testing.T, chain string, args ...string) string {
	t.Helper()

	cli, datadir := r.chain(chain)

	output, err := r.node(cli, datadir, args...)
	if err != nil {
		t.Fatalf("%v", err)
	}

	return output
}

// NodeTry runs one node CLI command and returns its error rather than failing.
//
// Suites that assert a node *refuses* something need this: a refusal is the
// observation, not a test failure.
func (r *Rig) NodeTry(t *testing.T, chain string, args ...string) (string, error) {
	t.Helper()

	cli, datadir := r.chain(chain)

	return r.node(cli, datadir, args...)
}

// NewSide writes a participant key and returns its configuration.
func NewSide(t *testing.T, name, role string) *Side {
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

	key, err := keys.FromSecret(secret)
	if err != nil {
		t.Fatal(err)
	}

	return &Side{
		Name: name, Role: role, State: dir, KeyFile: keyFile, Public: key.PublicHex(),
	}
}

// Sides maps a direction onto Alice's and Bob's configurations.
func Sides(t *testing.T, dir Direction) (alice, bob *Side) {
	t.Helper()

	goSide := NewSide(t, "go", dir.GoRole)
	pySide := NewSide(t, "python", dir.PyRole)

	if dir.GoRole == "alice" {
		return goSide, pySide
	}

	return pySide, goSide
}

// Env is the environment one participant's engine reads.
//
// TEST_MODE is on because the reference's own harness turns it on for every
// suite it runs (tests/swap_protocol_tests.py sets it on the shared command
// environment). It gates `advance` and the crash points, so the port would be
// testing a strictly smaller surface without it.
func (r *Rig) Env(s *Side, messages string) []string {
	env := append([]string{}, os.Environ()...)

	return append(env,
		"BLAKE_SWAP_STATE_DIR="+s.State,
		"BLAKE_SWAP_MESSAGE_DIR="+messages,
		"BLAKE_SWAP_ROLE="+s.Role,
		"BLAKE_SWAP_TRANSPORT=file",
		"BLAKE_SWAP_PARTICIPANT_KEY_FILE="+s.KeyFile,
		"BLAKE_SWAP_BTC_DATADIR="+r.BTCDatadir,
		"BLAKE_SWAP_BLAKE_DATADIR="+r.BlakeDatadir,
		"BLAKE_SWAP_BTC_CLI="+r.BTCCLI,
		"BLAKE_SWAP_BLAKE_CLI="+r.BlakeCLI,
		"BLAKE_SWAP_BTC_WALLET="+s.Role,
		"BLAKE_SWAP_BLAKE_WALLET="+s.Role,
		"BLAKE_SWAP_AUTO_MINE=1",
		"BLAKE_SWAP_TEST_MODE=1",
	)
}

// RunGo runs the Go CLI as this participant.
func (r *Rig) RunGo(t *testing.T, s *Side, messages string, args ...string) string {
	t.Helper()

	output, _, err := r.Exec(t, s, messages, nil, args...)
	if err != nil {
		t.Fatalf("go %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}

	return output
}

// RunPy runs the Python engine as this participant.
func (r *Rig) RunPy(t *testing.T, s *Side, messages string, args ...string) string {
	t.Helper()

	output, _, err := r.Exec(t, s, messages, nil, args...)
	if err != nil {
		t.Fatalf("python %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}

	return output
}

// Run invokes one command on whichever implementation this participant uses.
func (r *Rig) Run(t *testing.T, s *Side, messages string, args ...string) string {
	t.Helper()

	output, _, err := r.Exec(t, s, messages, nil, args...)
	if err != nil {
		t.Fatalf("%s %s failed: %v\n%s", s.Name, strings.Join(args, " "), err, output)
	}

	return output
}

// Exec runs one engine invocation as this participant and reports what happened
// without deciding that a non-zero exit is a test failure.
//
// A settlement suite asserts *how* a process died — exit 97 at a failpoint, exit
// 86 at a crash point — so the exit must be observable rather than fatal. It also
// carries extra environment, which is how those points are armed.
func (r *Rig) Exec(
	t *testing.T, s *Side, messages string, extraEnv []string, args ...string,
) (string, int, error) {
	t.Helper()

	binary := r.CLI
	command := args

	if s.Name == "python" {
		binary = r.Python
		command = append([]string{filepath.Join(r.Repo, "src", "swap_v2.py")}, command...)

		// The reference CLI requires --role on the commands that mutate a record
		// from either side; the Go CLI derives it from the environment like the
		// rest.
		if len(command) > 1 && command[1] == "complete" {
			command = append([]string{command[0], "complete", "--role", s.Role}, command[2:]...)
		}
	}

	cmd := exec.Command(binary, command...)
	cmd.Dir = r.Repo
	cmd.Env = append(r.Env(s, messages), extraEnv...)

	output, err := cmd.CombinedOutput()

	exit := 0

	if err != nil {
		var exitErr *exec.ExitError

		if errors.As(err, &exitErr) {
			exit = exitErr.ExitCode()
		} else {
			return string(output), -1, err
		}
	}

	return string(output), exit, nil
}

// RunPythonScript runs a Python snippet in this participant's environment.
func (r *Rig) RunPythonScript(t *testing.T, s *Side, messages, script string, extraEnv ...string) string {
	t.Helper()

	cmd := exec.Command(r.Python, "-c", script)
	cmd.Dir = r.Repo
	cmd.Env = append(r.Env(s, messages), extraEnv...)

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("python snippet failed: %v\n%s", err, output)
	}

	return string(output)
}

// swapIDPattern matches the identifier each CLI prints.
var swapIDPattern = regexp.MustCompile(`swap id:\s+([0-9a-f]{12})`)

// SwapID extracts the swap identifier from a command's output.
func SwapID(t *testing.T, output string) string {
	t.Helper()

	match := swapIDPattern.FindStringSubmatch(output)
	if len(match) != 2 {
		t.Fatalf("no swap id in output:\n%s", output)
	}

	return match[1]
}

// SetupSwap runs the handshake and returns both sides, the shared spool and the
// swap id.
func (r *Rig) SetupSwap(t *testing.T, dir Direction) (alice, bob *Side, messages, id string) {
	t.Helper()

	messages = t.TempDir()

	alice, bob = Sides(t, dir)

	output := r.Run(t, alice, messages, "new", "--blake", "0.001", "--sats", "25000")
	id = SwapID(t, output)

	r.Run(t, alice, messages, "publish-offer", id)
	r.Run(t, bob, messages, "accept-offer", id, "--expected-alice-pub", alice.Public)
	r.Run(t, alice, messages, "receive-accept", id)

	return alice, bob, messages, id
}

// SetupGoSwap runs the handshake between two participants that both use this
// implementation.
//
// The reference's own suites are single-implementation: the counterparty walk is a
// separate file. A ported suite that is about this engine's behaviour — a crash
// boundary, a reorg, an idle poll — therefore runs both sides here, and only the
// cross-implementation walks put the reference on one side.
func (r *Rig) SetupGoSwap(t *testing.T) (alice, bob *Side, messages, id string) {
	t.Helper()

	return r.SetupGoSwapIn(t, t.TempDir())
}

// SetupGoSwapIn runs the handshake on an existing spool, so two swaps can share
// one.
//
// Sharing matters for the replay suites: a message that names a different swap is
// only a replay if both swaps are reachable through the same transport, which is
// exactly the situation a participant with more than one swap is in.
func (r *Rig) SetupGoSwapIn(t *testing.T, messages string) (alice, bob *Side, _, id string) {
	t.Helper()

	alice = NewSide(t, "go", "alice")
	bob = NewSide(t, "go", "bob")

	return alice, bob, messages, r.Handshake(t, alice, bob, messages)
}

// Handshake runs the offer/accept exchange for a new swap between two existing
// participants, so one participant can hold several swaps at once.
func (r *Rig) Handshake(t *testing.T, alice, bob *Side, messages string) (id string) {
	t.Helper()

	output := r.Run(t, alice, messages, "new", "--blake", "0.001", "--sats", "25000")
	id = SwapID(t, output)

	r.Run(t, alice, messages, "publish-offer", id)
	r.Run(t, bob, messages, "accept-offer", id, "--expected-alice-pub", alice.Public)
	r.Run(t, alice, messages, "receive-accept", id)

	return id
}

// FundBothLegs funds and announces both legs.
func (r *Rig) FundBothLegs(t *testing.T, alice, bob *Side, messages, id string) {
	t.Helper()

	r.Run(t, alice, messages, "fund-blake", id)
	r.Run(t, alice, messages, "publish-blake-funded", id)
	r.Run(t, bob, messages, "receive-blake-funded", id)

	r.Run(t, bob, messages, "fund-btc", id)
	r.Run(t, bob, messages, "publish-btc-funded", id)
	r.Run(t, alice, messages, "receive-btc-funded", id)
}

// MineTo mines one chain up to a height, to the given wallet's address.
func (r *Rig) MineTo(t *testing.T, chain, wallet string, height int64) {
	t.Helper()

	tip := r.HeightOf(t, chain)

	if tip >= height {
		return
	}

	address := r.Node(t, chain, "-rpcwallet="+wallet, "getnewaddress")

	r.Node(t, chain, "-rpcwallet="+wallet, "generatetoaddress",
		strconv.FormatInt(height-tip, 10), strings.TrimSpace(address))
}

// Mine mines a number of blocks on one chain to one wallet's address.
func (r *Rig) Mine(t *testing.T, chain, wallet string, blocks int64) {
	t.Helper()

	address := r.Node(t, chain, "-rpcwallet="+wallet, "getnewaddress")

	r.Node(t, chain, "-rpcwallet="+wallet, "generatetoaddress",
		strconv.FormatInt(blocks, 10), strings.TrimSpace(address))
}

// HeightOf returns one chain's tip.
func (r *Rig) HeightOf(t *testing.T, chain string) int64 {
	t.Helper()

	output := strings.TrimSpace(r.Node(t, chain, "getblockcount"))

	value, err := strconv.ParseInt(output, 10, 64)
	if err != nil {
		t.Fatalf("getblockcount: %v", err)
	}

	return value
}

// FieldInt reads an integer field from one participant's rendered record.
func (r *Rig) FieldInt(t *testing.T, s *Side, messages, id, field string) int64 {
	t.Helper()

	output := r.Run(t, s, messages, "show", id)

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

// AssertState checks one participant's record state.
func (r *Rig) AssertState(t *testing.T, s *Side, messages, id, want string) {
	t.Helper()

	output := r.Run(t, s, messages, "show", id)

	if !strings.Contains(output, "state:") || !strings.Contains(output, want) {
		t.Fatalf("%s record does not say %s:\n%s", s.Name, want, output)
	}
}

// ReconcileJSON runs `reconcile --json` on whichever implementation this side uses.
func (r *Rig) ReconcileJSON(t *testing.T, s *Side, messages, id string) map[string]any {
	t.Helper()

	output := r.Run(t, s, messages, "reconcile", id, "--json")

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

// AssertReconcileAgrees compares the two implementations' protocol reports.
//
// Each side reports on its own record, but the *facts* about the chains and the
// checkpoint must agree — that is the whole point of a shared protocol. The
// recommended action is role-specific, so it is checked against what the role can
// legitimately do rather than against the other side.
func (r *Rig) AssertReconcileAgrees(t *testing.T, alice, bob *Side, messages, id string) {
	t.Helper()

	aliceReport := r.ReconcileJSON(t, alice, messages, id)
	bobReport := r.ReconcileJSON(t, bob, messages, id)

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

// RecordField reads one field from a participant's stored record.
func RecordField(t *testing.T, s *Side, id, field string) (any, bool) {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(s.State, id+".json"))
	if err != nil {
		t.Fatalf("reading %s's record: %v", s.Name, err)
	}

	var record map[string]any

	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("decoding %s's record: %v", s.Name, err)
	}

	value, present := record[field]

	return value, present
}

// Record reads a participant's stored record.
func Record(t *testing.T, s *Side, id string) map[string]any {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(s.State, id+".json"))
	if err != nil {
		t.Fatalf("reading %s's record: %v", s.Name, err)
	}

	var record map[string]any

	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("decoding %s's record: %v", s.Name, err)
	}

	return record
}

// AssertFieldPresent requires a field to be stored with a non-empty value.
func AssertFieldPresent(t *testing.T, s *Side, id, field string) {
	t.Helper()

	value, present := RecordField(t, s, id, field)

	if !present || value == nil || fmt.Sprint(value) == "" {
		t.Fatalf("%s did not record %s", s.Name, field)
	}
}

// AssertFieldAbsent requires a field to be absent or null.
func AssertFieldAbsent(t *testing.T, s *Side, id, field string) {
	t.Helper()

	value, present := RecordField(t, s, id, field)

	if present && value != nil && fmt.Sprint(value) != "" {
		t.Fatalf("%s recorded %s, which it must not have", s.Name, field)
	}
}

// Expect runs one command as this participant and requires it to fail, with the
// given fragment in its output.
//
// A refusal is a protocol observation, not a test failure: the suites that assert
// a wrong-role action or a replayed message is *refused* need the output, the exit
// status and the record that did not change.
func (r *Rig) Expect(t *testing.T, s *Side, messages, fragment string, args ...string) string {
	t.Helper()

	output, exit, err := r.Exec(t, s, messages, nil, args...)

	if err != nil {
		t.Fatalf("running %v: %v", args, err)
	}

	if exit == 0 {
		t.Fatalf("%s %v succeeded; expected a refusal containing %q\n%s",
			s.Name, args, fragment, output)
	}

	if fragment != "" && !strings.Contains(output, fragment) {
		t.Fatalf("%s %v refused for the wrong reason; expected %q in:\n%s",
			s.Name, args, fragment, output)
	}

	return output
}

// Mempool is one chain's current mempool transaction ids, sorted.
func (r *Rig) Mempool(t *testing.T, which string) []string {
	t.Helper()

	var ids []string

	if err := json.Unmarshal([]byte(r.Node(t, which, "getrawmempool")), &ids); err != nil {
		t.Fatalf("getrawmempool: %v", err)
	}

	sort.Strings(ids)

	return ids
}

// AssertMempoolUnchanged requires one chain's mempool to be the same before and
// after, which is how a suite proves an action moved no money.
func (r *Rig) AssertMempoolUnchanged(t *testing.T, which string, before []string, what string) {
	t.Helper()

	after := r.Mempool(t, which)

	if strings.Join(before, ",") != strings.Join(after, ",") {
		t.Fatalf("%s changed the %s mempool:\n before: %v\n after:  %v",
			what, which, before, after)
	}
}

// Snapshot is one participant's stored record, as bytes.
//
// Comparing bytes is how a suite proves a refused action left the record exactly
// as it was, including fields the rendered output does not show.
func (r *Rig) Snapshot(t *testing.T, s *Side, id string) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(s.State, id+".json"))

	if err != nil {
		t.Fatalf("reading %s's record: %v", s.Name, err)
	}

	return string(raw)
}

// AssertSnapshotUnchanged requires a participant's record to be byte-identical.
func (r *Rig) AssertSnapshotUnchanged(t *testing.T, s *Side, id, before, what string) {
	t.Helper()

	if after := r.Snapshot(t, s, id); after != before {
		t.Fatalf("%s changed %s's record:\n before: %s\n after:  %s",
			what, s.Name, before, after)
	}
}

// MessageSpool lists the coordination messages currently in the shared spool.
func (r *Rig) MessageSpool(t *testing.T, messages string) []string {
	t.Helper()

	names := []string{}

	entries, err := os.ReadDir(messages)

	if err != nil {
		if os.IsNotExist(err) {
			return names
		}

		t.Fatalf("reading the message spool: %v", err)
	}

	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	sort.Strings(names)

	return names
}

// AssertNoNewMessages requires a command to have published nothing.
func (r *Rig) AssertNoNewMessages(t *testing.T, messages string, before []string, what string) {
	t.Helper()

	after := r.MessageSpool(t, messages)

	if strings.Join(before, ",") != strings.Join(after, ",") {
		t.Fatalf("%s published a coordination message:\n before: %v\n after:  %v",
			what, before, after)
	}
}

// Command builds one engine invocation that the caller starts itself.
//
// The settlement race suite needs two processes released at the same instant, so
// the harness cannot own the wait.
func (r *Rig) Command(
	t *testing.T, s *Side, messages string, extraEnv []string, args ...string,
) *exec.Cmd {
	t.Helper()

	binary := r.CLI

	if s.Name == "python" {
		binary = r.Python
		args = append([]string{filepath.Join(r.Repo, "src", "swap_v2.py")}, args...)
	}

	command := exec.Command(binary, args...)
	command.Dir = r.Repo
	command.Env = append(r.Env(s, messages), extraEnv...)

	return command
}

// RestartNode stops one chain's node and starts it again with extra arguments.
//
// The waits are bounded readiness checks, not sleeps that paper over a race: a
// node that is stopped is not ready, and one that has not finished loading its
// chain cannot answer. Each loop has a hard deadline and reports what it was
// waiting for.
//
// A restart also unloads wallets that were loaded after the node started, so the
// harness re-ensures them: a test that restarted a node should not fail because of
// which wallets the node happened to have at boot.
func (r *Rig) RestartNode(t *testing.T, which string, extraArgs ...string) {
	t.Helper()

	r.restart(t, which, extraArgs...)
}

// restart stops and starts one node.
func (r *Rig) restart(t *testing.T, which string, extraArgs ...string) {
	t.Helper()

	cli, datadir := r.chain(which)
	daemon := filepath.Join(filepath.Dir(cli), "bitcoind")

	if _, err := os.Stat(daemon); err != nil {
		t.Fatalf("the %s node binary is not beside its CLI: %v", which, err)
	}

	if _, err := r.node(cli, datadir, "stop"); err != nil {
		t.Fatalf("stopping the %s node: %v", which, err)
	}

	// RPC stops answering well before the node has finished shutting down, and the
	// mempool dump is written late in the shutdown. Waiting for the data directory's
	// lock is waiting for the actual end of the process, which is what makes
	// deleting the dump afterwards deterministic.
	waitForDatadirRelease(t, which, datadir)

	// RPC stops answering before the node has released its data directory, so a
	// launch attempted at that moment fails on the lock. The launch is retried
	// against a deadline rather than after a fixed sleep: what is being waited for
	// is the lock being released, which is observable and bounded.
	deadline := time.Now().Add(90 * time.Second)

	var last error

	for {
		command := exec.Command(daemon, append([]string{"-datadir=" + datadir, "-daemon"}, extraArgs...)...)

		output, err := command.CombinedOutput()

		if err == nil {
			break
		}

		last = fmt.Errorf("%w\n%s", err, output)

		if time.Now().After(deadline) {
			t.Fatalf("starting the %s node: %v", which, last)
		}

		time.Sleep(100 * time.Millisecond)
	}

	deadline = time.Now().Add(90 * time.Second)

	for {
		if _, err := r.node(cli, datadir, "getblockcount"); err == nil {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("the %s node did not come back within 90s", which)
		}

		time.Sleep(100 * time.Millisecond)
	}

	WalletReady(t, r)
}

// waitForDatadirRelease blocks until the node has released its data directory.
//
// The node holds an exclusive flock on <datadir>/.lock for its whole life. Taking
// that lock is therefore the observable end of the process, and it is the only
// thing that has to be waited for: RPC going quiet happens first.
func waitForDatadirRelease(t *testing.T, which, datadir string) {
	t.Helper()

	lockPath := filepath.Join(datadir, ".lock")
	deadline := time.Now().Add(90 * time.Second)

	for {
		handle, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)

		if err == nil {
			err = syscall.Flock(int(handle.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)

			if err == nil {
				_ = syscall.Flock(int(handle.Fd()), syscall.LOCK_UN)
				_ = handle.Close()

				return
			}

			_ = handle.Close()
		}

		if time.Now().After(deadline) {
			t.Fatalf("the %s node did not release %s within 90s", which, lockPath)
		}

		time.Sleep(50 * time.Millisecond)
	}
}

// WalletReady loads the wallets this harness needs, without touching capital.
func WalletReady(t *testing.T, r *Rig) {
	t.Helper()

	for _, chain := range []struct {
		cli     string
		datadir string
	}{
		{r.BTCCLI, r.BTCDatadir},
		{r.BlakeCLI, r.BlakeDatadir},
	} {
		loaded, err := r.node(chain.cli, chain.datadir, "listwallets")

		if err != nil {
			t.Fatalf("listwallets: %v", err)
		}

		for _, wallet := range []string{"alice", "bob", "alice-watch", "bob-watch"} {
			if strings.Contains(loaded, `"`+wallet+`"`) {
				continue
			}

			_, _ = r.node(chain.cli, chain.datadir, "loadwallet", wallet)
		}
	}
}
