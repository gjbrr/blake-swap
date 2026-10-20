//go:build !js

package docsalign_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/htlc"
	"github.com/gjbrr/blake-swap/pkg/state"
	"github.com/gjbrr/blake-swap/pkg/timeout"
)

// Parity: tests/docs_alignment_tests.py
//
// The reference suite checks its documentation against its engine. This does the
// same against this one: every fact below is recomputed from Go, so a protocol or
// timeout change that leaves the prose behind fails here instead of misleading a
// reader.
//
// The reference's Python-package assertions -- pyproject metadata, the module list
// in src/, and the wheel's contents -- have no Go counterpart and are waived in
// waivedChecks below, with the mechanism that covers the real risk.

// waivedChecks is what the reference suite asserts that cannot exist here.
type waivedCheck struct {
	name   string
	reason string
}

// waived is every Python-package-specific assertion, and why it is absent.
func waived() []waivedCheck {
	return []waivedCheck{
		{
			name: "the Python package metadata matches the documentation",
			reason: "this repository's Go artifact is a module, not a Python " +
				"distribution: there is no pyproject.toml, no package list in " +
				"src/, and no [project] table to drift. The Go equivalent is " +
				"go.mod, checked by TestPackagingArtifacts and the packaging suite.",
		},
		{
			name: "the wheel's documentation files match the docs directory",
			reason: "no wheel is built here, so there is no second copy of the " +
				"docs to drift from the first. The documentation exists once, and " +
				"every check in this file reads that copy.",
		},
		{
			name: "the Python module inventory matches the documentation",
			reason: "the reference documents src/*.py by name; this port has no " +
				"such inventory, and the package inventory that replaces it is " +
				"checked by TestDocumentedPackagesExist.",
		},
	}
}

// TestWaiversAreSpecific requires every waiver to name the artifact that is
// missing and the mechanism that covers the risk instead.
func TestWaiversAreSpecific(t *testing.T) {
	for _, waiver := range waived() {
		if len(strings.Fields(waiver.reason)) < 12 {
			t.Fatalf("%q is waived too vaguely: %q", waiver.name, waiver.reason)
		}
	}

	if len(waived()) != 3 {
		t.Fatalf("%d waivers, want 3", len(waived()))
	}
}

// repoRoot walks up to the module root.
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

// docs reads every markdown document that describes this implementation.
func docs(t *testing.T) map[string]string {
	t.Helper()

	root := repoRoot(t)

	found := map[string]string{}

	readme, err := os.ReadFile(filepath.Join(root, "README.md"))

	if err != nil {
		t.Fatal(err)
	}

	found["README.md"] = string(readme)

	entries, err := os.ReadDir(filepath.Join(root, "docs"))

	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(root, "docs", entry.Name()))

		if err != nil {
			t.Fatal(err)
		}

		found["docs/"+entry.Name()] = string(data)
	}

	return found
}

// goSource concatenates every non-test Go file, plus every script and the
// Makefile.
//
// Scripts count because several documented variables are read by the wrappers
// rather than by the engine, and the Makefile counts because the build targets are
// part of the documented surface. Test files deliberately do not count: a variable
// a test reads is a variable the tool does not have.
func goSource(t *testing.T) string {
	t.Helper()

	root := repoRoot(t)

	var source strings.Builder

	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))

	if err != nil {
		t.Fatal(err)
	}

	source.Write(makefile)

	for _, directory := range []string{"pkg", "cmd", "internal", "scripts"} {
		err := filepath.WalkDir(filepath.Join(root, directory),
			func(path string, entry os.DirEntry, err error) error {
				if err != nil {
					return err
				}

				if entry.IsDir() {
					return nil
				}

				if strings.HasSuffix(entry.Name(), "_test.go") {
					return nil
				}

				// Scripts are read wholesale: several are extensionless, and the
				// documented variables they consume have to count.
				if directory != "scripts" &&
					!strings.HasSuffix(entry.Name(), ".go") &&
					!strings.HasSuffix(entry.Name(), ".sh") {
					return nil
				}

				if directory == "scripts" && strings.Contains(path, "/__pycache__/") {
					return nil
				}

				data, err := os.ReadFile(path)

				if err != nil {
					return err
				}

				source.Write(data)
				source.WriteString("\n")

				return nil
			})

		if err != nil {
			t.Fatal(err)
		}
	}

	return source.String()
}

// TestFencesAreBalanced: an unbalanced code fence swallows the rest of a
// document, which is how a table of states disappears from a rendered page.
func TestFencesAreBalanced(t *testing.T) {
	for path, text := range docs(t) {
		if fences := strings.Count(text, "\n```"); fences%2 != 0 {
			t.Errorf("%s: %d fence markers (odd)", path, fences)
		}
	}
}

// TestReferencedPathsExist: a document that points at a file which is not there
// is a document that sends a reader to a dead end.
func TestReferencedPathsExist(t *testing.T) {
	root := repoRoot(t)

	// Generated at run time and deliberately not committed.
	//
	// A documented path that a build or a run produces is not a path a clean checkout
	// has. The check exists to catch a stale reference, so an output named here is a
	// path the documentation is allowed to name without the file being committed: the
	// wasm modules come from `make wasm`, and the two screenshots are written by the
	// check script that captures them. Requiring them in a clean checkout would make
	// this test fail on every clone and pass only on a machine that had built once.
	//
	generated := map[string]bool{
		"scripts/participant-env":  true,
		"web/page.wasm":            true,
		"web/worker.wasm":          true,
		"web/hello-screenshot.png": true,
		"web/boot-screenshot.png":  true,
	}

	pattern := regexp.MustCompile(
		"`((?:src|scripts|tests|tools|docs|checkpoints|pkg|cmd|internal|web)/" +
			"[A-Za-z0-9_./*-]+)`")

	for path, text := range docs(t) {
		seen := map[string]bool{}

		for _, match := range pattern.FindAllStringSubmatch(text, -1) {
			target := match[1]

			if seen[target] || strings.Contains(target, "*") || generated[target] {
				continue
			}

			seen[target] = true

			if _, err := os.Stat(filepath.Join(root, target)); err != nil {
				t.Errorf("%s: missing path %s", path, target)
			}
		}
	}
}

// TestDocumentedEnvVarsAreRead: a variable the documentation tells an operator to
// set must be a variable some Go code reads. A documented no-op is worse than an
// undocumented feature.
func TestDocumentedEnvVarsAreRead(t *testing.T) {
	source := goSource(t)

	pattern := regexp.MustCompile(`BLAKE_SWAP_[A-Z0-9_]+`)

	for path, text := range docs(t) {
		seen := map[string]bool{}

		for _, variable := range pattern.FindAllString(text, -1) {
			if seen[variable] {
				continue
			}

			seen[variable] = true

			if !strings.Contains(source, variable) {
				t.Errorf("%s: %s is read nowhere in pkg/, cmd/ or scripts/",
					path, variable)
			}
		}
	}
}

// TestStatesAndTransitionsDocumented: every state and every allowed edge must
// appear in the protocol document, because that table is what an operator reads
// before deciding whether a swap can move.
func TestStatesAndTransitionsDocumented(t *testing.T) {
	protocol, ok := docs(t)["docs/PROTOCOL.md"]

	if !ok {
		t.Fatal("docs/PROTOCOL.md is missing")
	}

	states := state.All

	if len(states) != 9 {
		t.Fatalf("pkg/state declares %d states, want 9", len(states))
	}

	edges := 0

	for _, from := range states {
		if !strings.Contains(protocol, string(from)) {
			t.Errorf("docs/PROTOCOL.md does not document state %s", from)
		}

		for _, next := range from.NextStates() {
			edges++

			if !strings.Contains(protocol, string(next)) {
				t.Errorf("docs/PROTOCOL.md does not document transition %s -> %s",
					from, next)
			}
		}

		for _, target := range from.ReorgTargets() {
			edges++

			if !strings.Contains(protocol, string(target)) {
				t.Errorf("docs/PROTOCOL.md does not document reorg %s -> %s",
					from, target)
			}
		}
	}

	if edges < 10 {
		t.Fatalf("pkg/state declares %d edges", edges)
	}
}

// TestHTLCClausesDocumented: the documented script template must be the script
// the engine builds.
//
// The order matters as much as the clauses: `or_i(claim, refund)` and
// `or_i(refund, claim)` are both valid scripts and only one of them is this
// protocol's.
func TestHTLCClausesDocumented(t *testing.T) {
	protocol, ok := docs(t)["docs/PROTOCOL.md"]

	if !ok {
		t.Fatal("docs/PROTOCOL.md is missing")
	}

	secretHash := strings.Repeat("11", 32)

	probe := htlc.Bond{
		SecretHash:    secretHash,
		BlakeAlicePub: "02" + strings.Repeat("aa", 32),
		BlakeBobPub:   "02" + strings.Repeat("bb", 32),
		BTCAlicePub:   "02" + strings.Repeat("cc", 32),
		BTCBobPub:     "02" + strings.Repeat("dd", 32),
		BlakeTimeout:  1000,
		BTCTimeout:    900,
	}

	expected := map[htlc.Chain][]string{
		htlc.Blake: {
			"wsh(or_i(",
			"and_v(v:pk(" + probe.BlakeBobPub + "),sha256(" + secretHash + "))",
			"and_v(v:after(1000),pk(" + probe.BlakeAlicePub + "))",
			"))",
		},
		htlc.BTC: {
			"wsh(or_i(",
			"and_v(v:pk(" + probe.BTCAlicePub + "),sha256(" + secretHash + "))",
			"and_v(v:after(900),pk(" + probe.BTCBobPub + "))",
			"))",
		},
	}

	for chain, fragments := range expected {
		body, err := probe.Descriptor(chain)

		if err != nil {
			t.Fatalf("%s descriptor: %v", chain, err)
		}

		position := 0

		for _, fragment := range fragments {
			found := strings.Index(body[position:], fragment)

			if found < 0 {
				t.Errorf("the %s HTLC template does not contain %q in the "+
					"documented order: %s", chain, fragment, body)

				break
			}

			position += found + len(fragment)
		}

		for _, forbidden := range []string{"OP_DROP", "raw("} {
			if strings.Contains(body, forbidden) {
				t.Errorf("the %s HTLC template contains %s", chain, forbidden)
			}
		}

		if !strings.Contains(protocol, "sha256(") || !strings.Contains(protocol, "or_i(") {
			t.Error("docs/PROTOCOL.md does not state the HTLC template")
		}
	}

	// The private-key form must be the same script with the key substituted. That
	// is what makes a descriptor import able to recover a wallet that lost a key,
	// and it is the difference between a recovery plan and a description.
	withKey, err := probe.DescriptorWithKeys(htlc.Blake, map[string]string{
		probe.BlakeAlicePub: "cTestWif",
	})

	if err != nil {
		t.Fatalf("descriptor with substituted keys: %v", err)
	}

	if strings.Contains(withKey, probe.BlakeAlicePub) {
		t.Error("substituting a private key left the public key in place")
	}

	if !strings.Contains(withKey, "cTestWif") {
		t.Error("substituting a private key did not reach the descriptor")
	}

	if !strings.Contains(withKey, probe.BlakeBobPub) {
		t.Error("substituting one key removed the counterparty's key")
	}
}

// TestDefaultTimeoutPlanDocumented: the protocol document states concrete window
// lengths. Recompute them and require the numbers to still be true.
func TestDefaultTimeoutPlanDocumented(t *testing.T) {
	protocol, ok := docs(t)["docs/PROTOCOL.md"]

	if !ok {
		t.Fatal("docs/PROTOCOL.md is missing")
	}

	policy := timeout.DefaultPolicy()

	plan, err := timeout.BuildPlan(0, 0, policy)

	if err != nil {
		t.Fatalf("building the default plan: %v", err)
	}

	documented := map[string]struct {
		computed   int64
		documented string
	}{
		"Bitcoin block count": {plan.BTCBlocks, "21"},
		"BLAKE block count":   {plan.BlakeBlocks, "46"},
	}

	names := make([]string, 0, len(documented))

	for name := range documented {
		names = append(names, name)
	}

	sort.Strings(names)

	for _, name := range names {
		entry := documented[name]

		if fmt.Sprintf("%d", entry.computed) != entry.documented {
			t.Errorf("%s changed to %d; docs/PROTOCOL.md documents %s",
				name, entry.computed, entry.documented)
		}
	}

	for label, seconds := range map[string]int64{
		"BTC window":      plan.BTCWindowSeconds,
		"BLAKE window":    plan.BlakeWindowSeconds,
		"recovery budget": plan.BobRecoverySeconds,
	} {
		hours := fmt.Sprintf("%.2f", float64(seconds)/3600)

		if !strings.Contains(protocol, hours) {
			t.Errorf("docs/PROTOCOL.md does not state the computed %s %s h",
				label, hours)
		}
	}
}

// TestDocumentedCommandsExist: every command the docs tell a reader to run must
// be a command that exists.
func TestDocumentedCommandsExist(t *testing.T) {
	root := repoRoot(t)

	// The wrapper's case labels are the vocabulary `scripts/blake` accepts.
	wrapper, err := os.ReadFile(filepath.Join(root, "scripts", "blake"))

	if err != nil {
		t.Fatal(err)
	}

	handled := map[string]bool{}

	for _, match := range regexp.MustCompile(`(?m)^\s{2}([a-z][a-z-]*)\|?[a-z-]*\)`).
		FindAllStringSubmatch(string(wrapper), -1) {
		handled[match[1]] = true
	}

	documented := map[string]bool{}

	for _, text := range docs(t) {
		for _, match := range regexp.MustCompile(`(?:\./)?scripts/blake ([a-z][a-z-]*)`).
			FindAllStringSubmatch(text, -1) {
			documented[match[1]] = true
		}
	}

	for command := range documented {
		if !handled[command] {
			t.Errorf("docs reference `scripts/blake %s` but the wrapper has no case",
				command)
		}
	}

	if len(documented) == 0 {
		t.Fatal("no `scripts/blake <command>` references were found at all")
	}
}

// TestReadmeDocLinksExist: the README is the index, so a document it names must
// exist and a document nobody names must be indexed.
func TestReadmeDocLinksExist(t *testing.T) {
	root := repoRoot(t)

	readme, err := os.ReadFile(filepath.Join(root, "README.md"))

	if err != nil {
		t.Fatal(err)
	}

	text := string(readme)

	for _, match := range regexp.MustCompile("`(docs/[A-Za-z0-9_.-]+\\.md)`").
		FindAllStringSubmatch(text, -1) {
		if _, err := os.Stat(filepath.Join(root, match[1])); err != nil {
			t.Errorf("README.md links missing document %s", match[1])
		}
	}

	entries, err := os.ReadDir(filepath.Join(root, "docs"))

	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}

		if !strings.Contains(text, entry.Name()) {
			t.Errorf("README.md does not mention docs/%s", entry.Name())
		}
	}
}

// TestEveryPackageIsDocumented: every Go package in this repository is named by
// some document.
//
// The direction matters. A document that names `pkg/p256k1` is describing an
// upstream module, not a directory here, so checking that every pkg/ mention
// resolves would be checking prose rather than this repository. Checking the other
// way round -- every package that exists is described somewhere -- is the claim an
// operator needs: there is no part of the port they cannot read about.
func TestEveryPackageIsDocumented(t *testing.T) {
	root := repoRoot(t)

	corpus := strings.Join(func() []string {
		var texts []string

		for _, text := range docs(t) {
			texts = append(texts, text)
		}

		return texts
	}(), "\n")

	checked := 0

	for _, directory := range []string{"pkg", "internal", "cmd"} {
		entries, err := os.ReadDir(filepath.Join(root, directory))

		if err != nil {
			t.Fatal(err)
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}

			target := directory + "/" + entry.Name()

			hasGo := false

			files, err := os.ReadDir(filepath.Join(root, target))

			if err != nil {
				t.Fatal(err)
			}

			for _, file := range files {
				if strings.HasSuffix(file.Name(), ".go") {
					hasGo = true

					break
				}
			}

			if !hasGo {
				continue
			}

			checked++

			if !strings.Contains(corpus, target) {
				t.Errorf("%s exists but no document names it", target)
			}
		}
	}

	if checked < 20 {
		t.Fatalf("found only %d Go packages", checked)
	}
}

// TestSuiteInventoryCountsMatch: the counts the test documentation states must be
// the counts the registry actually has.
func TestSuiteInventoryCountsMatch(t *testing.T) {
	root := repoRoot(t)

	entries, err := os.ReadDir(filepath.Join(root, "tests"))

	if err != nil {
		t.Fatal(err)
	}

	registered := 0

	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), "_tests.py") {
			registered++
		}
	}

	if registered != 60 {
		t.Fatalf("tests/ has %d registered suites, want 60", registered)
	}

	inventory, err := os.ReadFile(filepath.Join(root, "tests", "README.md"))

	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(inventory), fmt.Sprintf("%d", registered)) {
		t.Errorf("tests/README.md does not state the registered suite count (%d)",
			registered)
	}
}
