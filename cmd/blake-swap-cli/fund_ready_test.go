package main

import (
	"context"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/coord"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// TestRenderNoFundingReadyMatchesTheReference pins the block for an empty candidate set.
func TestRenderNoFundingReadyMatchesTheReference(t *testing.T) {
	want := "\nNO FUNDING-READY SWAP\n" +
		"=====================\n" +
		"No local protocol-Alice OFFERED swap has an authenticated Bob acceptance ready.\n" +
		"\n" +
		"The taker may still need to run `accept-match`.\n"

	if got := renderNoFundingReady(); got != want {
		t.Fatalf("renderNoFundingReady mismatch:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

// TestRenderFundingReadyMatchesTheReference pins the block naming the trade about to be
// funded — the last thing an operator reads before money moves.
func TestRenderFundingReadyMatchesTheReference(t *testing.T) {
	record := fundingSwap()

	got := renderFundingReady(record)

	want := "\nFUNDING-READY SWAP\n" +
		"==================\n" +
		"swap:  aaaa00000001\n" +
		"BLAKE: 125.0\n" +
		"sats:  150,000\n" +
		"\n"

	if got != want {
		t.Fatalf("renderFundingReady mismatch:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

// TestRenderMultipleFundingReadyRefusesToChoose: two trades ready to fund means a
// decision about money, and the command must not make it.
func TestRenderMultipleFundingReadyRefusesToChoose(t *testing.T) {
	second := fundingSwap()
	second.SwapID = "bbbb00000002"

	got := renderMultipleFundingReady([]fundingCandidate{
		{record: fundingSwap()},
		{record: second},
	})

	want := "\nMULTIPLE FUNDING-READY SWAPS\n" +
		"============================\n" +
		"Refusing to choose which trade receives funds.\n" +
		"\n" +
		"1. aaaa00000001  125.0 BLAKE / 150,000 sats\n" +
		"2. bbbb00000002  125.0 BLAKE / 150,000 sats\n" +
		"\n" +
		"Use `fund-blake <swap-id>` for the one you intend to commit.\n"

	if got != want {
		t.Fatalf("renderMultipleFundingReady mismatch:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

// TestRenderExplicitCommitmentMatchesTheReference: the sentence that says money moves.
func TestRenderExplicitCommitmentMatchesTheReference(t *testing.T) {
	want := "\nEXPLICIT COMMITMENT\n" +
		"===================\n" +
		"Funding the BLAKE HTLC now. This moves funds.\n" +
		"\n"

	if got := renderExplicitCommitment(); got != want {
		t.Fatalf("renderExplicitCommitment mismatch:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

// TestFundingCandidatesKeepsOnlyFundableSwaps: a swap is a candidate only when it is
// Alice's, still OFFERED, has a pinned Bob identity, and holds Bob's keys.
func TestFundingCandidatesKeepsOnlyFundableSwaps(t *testing.T) {
	me := strings.Repeat("11", 32)
	other := strings.Repeat("99", 32)

	funded := fundingSwap()

	notOffered := fundingSwap()
	notOffered.SwapID = "bbbb00000002"
	notOffered.State = "BLAKE_FUNDED"

	notMine := fundingSwap()
	notMine.SwapID = "cccc00000003"
	notMine.NostrAlicePub = &other

	noBob := fundingSwap()
	noBob.SwapID = "dddd00000004"
	noBob.NostrBobPub = nil

	noKeys := fundingSwap()
	noKeys.SwapID = "eeee00000005"
	noKeys.BlakeBobPub = nil
	noKeys.BTCBobPub = nil

	engine := &swap.Engine{}

	got := fundingCandidates(context.Background(), engine, []*model.Swap{
		funded, notOffered, notMine, noBob, noKeys,
	}, me)

	if len(got) != 1 || got[0].record.SwapID != "aaaa00000001" {
		t.Fatalf("candidates = %d", len(got))
	}

	if !got[0].imported {
		t.Fatal("a swap holding Bob's keys was not reported as already imported")
	}
}

// TestAcceptAvailableValidatesTheMessage: a pending accept is only evidence when it is
// Bob's, for this swap, for this hash, and carries both of his contract keys.
func TestAcceptAvailableValidatesTheMessage(t *testing.T) {
	record := fundingSwap()
	record.BlakeBobPub = nil
	record.BTCBobPub = nil

	good := map[string]any{
		"type":          "accept",
		"swap_id":       record.SwapID,
		"secret_hash":   record.SecretHash,
		"blake_bob_pub": strings.Repeat("77", 32),
		"btc_bob_pub":   strings.Repeat("77", 32),
	}

	engine := &swap.Engine{Transport: &stubTransport{body: good}}

	if !acceptAvailable(context.Background(), engine, record) {
		t.Fatal("a valid acceptance was not found")
	}

	// A message for another swap is not this swap's acceptance.
	other := map[string]any{}

	for key, value := range good {
		other[key] = value
	}

	other["swap_id"] = "bbbb00000002"

	if acceptAvailable(context.Background(), &swap.Engine{
		Transport: &stubTransport{body: other},
	}, record) {
		t.Fatal("an acceptance for another swap was accepted")
	}

	// A different hash is a different trade.
	wrongHash := map[string]any{}

	for key, value := range good {
		wrongHash[key] = value
	}

	wrongHash["secret_hash"] = strings.Repeat("01", 32)

	if acceptAvailable(context.Background(), &swap.Engine{
		Transport: &stubTransport{body: wrongHash},
	}, record) {
		t.Fatal("an acceptance for another hash was accepted")
	}

	// And a message with no Bob keys cannot be imported at all.
	noKeys := map[string]any{}

	for key, value := range good {
		noKeys[key] = value
	}

	delete(noKeys, "btc_bob_pub")

	if acceptAvailable(context.Background(), &swap.Engine{
		Transport: &stubTransport{body: noKeys},
	}, record) {
		t.Fatal("an acceptance without Bob's Bitcoin key was accepted")
	}
}

// stubTransport answers Receive with one fixed body and never sends.
type stubTransport struct {
	body map[string]any
}

func (s *stubTransport) Send(
	context.Context, string, string, map[string]any, coord.SendOptions,
) (string, error) {
	return "", nil
}

func (s *stubTransport) Receive(
	context.Context, string, string, coord.ReceiveOptions,
) (*coord.Message, error) {
	return &coord.Message{Kind: "accept", Sender: strings.Repeat("22", 32), Body: s.body}, nil
}

// fundingSwap is an OFFERED swap with Bob's keys already imported.
func fundingSwap() *model.Swap {
	alice, bob := strings.Repeat("11", 32), strings.Repeat("22", 32)
	blakeKey, btcKey := strings.Repeat("77", 32), strings.Repeat("88", 32)

	return &model.Swap{
		SwapID:               "aaaa00000001",
		State:                "OFFERED",
		SecretHash:           strings.Repeat("00", 32),
		BlakeAmountBaseUnits: 12_500_000_000,
		BTCSats:              150_000,
		NostrAlicePub:        &alice,
		NostrBobPub:          &bob,
		BlakeBobPub:          &blakeKey,
		BTCBobPub:            &btcKey,
	}
}
