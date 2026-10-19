package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/store"
)

// TestRenderMatchImportedMatchesTheReference pins the taker's import confirmation.
func TestRenderMatchImportedMatchesTheReference(t *testing.T) {
	matchID := strings.Repeat("ab", 32)

	got := renderMatchImported(matchID, "aaaa00000001")

	want := "\nMARKET MATCH IMPORTED\n" +
		"=====================\n" +
		"match id: " + matchID + "\n" +
		"swap id:  aaaa00000001\n" +
		"\n" +
		"PASS: swap offer signer == matched Alice\n" +
		"PASS: Bob identity == signed take\n" +
		"PASS: market negotiation tuple durably bound to swap\n" +
		"PASS: Bob still does NOT know secret S\n" +
		"PASS: no funds moved\n"

	if got != want {
		t.Fatalf("renderMatchImported mismatch:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

// TestRenderMatchAlreadyImportedMatchesTheReference: a repeated import must read as a
// no-op, not as a second swap.
func TestRenderMatchAlreadyImportedMatchesTheReference(t *testing.T) {
	matchID := strings.Repeat("ab", 32)

	got := renderMatchAlreadyImported(matchID, "aaaa00000001")

	want := "\nMARKET MATCH ALREADY IMPORTED\n" +
		"=============================\n" +
		"match id: " + matchID + "\n" +
		"swap id:  aaaa00000001\n" +
		"\n" +
		"PASS: duplicate command did not create new Bob keys\n" +
		"PASS: immutable market negotiation binding preserved\n" +
		"PASS: missing binding, if any, was repaired\n"

	if got != want {
		t.Fatalf("renderMatchAlreadyImported mismatch:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

// bindingFixture is one proposed binding, as the command builds it.
func bindingFixture() map[string]any {
	return map[string]any{
		"version":        1,
		"offer_id":       strings.Repeat("11", 32),
		"take_id":        strings.Repeat("22", 32),
		"swap_id":        "aaaa00000001",
		"maker":          strings.Repeat("33", 32),
		"taker":          strings.Repeat("44", 32),
		"blake_amount":   json.Number("125.0"),
		"btc_sats":       json.Number("150000"),
		"first_match_id": strings.Repeat("55", 32),
	}
}

// TestMatchBindingConflictAcceptsTheSameNegotiation: re-signing the same match must not
// change anything, including when the stored numbers arrive as JSON floats.
func TestMatchBindingConflictAcceptsTheSameNegotiation(t *testing.T) {
	existing := bindingFixture()
	existing["blake_amount"] = float64(125)
	existing["btc_sats"] = float64(150000)

	if err := matchBindingConflict(existing, bindingFixture()); err != nil {
		t.Fatalf("an identical negotiation was refused: %v", err)
	}
}

// TestMatchBindingConflictRefusesEveryRedefinition: each field of the tuple is part of
// what the binding promises, so changing any one of them is a different negotiation.
func TestMatchBindingConflictRefusesEveryRedefinition(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"offer": func(b map[string]any) { b["offer_id"] = strings.Repeat("99", 32) },
		"take":  func(b map[string]any) { b["take_id"] = strings.Repeat("99", 32) },
		"swap":  func(b map[string]any) { b["swap_id"] = "bbbb00000002" },
		"maker": func(b map[string]any) { b["maker"] = strings.Repeat("99", 32) },
		"taker": func(b map[string]any) { b["taker"] = strings.Repeat("99", 32) },
		"blake": func(b map[string]any) { b["blake_amount"] = float64(126) },
		"btc":   func(b map[string]any) { b["btc_sats"] = float64(150001) },
	} {
		existing := bindingFixture()
		mutate(existing)

		err := matchBindingConflict(existing, bindingFixture())

		if err == nil {
			t.Errorf("%s: a changed negotiation was accepted", name)

			continue
		}

		if !strings.Contains(err.Error(), "disagrees with validated match") {
			t.Errorf("%s: refusal = %v", name, err)
		}
	}
}

// TestMatchBindingConflictRefusesAnIncompleteRecord: a binding missing part of the tuple
// cannot be checked, so it is malformed rather than consistent.
func TestMatchBindingConflictRefusesAnIncompleteRecord(t *testing.T) {
	existing := bindingFixture()

	delete(existing, "taker")

	err := matchBindingConflict(existing, bindingFixture())

	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("an incomplete binding was accepted: %v", err)
	}
}

// TestReadBindingRefusesGarbage: an unreadable binding is reported, not ignored, because
// ignoring it would silently replace evidence.
func TestReadBindingRefusesGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aaaa00000001.json")

	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := readBinding(path); err == nil ||
		!strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("garbage was accepted: %v", err)
	}

	missing, err := readBinding(filepath.Join(t.TempDir(), "absent.json"))

	if err != nil || missing != nil {
		t.Fatalf("a missing binding = %v, %v", missing, err)
	}
}

// matchItem is a parsed match with the fields the import reads.
func matchItem() *marketflow.ParsedMatch {
	return &marketflow.ParsedMatch{
		Match: marketMatch("aaaa00000001"),
	}
}

func marketMatch(swapID string) market.Match {
	return market.Match{
		OfferID:     strings.Repeat("11", 32),
		TakeID:      strings.Repeat("22", 32),
		SwapID:      swapID,
		Maker:       strings.Repeat("33", 32),
		Taker:       strings.Repeat("44", 32),
		BlakeAmount: amount.Blake(12_500_000_000),
		BTCSats:     150_000,
	}
}

// TestRepeatAcceptMarketMatchRepairsTheMissingBinding: a swap that exists without its
// receipt is finished, not refused.
func TestRepeatAcceptMarketMatchRepairsTheMissingBinding(t *testing.T) {
	dir := t.TempDir()

	records := store.NewFile(dir)
	alice, bob := strings.Repeat("33", 32), strings.Repeat("44", 32)

	record := &model.Swap{
		SwapID:               "aaaa00000001",
		State:                "OFFERED",
		SecretHash:           strings.Repeat("00", 32),
		BlakeAmountBaseUnits: 12_500_000_000,
		BTCSats:              150_000,
		NostrAlicePub:        &alice,
		NostrBobPub:          &bob,
	}

	if err := records.Save(context.Background(), store.Bob, record); err != nil {
		t.Fatal(err)
	}

	bindingPath := filepath.Join(dir, matchBindingDirectory, "aaaa00000001.json")
	proposed := bindingFixture()

	if err := repeatAcceptMarketMatch(context.Background(), repeatMatch{
		MatchID: strings.Repeat("55", 32),
		Item:    matchItem(),
		Bob:     bob,
		Record:  record,
		Store:   records,
		Binding: nil,
		Path:    bindingPath,
		Write:   proposed,
	}); err != nil {
		t.Fatalf("repair failed: %v", err)
	}

	if !fileExists(bindingPath) {
		t.Fatal("the missing binding was not written")
	}
}

// TestRepeatAcceptMarketMatchRefusesDifferentTerms: a local swap that disagrees with the
// match is not the swap the match describes.
func TestRepeatAcceptMarketMatchRefusesDifferentTerms(t *testing.T) {
	dir := t.TempDir()

	records := store.NewFile(dir)
	alice, bob := strings.Repeat("33", 32), strings.Repeat("44", 32)

	for name, mutate := range map[string]func(*model.Swap){
		"alice identity": func(s *model.Swap) { other := strings.Repeat("99", 32); s.NostrAlicePub = &other },
		"bob identity":   func(s *model.Swap) { other := strings.Repeat("99", 32); s.NostrBobPub = &other },
		"blake more":     func(s *model.Swap) { s.BlakeAmountBaseUnits++ },
		"blake less":     func(s *model.Swap) { s.BlakeAmountBaseUnits-- },
		"btc more":       func(s *model.Swap) { s.BTCSats++ },
		"btc less":       func(s *model.Swap) { s.BTCSats-- },
	} {
		record := &model.Swap{
			SwapID:               "aaaa00000001",
			State:                "OFFERED",
			SecretHash:           strings.Repeat("00", 32),
			BlakeAmountBaseUnits: 12_500_000_000,
			BTCSats:              150_000,
			NostrAlicePub:        &alice,
			NostrBobPub:          &bob,
		}

		mutate(record)

		err := repeatAcceptMarketMatch(context.Background(), repeatMatch{
			MatchID: strings.Repeat("55", 32),
			Item:    matchItem(),
			Bob:     bob,
			Record:  record,
			Store:   records,
			Binding: bindingFixture(),
			Path:    filepath.Join(dir, "binding.json"),
			Write:   bindingFixture(),
		})

		if err == nil {
			t.Errorf("%s: a disagreeing swap was accepted", name)

			continue
		}

		if !strings.Contains(err.Error(), "SECURITY FAILURE") {
			t.Errorf("%s: refusal = %v", name, err)
		}
	}
}
