package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/reconcile"
	"github.com/gjbrr/blake-swap/pkg/state"
)

// The resume command is discovery plus a handoff, and its output is the contract an operator
// reads before the driver starts doing things. These tests pin the words; the parity with the
// reference on a live state tree was verified separately, and is what the wording is copied
// from.

// resumeRecord is one participant's swap.
func resumeRecord(id string, current state.State) *model.Swap {
	return &model.Swap{SwapID: id, State: current}
}

// TestResumeRendersNoResumable: an empty queue says so, and names what is blocking.
func TestResumeRendersNoResumable(t *testing.T) {
	blocked := resumeItem{
		record: resumeRecord("aaaa00000001", state.Offered),
		reason: "protocol Bob settlement pubkeys were never created or imported",
	}

	text := renderNoResumable([]resumeItem{blocked})

	for _, want := range []string{
		"\nNO RESUMABLE SWAPS\n",
		"==================\n",
		"No swap currently has an automatic recovery, settlement, or waiting path.\n",
		"NEEDS MANUAL ATTENTION\n",
		"aaaa00000001  OFFERED  protocol Bob settlement pubkeys were never created or imported\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q:\n%s", want, text)
		}
	}

	// With nothing blocking, the attention section is absent rather than empty.
	if strings.Contains(renderNoResumable(nil), "NEEDS MANUAL ATTENTION") {
		t.Fatal("an empty attention list was printed")
	}
}

// TestResumeRendersMultipleResumableRefusesToGuess: two live paths is a decision, and the
// command does not make it.
func TestResumeRendersMultipleResumableRefusesToGuess(t *testing.T) {
	text := renderMultipleResumable([]resumeItem{{
		record: resumeRecord("aaaa00000001", state.BTCFunded),
		action: reconcile.ActionClaimBTC,
	}}, nil)

	for _, want := range []string{
		"\nMULTIPLE ACTIVE SWAPS\n",
		"Refusing to guess which one you intend to resume.\n",
		"aaaa00000001  BTC_FUNDED        claim-btc",
		"Run `resume <short-swap-prefix>` for the active swap you want.\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q:\n%s", want, text)
		}
	}
}

// TestResumeRendersTheHandoff: the block that names the swap, the checkpoint and the action
// before the driver takes over.
func TestResumeRendersTheHandoff(t *testing.T) {
	text := renderResuming(resumeItem{
		record: resumeRecord("aaaa00000001", state.BTCFunded),
		action: reconcile.ActionClaimBTC,
	}, []resumeItem{{record: resumeRecord("bbbb00000002", state.Offered)}})

	for _, want := range []string{
		"\nRESUMING SWAP\n",
		"swap:        aaaa00000001\n",
		"checkpoint:  BTC_FUNDED\n",
		"next:        claim-btc\n",
		"NOTE: 1 unrelated record(s) need manual attention but do not block this swap.\n",
		"Handing this swap to the existing autonomous driver.\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q:\n%s", want, text)
		}
	}
}

// TestResumeRendersTheAttentionRefusal: a manual-attention swap is not handed to the runner.
func TestResumeRendersTheAttentionRefusal(t *testing.T) {
	text := renderResumeAttention(resumeItem{
		record: resumeRecord("aaaa00000001", state.Offered),
		reason: "reconciliation failed during discovery",
	})

	for _, want := range []string{
		"\nMANUAL ATTENTION REQUIRED\n",
		"swap:   aaaa00000001\n",
		"reason: reconciliation failed during discovery\n",
		"Refusing to hand a manual-attention swap to the autonomous runner.\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q:\n%s", want, text)
		}
	}
}

// TestResumeRendersTerminalSwaps: an explicit prefix for a finished swap answers for it rather
// than claiming it does not exist.
func TestResumeRendersTerminalSwaps(t *testing.T) {
	settled := renderTerminal(resumeRecord("aaaa00000001", state.Complete))

	for _, want := range []string{
		"\nSWAP ALREADY SETTLED\n",
		"====================\n",
		"state: COMPLETE\n",
		"No action required.\n",
	} {
		if !strings.Contains(settled, want) {
			t.Fatalf("settled is missing %q:\n%s", want, settled)
		}
	}

	refunded := renderTerminal(resumeRecord("bbbb00000002", state.BlakeRefunded))

	if !strings.Contains(refunded, "SWAP ALREADY REFUNDED") ||
		!strings.Contains(refunded, "state: BLAKE_REFUNDED") {
		t.Fatalf("refunded block is wrong:\n%s", refunded)
	}
}

// TestResumeRendersAmbiguousPrefix: every candidate is listed, so the operator can lengthen
// the prefix.
func TestResumeRendersAmbiguousPrefix(t *testing.T) {
	text := renderAmbiguousResume([]resumeItem{
		{record: resumeRecord("aaaa00000001", state.BTCFunded), class: reconcile.ClassActionable,
			action: reconcile.ActionClaimBTC},
		{record: resumeRecord("aaaa00000002", state.Offered), class: reconcile.ClassManualAttention,
			action: reconcile.ActionManualAttention},
	})

	for _, want := range []string{
		"\nAMBIGUOUS SWAP PREFIX\n",
		"aaaa00000001 actionable claim-btc\n",
		"aaaa00000002 manual-attention manual-attention\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q:\n%s", want, text)
		}
	}
}

// TestResumePrefixValidates: the prefix is checked before the scan, so a typo is reported as a
// typo rather than as "no swap matches".
func TestResumePrefixValidates(t *testing.T) {
	if _, err := resumePrefix("abc"); err == nil ||
		!strings.Contains(err.Error(), "too short") {
		t.Fatalf("a three-character prefix was accepted: %v", err)
	}

	// Four is the minimum, and case does not matter.
	got, err := resumePrefix("  ABCD  ")

	if err != nil || got != "abcd" {
		t.Fatalf("prefix = %q, %v", got, err)
	}

	// No prefix at all is the scanning case, not an error.
	if value, err := resumePrefix("   "); err != nil || value != "" {
		t.Fatalf("blank prefix = %q, %v", value, err)
	}
}

// TestResumeHandsOffOnlyLiveSwaps: a manual-attention swap, or one whose report could not be
// built, is not handed to the driver.
func TestResumeHandsOffOnlyLiveSwaps(t *testing.T) {
	if !shouldHandOff(resumeItem{class: reconcile.ClassActionable}) {
		t.Fatal("an actionable swap was not handed off")
	}

	if !shouldHandOff(resumeItem{class: reconcile.ClassWaiting}) {
		t.Fatal("a waiting swap was not handed off")
	}

	for name, item := range map[string]resumeItem{
		"manual attention": {class: reconcile.ClassManualAttention},
		"failed report":    {failed: true},
	} {
		if shouldHandOff(item) {
			t.Errorf("%s: was handed to the driver", name)
		}
	}
}

// TestResumeForwardsTheStepBudget: the resume command's --max-steps reaches the driver
// explicitly, because a resume that silently used the driver's default would ignore the one
// knob an operator has over how much the automatic path may do in one invocation.
func TestResumeForwardsTheStepBudget(t *testing.T) {
	args := driverHandoffArgs("aaaa00000001", 1)

	want := []string{"aaaa00000001", "--max-steps", "1"}

	if len(args) != len(want) {
		t.Fatalf("args = %v", args)
	}

	for index := range want {
		if args[index] != want[index] {
			t.Fatalf("args = %v, want %v", args, want)
		}
	}
}

// TestUsageListsEveryCommand: the usage text is the only documentation a person has at the
// terminal, so a command missing from it is a command nobody finds.
func TestUsageListsEveryCommand(t *testing.T) {
	// The commands the dispatcher knows, from the parity manifest's list.
	commands := []string{
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

	text := captureUsage(t)

	// The usage writes several commands per line ("publish-btc-claimed SWAP_ID |
	// receive-btc-claimed SWAP_ID"), so the check is on whole tokens rather than on
	// prefixes: a command listed only as an argument to another is not documented.
	tokens := map[string]bool{}

	for _, field := range strings.FieldsFunc(text, func(r rune) bool {
		return r == ' ' || r == '\n' || r == '\t' || r == '|'
	}) {
		tokens[field] = true
	}

	for _, command := range commands {
		if !tokens[command] {
			t.Errorf("the usage text does not list %q", command)
		}
	}
}

// captureUsage reads the usage text by redirecting os.Stderr while usage runs.
func captureUsage(t *testing.T) string {
	t.Helper()

	// usage writes to stderr through fmt.Fprint, so the test reads the file descriptor it
	// is pointed at rather than the terminal's.
	original := os.Stderr

	reader, writer, err := os.Pipe()

	if err != nil {
		t.Fatal(err)
	}

	os.Stderr = writer

	defer func() { os.Stderr = original }()

	usage()

	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := io.ReadAll(reader)

	if err != nil {
		t.Fatal(err)
	}

	return string(raw)
}
