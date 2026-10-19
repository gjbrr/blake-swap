//go:build !js

// Parity: tests/installed_wheel_tests.py

package installcheck

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/relaystub"
)

// waiver is one Python wheel assertion that has no Go artefact to inspect.
type waiver struct {
	// name is the Python assertion being replaced.
	name string
	// reason names the Python assertion and the Go mechanism that covers the
	// real risk, so a waiver can never be a silent "not applicable".
	reason string
}

// waived records the wheel-specific sub-checks of
// tests/installed_wheel_tests.py that cannot have a Go counterpart.
//
// The Python suite inspects the built wheel: its member list, its generated
// console scripts, its metadata files and the inputs packaging reads. A Go
// installation is one linked executable and has none of those artefacts. Each
// entry below says which Python assertion it replaces and which test or helper
// in this package covers the risk that assertion was really guarding.
var waived = []waiver{
	{
		name: "wheel archive contains the expected engine module files",
		reason: "Replaces the Python assertion (test_imports_are_checkout_independent, " +
			"backed by test_wheel_install) that the built wheel's member list contains " +
			"swap_v2.py, swap_agent.py, swap_wire.py, fee_policy.py and " +
			"swap_timeout_policy.py. An installed Go program has no member list: " +
			"`go install ./cmd/blake-swap-cli` links one executable from the module's " +
			"package graph. The real risk -- an artifact missing code it needs -- is " +
			"covered by the Go mechanism in TestInstalledBinaryComesFromThisCheckout, " +
			"which requires the embedded build info to name " +
			"github.com/gjbrr/blake-swap and its cmd/blake-swap-cli package, and by " +
			"TestInstalledBinaryNodeFreeMarketRead, which executes the linked market, " +
			"relay-client, event-verification and JSON-rendering code.",
	},
	{
		name: "console entry points are generated for the installed distribution",
		reason: "Replaces the Python assertion in test_console_entry_points that pip " +
			"generates executable blake-swap-engine and blake-swap-agent console " +
			"scripts in the virtual environment's bin directory and that each one " +
			"answers --help. Go has no console-script generation step: installing a main " +
			"package writes exactly one executable named after the package directory. " +
			"The real risk -- an installation that leaves no runnable command -- is " +
			"covered by the Go mechanism Install, which fails unless " +
			"GOBIN/blake-swap-cli exists as an executable regular file, and by " +
			"TestInstalledBinaryRunsHelpOutsideCheckout, which executes that file for " +
			"--help, -h and help from outside the checkout.",
	},
	{
		name: "wheel metadata files are well formed",
		reason: "Replaces the Python assertions that the wheel's METADATA, WHEEL and " +
			"RECORD files exist and are well formed enough for pip to install the " +
			"distribution and for `pip check` to report a healthy dependency set. Go " +
			"binaries carry build information rather than wheel metadata. The real risk " +
			"-- an unidentifiable or dependency-inconsistent artifact -- is covered by " +
			"the Go mechanism TestInstalledBinaryComesFromThisCheckout, which runs " +
			"`go version -m` on the installed file and requires the module path " +
			"github.com/gjbrr/blake-swap and the cmd/blake-swap-cli package path; a " +
			"missing dependency or link failure fails `go install` before a binary " +
			"exists at all.",
	},
	{
		name: "packaging does not depend on the checkout's tests/ directory",
		reason: "Replaces the Python assertion, implicit in the clean-copy wheel build, " +
			"that packaging reads only pyproject.toml, README.md, LICENSE and src/ and " +
			"never the checkout's tests/ directory. Go's `go install " +
			"./cmd/blake-swap-cli` compiles only the import graph of the main package, so " +
			"test files and tests/ are not build inputs. The real risk -- an installed " +
			"artifact that needs the checkout at run time -- is covered by the Go " +
			"mechanism of running the installed binary with its working directory and " +
			"HOME in fresh temporary directories outside the repository, with a " +
			"from-scratch environment containing no repository path and no chain " +
			"discovery variable, and by asserting that the process leaves the empty " +
			"working directory and HOME empty while writing its relay cache only under " +
			"the configured temporary BLAKE_SWAP_STATE_DIR.",
	},
}

// TestInstalledBinaryComesFromThisCheckout proves that `go install` produced a
// real executable from this module and that the file is not a file inside the
// repository.
//
// This is the Go replacement for the Python suite's wheel-existence and
// installed-imports checks: the artifact must exist, be executable, be linked
// from github.com/gjbrr/blake-swap, and be a different file from every file in
// the checkout (including the checked-in bin/blake-swap-cli, which an accidental
// PATH lookup could otherwise have used).
func TestInstalledBinaryComesFromThisCheckout(t *testing.T) {
	install := Install(t)

	t.Log(install.String())

	if IsWithin(install.Repo, install.Binary) {
		t.Fatalf("installed binary lives inside the checkout: %s", install.Binary)
	}

	if !IsWithin(install.GOBIN, install.Binary) {
		t.Fatalf("installed binary %s is not inside the temporary GOBIN %s", install.Binary, install.GOBIN)
	}

	binaryInfo, err := os.Stat(install.Binary)
	if err != nil {
		t.Fatalf("stat installed binary: %v", err)
	}

	if binaryInfo.Size() == 0 {
		t.Fatalf("installed binary is empty: %s", install.Binary)
	}

	buildInfo := install.BuildInfo(t, GoTool(t))

	wantPath := "path\t" + ModulePath + "/cmd/blake-swap-cli"
	if !strings.Contains(buildInfo, wantPath) {
		t.Fatalf("installed binary build info does not name %q:\n%s", wantPath, buildInfo)
	}

	wantModule := "mod\t" + ModulePath + "\t"
	if !strings.Contains(buildInfo, wantModule) {
		t.Fatalf("installed binary build info does not name module %q:\n%s", ModulePath, buildInfo)
	}

	files := RepoFiles(t, install.Repo)

	if len(files) == 0 {
		t.Fatalf("repository %s contains no regular files; the distinct-file check would be vacuous", install.Repo)
	}

	for _, repoFile := range files {
		fileInfo, err := os.Stat(repoFile)
		if err != nil {
			t.Fatalf("stat repository file %s: %v", repoFile, err)
		}

		if os.SameFile(binaryInfo, fileInfo) {
			t.Fatalf("installed binary %s is the same file as repository file %s", install.Binary, repoFile)
		}
	}

	// The checked-in build is the file a PATH accident would most plausibly
	// execute instead of the fresh installation, so name it explicitly.
	checkedIn := filepath.Join(install.Repo, "bin", cliName)

	checkedInInfo, err := os.Stat(checkedIn)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}

		t.Fatalf("stat checked-in binary %s: %v", checkedIn, err)
	}

	if os.SameFile(binaryInfo, checkedInInfo) {
		t.Fatalf("installed binary is the checked-in file %s, not a fresh install", checkedIn)
	}
}

// TestInstalledBinaryRunsHelpOutsideCheckout proves the installed artifact
// answers usage questions from outside the checkout with no node, no identity,
// no state directory and no inherited environment.
//
// The working directory and HOME are fresh temporary directories outside the
// repository, the child environment contains only PATH and HOME, and the test
// asserts that both directories are still empty afterwards: the command cannot
// have read or written anything in the checkout.
func TestInstalledBinaryRunsHelpOutsideCheckout(t *testing.T) {
	install := Install(t)

	for _, command := range []string{"--help", "-h", "help"} {
		t.Run(command, func(t *testing.T) {
			work := t.TempDir()
			home := t.TempDir()

			assertOutsideRepo(t, install.Repo, work, home)

			env := ScrubbedEnv(home, "", nil)

			assertEnvHygiene(t, install.Repo, env, []string{"PATH", "HOME"})

			result := install.Run(t, RunOptions{Dir: work, Env: env}, command)

			if !strings.Contains(result.Stderr, "usage: blake-swap-cli <command> [arguments]") {
				t.Fatalf("%s did not print the CLI usage on stderr:\nstdout:\n%s\nstderr:\n%s",
					command, result.Stdout, result.Stderr)
			}

			if !strings.Contains(result.Stderr, "list") {
				t.Fatalf("%s usage does not list the list command:\n%s", command, result.Stderr)
			}

			assertEmptyDir(t, work)
			assertEmptyDir(t, home)
		})
	}
}

// TestInstalledBinaryNodeFreeMarketRead proves the installed binary does real
// work without a node: it connects to a loopback relay, subscribes, verifies a
// signed offer, derives the market state, writes the verified event to the
// configured temporary state directory and renders both the JSON and human
// views.
//
// The relay is a real WebSocket server on loopback, not a fake client. The
// environment contains PATH, HOME, BLAKE_SWAP_STATE_DIR and
// BLAKE_SWAP_NOSTR_RELAYS and nothing else; in particular both node datadirs,
// every key path and every other BLAKE_SWAP_* variable are absent. Running
// outside the checkout with an empty HOME and an empty working directory, and
// then finding the cache only under the temporary state directory, is the
// evidence that the artifact does not fall back to the repository.
func TestInstalledBinaryNodeFreeMarketRead(t *testing.T) {
	install := Install(t)

	stub, err := relaystub.New()
	if err != nil {
		t.Fatalf("start loopback relay: %v", err)
	}

	t.Cleanup(stub.Close)

	key, err := nostr.NewKey(strings.Repeat("31", 32))
	if err != nil {
		t.Fatalf("make offer identity: %v", err)
	}

	offerEvent, _, err := marketflow.BuildOffer(key, marketflow.BuildOfferOptions{
		BlakeAmount: amount.Blake(1_000_000_000),
		BTCSats:     25_000,
		Minutes:     30,
		Direction:   market.BlakeForBTC,
	})
	if err != nil {
		t.Fatalf("build signed market offer: %v", err)
	}

	stub.Inject(offerEvent)

	work := t.TempDir()
	home := t.TempDir()
	stateJSON := filepath.Join(t.TempDir(), "state-json")
	stateHuman := filepath.Join(t.TempDir(), "state-human")

	assertOutsideRepo(t, install.Repo, work, home, stateJSON, stateHuman)

	envJSON := ScrubbedEnv(home, stateJSON, map[string]string{
		"BLAKE_SWAP_NOSTR_RELAYS": stub.URL,
	})

	envHuman := ScrubbedEnv(home, stateHuman, map[string]string{
		"BLAKE_SWAP_NOSTR_RELAYS": stub.URL,
	})

	for _, env := range []struct {
		name string
		env  []string
	}{
		{"market-state", envJSON},
		{"market", envHuman},
	} {
		assertEnvHygiene(t, install.Repo, env.env, []string{
			"PATH", "HOME", "BLAKE_SWAP_STATE_DIR", "BLAKE_SWAP_NOSTR_RELAYS",
		})
	}

	// The JSON view is the machine-readable boundary, so it is the strongest
	// thing to parse: it proves the offer crossed the socket, verified and
	// reached the renderer.
	result := install.Run(t, RunOptions{Dir: work, Env: envJSON}, "market-state", "--json")

	var payload struct {
		Offers []struct {
			EventID   string `json:"event_id"`
			Maker     string `json:"maker"`
			Direction string `json:"direction"`
			State     string `json:"state"`
			BTCSats   int64  `json:"btc_sats"`
		} `json:"offers"`
	}

	if err := json.Unmarshal([]byte(result.Stdout), &payload); err != nil {
		t.Fatalf("market-state --json did not emit JSON: %v\nstdout:\n%s\nstderr:\n%s",
			err, result.Stdout, result.Stderr)
	}

	if len(payload.Offers) != 1 {
		t.Fatalf("installed binary saw %d offers, want the one on the relay:\n%s",
			len(payload.Offers), result.Stdout)
	}

	got := payload.Offers[0]

	if got.EventID != offerEvent.ID {
		t.Fatalf("offer event id = %q, want %q", got.EventID, offerEvent.ID)
	}

	if got.Maker != key.PublicKey() {
		t.Fatalf("offer maker = %q, want the signing key %q", got.Maker, key.PublicKey())
	}

	if got.Direction != string(market.BlakeForBTC) {
		t.Fatalf("offer direction = %q, want %q", got.Direction, market.BlakeForBTC)
	}

	if got.State != string(marketflow.StateOpen) {
		t.Fatalf("offer state = %q, want %q", got.State, marketflow.StateOpen)
	}

	if got.BTCSats != 25_000 {
		t.Fatalf("offer btc_sats = %d, want 25000", got.BTCSats)
	}

	// The verified event must have been cached under the temporary state
	// directory. That is the positive half of "it does not write into the
	// checkout": the one directory it was given is the one it used.
	cacheFile := filepath.Join(stateJSON, "nostr-event-cache", offerEvent.ID+".json")

	if info, err := os.Stat(cacheFile); err != nil {
		t.Fatalf("installed binary did not cache the verified event at %s: %v\nstdout:\n%s\nstderr:\n%s",
			cacheFile, err, result.Stdout, result.Stderr)
	} else if !info.Mode().IsRegular() {
		t.Fatalf("relay cache entry %s is not a regular file", cacheFile)
	}

	if !strings.Contains(result.Stdout, `"offers"`) {
		t.Fatalf("market-state --json output has no offers field:\n%s", result.Stdout)
	}

	// The human view is the second half of the real work: the same read must
	// produce the reference's listing, from a relay that answered. It gets its
	// own state directory so this invocation's cache is independent of the
	// JSON run's.
	listed := install.Run(t, RunOptions{Dir: work, Env: envHuman}, "market", "--limit", "50")

	if !strings.Contains(listed.Stdout, "Nostr market read:") ||
		!strings.Contains(listed.Stdout, "from 1/1 relay(s)") {
		t.Fatalf("market did not report a read from the loopback relay:\nstdout:\n%s\nstderr:\n%s",
			listed.Stdout, listed.Stderr)
	}

	if !strings.Contains(listed.Stdout, "1 open offer(s)") {
		t.Fatalf("market did not list the offer:\nstdout:\n%s\nstderr:\n%s",
			listed.Stdout, listed.Stderr)
	}

	// Each invocation cached the verified event under the state directory it
	// was given, and nowhere else.
	humanCache := filepath.Join(stateHuman, "nostr-event-cache", offerEvent.ID+".json")

	if _, err := os.Stat(humanCache); err != nil {
		t.Fatalf("market did not cache the verified event at %s: %v\nstdout:\n%s\nstderr:\n%s",
			humanCache, err, listed.Stdout, listed.Stderr)
	}

	// With HOME and the working directory pointed at empty temporary
	// directories, neither may gain state: all of it belongs in
	// BLAKE_SWAP_STATE_DIR, which is also outside the checkout.
	assertEmptyDir(t, work)
	assertEmptyDir(t, home)
}

// TestWaivedWheelChecksAreSpecific requires every waiver to be a real,
// specific substitution rather than a placeholder.
//
// A waiver is only honest if it names the Python assertion it replaces and the
// Go mechanism that covers the risk that assertion guarded, so the test checks
// for both and rejects the usual placeholders ("not applicable", "n/a",
// "tbd", "no counterpart").
func TestWaivedWheelChecksAreSpecific(t *testing.T) {
	if len(waived) == 0 {
		t.Fatal("no wheel waivers recorded")
	}

	seen := make(map[string]bool, len(waived))

	for _, entry := range waived {
		name := strings.TrimSpace(entry.name)

		if name == "" {
			t.Fatal("waiver has an empty name")
		}

		if seen[name] {
			t.Fatalf("waiver %q is recorded more than once", name)
		}

		seen[name] = true

		reason := strings.TrimSpace(entry.reason)

		if reason == "" {
			t.Fatalf("waiver %q has an empty reason", name)
		}

		if len(reason) < 120 {
			t.Fatalf("waiver %q has a reason too short to be specific (%d bytes): %q",
				name, len(reason), reason)
		}

		if !strings.Contains(reason, "Python") {
			t.Fatalf("waiver %q does not name the Python assertion it replaces: %q", name, reason)
		}

		if !strings.Contains(reason, "Go") {
			t.Fatalf("waiver %q does not name the Go mechanism that covers the risk: %q", name, reason)
		}

		lower := strings.ToLower(reason)

		for _, placeholder := range []string{"not applicable", "n/a", "tbd", "no counterpart"} {
			if strings.Contains(lower, placeholder) {
				t.Fatalf("waiver %q uses the placeholder %q instead of a specific reason: %q",
					name, placeholder, reason)
			}
		}
	}
}

// assertOutsideRepo fails unless every path is outside the checkout.
func assertOutsideRepo(t *testing.T, repo string, paths ...string) {
	t.Helper()

	for _, path := range paths {
		if IsWithin(repo, path) {
			t.Fatalf("test path %s is inside the checkout %s; it cannot prove independence", path, repo)
		}
	}
}

// assertEmptyDir fails unless dir exists and contains no entries.
func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read directory %s: %v", dir, err)
	}

	if len(entries) != 0 {
		names := make([]string, 0, len(entries))

		for _, entry := range entries {
			names = append(names, entry.Name())
		}

		t.Fatalf("directory %s is not empty after the installed binary ran: %v", dir, names)
	}
}

// assertEnvHygiene fails unless env contains only the allow-listed keys, none of
// the deliberately withheld chain/key variables, and no value that points into
// the checkout.
func assertEnvHygiene(t *testing.T, repo string, env []string, allowed []string) {
	t.Helper()

	allow := make(map[string]bool, len(allowed))

	for _, key := range allowed {
		allow[key] = true
	}

	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("malformed environment entry %q", entry)
		}

		if !allow[key] {
			t.Fatalf("child environment contains %q, which the command does not need", key)
		}

		for _, withheld := range WithheldEnvKeys {
			if key == withheld {
				t.Fatalf("child environment contains withheld variable %q", key)
			}
		}

		// Only absolute values are paths in the child. A relative value would
		// resolve against the child's working directory, which the tests place
		// outside the checkout; a URL is not a path at all.
		if value != "" && filepath.IsAbs(value) && IsWithin(repo, value) {
			t.Fatalf("child environment variable %s points into the checkout: %s", key, value)
		}
	}

	for _, key := range []string{"PATH", "HOME"} {
		if EnvValue(env, key) == "" {
			t.Fatalf("child environment is missing %s", key)
		}
	}
}
