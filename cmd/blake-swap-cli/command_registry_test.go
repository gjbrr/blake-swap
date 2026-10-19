//go:build !js

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Parity: tests/cli_wiring_tests.py
//
// The reference suite guards a bug class that Go does not have: a command
// function that reads `args.something` its parser never declares. What Go does
// have is a dispatcher, so the same class appears as a command name that no case
// handles, or a flag a command advertises in its usage text but never registers.
// This is that check, over all 58 reference command names.

// referenceCommandNames is the reference parser's subcommands, in its order,
// plus the `agent` verb that replaces the reference's separate agent script.
//
// It is written out rather than derived from the dispatcher on purpose: this is
// the contract, and a list derived from the implementation could not catch a
// command that was dropped.
func referenceCommandNames() []string {
	return []string{
		"new", "show", "list", "status", "advance", "publish-offer", "accept-offer",
		"receive-accept", "fund", "fund-blake", "fund-btc", "verify-blake", "verify-btc",
		"publish-blake-funded", "receive-blake-funded", "publish-btc-funded",
		"receive-btc-funded", "claim-btc", "claim-blake", "refund-btc", "refund-blake",
		"extract-secret", "publish-btc-claimed", "receive-btc-claimed",
		"publish-blake-claimed", "receive-blake-claimed", "publish-btc-refunded",
		"receive-btc-refunded", "complete", "reconcile", "next", "resume", "run", "agent",
		"watch-htlc", "recovery-plan", "refresh-timeouts", "sync-funding",
		"sync-settlement", "dashboard", "resume-all", "market", "market-state",
		"publish-market-offer", "cancel-market-offer", "inspect-market-take",
		"take-market-offer", "market-takes", "accept-take", "accept-match",
		"accept-market-take", "accept-market-match", "accept-reverse-market-match",
		"accept-reverse-swap-offer", "publish-market-match", "market-matches",
		"ignore-match", "dismiss", "rebroadcast-nostr-event",
	}
}

// TestCommandRegistryDispatchesEveryReferenceCommand runs every command name and
// requires the dispatcher to have recognised it.
//
// The command is given a flag that no parser defines, so it fails either way;
// what is asserted is *which* failure. A name the dispatcher does not know says
// "unknown command", and that is the one answer that must never appear.
//
// Choosing a bad flag rather than valid arguments keeps this node-free and
// side-effect-free: no command can get far enough to touch a record or a chain.
func TestCommandRegistryDispatchesEveryReferenceCommand(t *testing.T) {
	binary := cliForNodeConfig(t)
	root := t.TempDir() // no rig: the command must not need one to be dispatched

	for _, command := range referenceCommandNames() {
		output, _ := runCLI(t, binary, root, command, "--definitely-not-a-flag")

		if strings.Contains(output, "unknown command") {
			t.Errorf("%s is listed but not dispatched:\n%s", command, output)
		}
	}
}

// TestUnknownCommandFailsLoudly is the other half: a name nobody defines must be
// refused, and refused by name so an operator can see the typo.
func TestUnknownCommandFailsLoudly(t *testing.T) {
	binary := cliForNodeConfig(t)
	root := t.TempDir()

	missing := "definitely-not-a-command"

	output, err := runCLI(t, binary, root, missing)

	if err == nil {
		t.Fatalf("an unknown command succeeded:\n%s", output)
	}

	if !strings.Contains(output, "unknown command: "+missing) {
		t.Fatalf("the refusal does not name the command:\n%s", output)
	}
}

// TestEveryDocumentedFlagIsRegistered: a flag the usage text advertises must be a
// flag some command registers. This is the Go shape of the reference's bug class:
// documentation and wiring drifting apart is silent until an operator uses it.
func TestEveryDocumentedFlagIsRegistered(t *testing.T) {
	registered := registeredFlagNames(t)

	if len(registered) < 10 {
		t.Fatalf("found only %d registered flags; the scan is wrong", len(registered))
	}

	tokens := strings.FieldsFunc(captureUsage(t), func(r rune) bool {
		return r == ' ' || r == '\n' || r == '\t' || r == '|'
	})

	documented := 0

	for _, token := range tokens {
		if !strings.HasPrefix(token, "--") {
			continue
		}

		documented++

		name := strings.TrimPrefix(token, "--")

		if !registered[name] {
			t.Errorf("the usage text documents --%s but no command registers it", name)
		}
	}

	if documented == 0 {
		t.Fatal("the usage text documents no flags at all")
	}
}

// TestEveryDispatchedCaseIsInTheRegistry closes the other direction: a case the
// dispatcher handles but the early check does not know is a command that would be
// refused before it ran.
//
// The flag package itself is what enforces registration -- `flags.String` both
// registers and reads, so the reference's unregistered-attribute bug class cannot
// occur here. What can drift is the *documented* flags, which the previous test
// pins.
func TestEveryDispatchedCaseIsInTheRegistry(t *testing.T) {
	repo := repoRootForTest(t)
	source := readPackageSource(t, repo)

	// Only the dispatcher's own switch counts. The package has other switches over
	// message types and booleans, and their cases are not commands.
	start := strings.Index(source, "switch command {")

	if start < 0 {
		t.Fatal("the dispatcher's switch is not in the package source")
	}

	end := strings.Index(source[start:], `fmt.Errorf("unknown command:`)

	if end < 0 {
		t.Fatal("the dispatcher's unknown-command branch is not in the package source")
	}

	dispatched := map[string]bool{}

	for _, group := range commandCase.FindAllStringSubmatch(source[start:start+end], -1) {
		for _, name := range regexp.MustCompile(`"([a-z0-9-]+)"`).FindAllStringSubmatch(group[1], -1) {
			dispatched[name[1]] = true
		}
	}

	if len(dispatched) < 50 {
		t.Fatalf("found only %d dispatched cases; the scan is wrong", len(dispatched))
	}

	for name := range dispatched {
		if _, known := commands[name]; !known {
			t.Errorf("the dispatcher handles %q but the command registry does not know it", name)
		}
	}
}

// commandCase matches the dispatch switch's case groups.
var commandCase = regexp.MustCompile(`case ((?:"[a-z0-9\-]+"(?:\s*,\s*)?)+):`)

// readPackageSource reads every non-test Go file in the command package.
func readPackageSource(t *testing.T, repo string) string {
	t.Helper()

	directory := filepath.Join(repo, "cmd", "blake-swap-cli")

	entries, err := os.ReadDir(directory)

	if err != nil {
		t.Fatal(err)
	}

	var source strings.Builder

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") ||
			strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(directory, entry.Name()))

		if err != nil {
			t.Fatal(err)
		}

		source.Write(data)
		source.WriteString("\n")
	}

	return source.String()
}

// registeredFlagNames collects every flag name the CLI registers.
var flagRegistration = regexp.MustCompile(
	`(?:flags|settlementFlags|flag)\.(?:String|Bool|Int64|Int|Uint64|Duration|Float64)\(\s*"([a-z0-9-]+)"`,
)

// registeredFlagNames scans the command package for flag registrations.
//
// The scan is deliberately about the union of names rather than about which
// command owns which: ownership follows the FlagSet each handler creates, and a
// test that re-derived that mapping from source text would be asserting its own
// parser rather than the program.
func registeredFlagNames(t *testing.T) map[string]bool {
	t.Helper()

	source := readPackageSource(t, repoRootForTest(t))

	names := map[string]bool{}

	for _, match := range flagRegistration.FindAllStringSubmatch(source, -1) {
		names[match[1]] = true
	}

	return names
}
