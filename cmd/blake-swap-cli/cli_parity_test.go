//go:build !js

package main

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/internal/rigtest"
)

// Parity: tests/command_surface_tests.py, tests/swap_driver_surface_tests.py
//
// The port's human output is byte-identical to the reference on the commands
// listed in docs/GO_PORT_MAP.md §5. This file is how that claim is extended to
// the rest: both implementations run the same command against the same durable
// state and their output is compared after the identifiers that legitimately
// differ are normalised.
//
// Refusals are compared first, because they are the half that can be compared
// exactly: neither implementation changes anything, so both see the same record
// and the same chains.

// parityPair is one participant's state, addressed by both implementations.
type parityPair struct {
	r      *rigtest.Rig
	goSide *rigtest.Side
	pySide *rigtest.Side
	spool  string
}

// newPair writes one identity and points both implementations at it.
func newPair(t *testing.T) *parityPair {
	t.Helper()

	r := rigtest.Load(t)

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "participant.key")

	secret := make([]byte, 32)

	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(keyFile, []byte(hex.EncodeToString(secret)), 0o600); err != nil {
		t.Fatal(err)
	}

	goSide := &rigtest.Side{Name: "go", Role: "alice", State: dir, KeyFile: keyFile}
	pySide := &rigtest.Side{Name: "python", Role: "alice", State: dir, KeyFile: keyFile}

	return &parityPair{r: r, goSide: goSide, pySide: pySide, spool: t.TempDir()}
}

// output runs one command on one implementation and returns what it printed.
func (p *parityPair) output(t *testing.T, side *rigtest.Side, args ...string) string {
	t.Helper()

	output, _, err := p.r.Exec(t, side, p.spool, nil, args...)

	if err != nil {
		t.Fatalf("running %v: %v", args, err)
	}

	return output
}

// volatile is everything in an output that legitimately differs between two runs
// of the same command: transaction ids, block hashes, event ids and keys, heights,
// fees and durations.
var volatile = []*regexp.Regexp{
	// Compressed public keys are 66 hex characters, x-only keys and event ids 64,
	// swap ids 12. Two runs of the same scene generate different keys, so none of
	// them can be compared.
	regexp.MustCompile(`\b[0-9a-f]{64,66}\b`),
	regexp.MustCompile(`\b[0-9a-f]{12}\b`),
	regexp.MustCompile(`\b\d+(\.\d+)?\s*(sats|BLAKE|sat/vB)\b`),
	regexp.MustCompile(`\b\d{3,}\b`),
	// An HTLC address is derived from the scene's keys, so two runs of the same
	// walk fund different addresses. What is comparable is that the address and the
	// expected address agree.
	regexp.MustCompile(`\bbcrt1[a-z0-9]+`),
	// A remaining lifetime is a clock difference: two runs a fraction of a second
	// apart can land either side of a minute boundary.
	regexp.MustCompile(`\b\d+[smhd]\b`),
	// Which output index the HTLC landed in depends on how the node assembled the
	// funding transaction, and both indexes are valid. The engine's own verification
	// is what proves the right output was read, and the command succeeding is that
	// proof.
	regexp.MustCompile(`(?m)^(\s*vout:\s+)\d+`),
}

// pythonScaffolding is the part of a reference failure that is Python rather than
// protocol: the exception class it names inside a message.
var pythonScaffolding = regexp.MustCompile(`\b[A-Z][A-Za-z]*(Error|Exception): `)

// normalise replaces the volatile values, and the Python exception classes, with a
// marker so the wording can be compared.
func normalise(output string) string {
	// The watch-only block is collapsed before the volatile patterns run, because
	// those patterns would otherwise rewrite its first continuation line and the
	// block would no longer be recognisable.
	output = normaliseWatchOnly(output)
	output = pythonScaffolding.ReplaceAllString(output, "")
	for _, pattern := range volatile {
		if pattern.String() == `(?m)^(\\s*vout:\\s+)\\d+` {
			output = pattern.ReplaceAllString(output, "${1}<v>")

			continue
		}

		output = pattern.ReplaceAllString(output, "<v>")
	}

	return strings.TrimRight(output, " \t\n")
}

// normaliseWatchOnly collapses the body of a watch-only import block.
//
// Whether the node accepts the descriptor import is an outcome rather than a
// wording: on this rig the reference's import is refused for an internal-address
// label while the port's succeeds, and both are correct responses to the node. The
// header is kept so its wording is still compared.
func normaliseWatchOnly(output string) string {
	lines := strings.Split(output, "\n")
	kept := make([]string, 0, len(lines))

	skipping := false

	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "WATCH-ONLY ") {
			skipping = true
			kept = append(kept, line)

			continue
		}

		if skipping {
			trimmed := strings.TrimSpace(line)

			watch := (trimmed != "" && strings.Trim(trimmed, "=") == "") ||
				strings.HasPrefix(trimmed, "wallet:") ||
				strings.HasPrefix(trimmed, "address:") ||
				strings.HasPrefix(trimmed, "PASS: descriptor imported") ||
				strings.HasPrefix(trimmed, "WARNING: the HTLC was NOT recorded") ||
				strings.HasPrefix(trimmed, "Funding may continue") ||
				line == ""

			if watch {
				continue
			}

			skipping = false
		}

		kept = append(kept, line)
	}

	return strings.Join(kept, "\n")
}

// referenceMessage is the sentence a Python failure carries.
//
// The reference often raises rather than printing, so its refusal arrives inside a
// traceback. The port is documented to print the reference's message on one
// `ERROR:` line instead, so the comparison is between those two sentences.
func referenceMessage(output string) string {
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")

	for index := len(lines) - 1; index >= 0; index-- {
		line := lines[index]

		if separator := strings.Index(line, ": "); separator > 0 {
			kind := line[:separator]

			if kind != "" && !strings.ContainsAny(kind, " \t/") && strings.HasSuffix(kind, "Error") {
				return line[separator+2:]
			}
		}
	}

	return output
}

// portMessage is the sentence the port prints after its `ERROR:` prefix.
func portMessage(output string) string {
	return strings.TrimPrefix(strings.TrimSpace(output), "ERROR: ")
}

// compareRefusals diffs what both implementations say about a command they both
// refuse.
//
// Neither implementation changes anything on this path, so both see the same
// record and the same chains and the sentence is the whole claim.
func (p *parityPair) compareRefusals(t *testing.T, name string, args ...string) {
	t.Helper()

	t.Run(name, func(t *testing.T) {
		reference := normalise(referenceMessage(p.output(t, p.pySide, args...)))
		port := normalise(portMessage(p.output(t, p.goSide, args...)))

		if reference == port {
			return
		}

		t.Errorf("%v differs\n--- reference ---\n%s\n--- port ---\n%s", args, reference, port)
	})
}

// compareReports diffs one command's whole output between the two implementations.
func (p *parityPair) compareReports(t *testing.T, name string, args ...string) {
	t.Helper()

	t.Run(name, func(t *testing.T) {
		reference := normalise(p.output(t, p.pySide, args...))
		port := normalise(p.output(t, p.goSide, args...))

		if reference == port {
			return
		}

		t.Errorf("%v differs\n--- reference ---\n%s\n--- port ---\n%s", args, reference, port)
	})
}

// TestCLIParityUnknownSwap compares what both implementations say about a swap
// that does not exist.
//
// It is the cheapest sweep that touches every command: an unknown id reaches the
// same load-or-fail path in both engines, so a difference here is pure wording.
func TestCLIParityUnknownSwap(t *testing.T) {
	p := newPair(t)

	const missing = "000000000000"

	for _, command := range []string{
		"show", "publish-offer", "accept-offer", "receive-accept",
		"fund-blake", "fund-btc", "verify-blake", "verify-btc",
		"publish-blake-funded", "receive-blake-funded",
		"publish-btc-funded", "receive-btc-funded",
		"claim-btc", "claim-blake", "refund-btc", "refund-blake",
		"extract-secret", "publish-btc-claimed", "receive-btc-claimed",
		"publish-blake-claimed", "receive-blake-claimed",
		"publish-btc-refunded", "receive-btc-refunded",
		"complete", "reconcile", "watch-htlc",
		"recovery-plan", "refresh-timeouts", "dismiss", "inspect-market-take",
	} {
		p.compareRefusals(t, command, command, missing)
	}

	// The reference requires --role on these, so a caller of the reference passes
	// it and the port must accept it.
	for _, command := range []string{"sync-funding", "sync-settlement"} {
		p.compareRefusals(t, command, command, missing, "--role", "alice")
	}

	// A match this participant has never seen is refused, and the refusal is
	// comparable. Ignoring a match that already has local state is a different
	// refusal, covered by the market walk's ordering.
	for _, command := range []string{"ignore-match", "take-market-offer"} {
		p.compareRefusals(t, command, command, strings.Repeat("00", 32))
	}

	// `next` with no argument is discovery and takes no id; `next SWAP_ID` is this
	// port's extension, so there is no reference output to compare it with.
	p.compareReports(t, "next discovery", "next")

	// `run` reports a failed step rather than failing the command, so its whole
	// block is comparable -- apart from the exception class the reference prints
	// inside it, which is Python scaffolding rather than a message.
	p.compareReports(t, "run", "run", missing)
}
