package main

import (
	"context"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/amount"
)

// TestRenderTakenMatchesTheReference pins the confirmation block for a published take.
func TestRenderTakenMatchesTheReference(t *testing.T) {
	offerID := strings.Repeat("ab", 32)
	takeID := strings.Repeat("ef", 32)
	maker := strings.Repeat("11", 32)
	taker := strings.Repeat("22", 32)

	want := `
MARKET OFFER TAKEN
==================
offer id: ` + offerID + `
take id:  ` + takeID + `
maker:    ` + maker + `
taker:    ` + taker + `
selling:  0.1 BLAKE
asking:   25,000 sats

PASS: offer was open when taker took it
PASS: take request is signed by taker
PASS: no money has moved
`

	got := renderTaken(offerID, takeID, maker, taker, amount.Blake(10_000_000), 25_000)

	if got != want {
		t.Fatalf("renderTaken mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestRenderAlreadyTakenMatchesTheReference pins the reuse block.
func TestRenderAlreadyTakenMatchesTheReference(t *testing.T) {
	offerID := strings.Repeat("ab", 32)
	takeID := strings.Repeat("ef", 32)
	taker := strings.Repeat("22", 32)

	want := `
MARKET OFFER ALREADY TAKEN
==========================
offer id: ` + offerID + `
take id:  ` + takeID + `
taker:    ` + taker + `

PASS: existing signed take reused
PASS: no duplicate Nostr event created
PASS: no money has moved

Retrying publication of the exact same signed take...
`

	if got := renderAlreadyTaken(offerID, takeID, taker); got != want {
		t.Fatalf("renderAlreadyTaken mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestRenderPublishUnconfirmedSaysWhatSurvived: a failed retry must not read as a
// failed take.
func TestRenderPublishUnconfirmedSaysWhatSurvived(t *testing.T) {
	got := renderPublishUnconfirmed(errTest("no relay accepted the event"))

	for _, want := range []string{
		"WARNING: relay publication was not confirmed\n",
		"no relay accepted the event\n",
		"PASS: original signed take remains durably cached\n",
		"PASS: retry did not create a new take event\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output is missing %q:\n%s", want, got)
		}
	}
}

// errTest is an error with a fixed message.
type errTest string

func (e errTest) Error() string { return string(e) }

// TestResolveOfferPrefixValidates: a prefix is a convenience with rules, and each
// refusal names the rule.
func TestResolveOfferPrefixValidates(t *testing.T) {
	cases := map[string]string{
		"abcd":                  "too short",
		"zzzzzzzz":              "not hexadecimal",
		strings.Repeat("z", 64): "not hexadecimal",
	}

	for value, want := range cases {
		client := marketClient()

		_, err := resolveCachedOfferPrefix(context.Background(), client, value)

		if err == nil {
			t.Fatalf("%q was accepted", value)
		}

		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", value, err, want)
		}
	}
}

// TestHexOnly: the gate itself, both directions.
func TestHexOnly(t *testing.T) {
	for _, ok := range []string{"0123456789abcdef", "ABCDEF"} {
		if err := hexOnly(strings.ToLower(ok)); err != nil {
			t.Errorf("%q was refused: %v", ok, err)
		}
	}

	for _, bad := range []string{"0x12", "abcg", "abc def", ""} {
		if bad == "" {
			continue
		}

		if err := hexOnly(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}
