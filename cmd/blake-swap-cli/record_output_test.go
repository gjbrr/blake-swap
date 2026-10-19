package main

import (
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/model"
)

// swapWithEverything is a record that exercises every line of the reference's print_swap.
func swapWithEverything() *model.Swap {
	version := "1.5"
	secret := strings.Repeat("66", 32)
	blakeFunding := strings.Repeat("aa", 32)
	btcFunding := strings.Repeat("bb", 32)
	btcClaim := strings.Repeat("cc", 32)
	blakeClaim := strings.Repeat("dd", 32)
	btcWindow, blakeWindow, recovery := int64(12_600), int64(27_600), int64(14_700)

	return &model.Swap{
		SwapID:                    "e333364be8ee",
		State:                     "OFFERED",
		BlakeAmountBaseUnits:      1_000_000_000,
		BTCSats:                   50_000,
		SecretHash:                strings.Repeat("17", 32),
		SecretHex:                 &secret,
		BTCTimeout:                3076,
		BlakeTimeout:              4872,
		TimeoutPolicyVersion:      &version,
		BTCTimeoutWindowSeconds:   &btcWindow,
		BobRecoverySeconds:        &recovery,
		BlakeTimeoutWindowSeconds: &blakeWindow,
		BlakeFundingTxID:          &blakeFunding,
		BTCFundingTxID:            &btcFunding,
		BTCCLaimTxID:              &btcClaim,
		BlakeClaimTxID:            &blakeClaim,
	}
}

// TestRenderSwapMatchesTheReference pins print_swap, verified against the reference on a
// real record written by each implementation.
func TestRenderSwapMatchesTheReference(t *testing.T) {
	got := renderSwap(swapWithEverything())

	want := "\nATOMIC SWAP\n" +
		"===========\n" +
		"swap id:        e333364be8ee\n" +
		"state:          OFFERED\n" +
		"\n" +
		"TRADE\n" +
		"-----\n" +
		"BLAKE:          10.0\n" +
		"BTC sats:       50000\n" +
		"\n" +
		"HASHLOCK\n" +
		"--------\n" +
		"secret:         " + strings.Repeat("66", 32) + "\n" +
		"SHA256:         " + strings.Repeat("17", 32) + "\n" +
		"\n" +
		"TIMEOUTS\n" +
		"--------\n" +
		"BTC timeout:    3076\n" +
		"BLAKE timeout:  4872\n" +
		"BTC window:     12600 s\n" +
		"Bob recovery:   14700 s\n" +
		"BLAKE window:   27600 s\n" +
		"\n" +
		"TRANSACTIONS\n" +
		"------------\n" +
		"BLAKE funding:  " + strings.Repeat("aa", 32) + "\n" +
		"BTC funding:    " + strings.Repeat("bb", 32) + "\n" +
		"BTC claim:      " + strings.Repeat("cc", 32) + "\n" +
		"BLAKE claim:    " + strings.Repeat("dd", 32) + "\n"

	if got != want {
		t.Fatalf("renderSwap mismatch:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

// TestRenderSwapOmitsWindowsWithoutV15Policy: a record from an older policy has no window
// fields, and printing zeros would claim margins it never recorded.
func TestRenderSwapOmitsWindowsWithoutV15Policy(t *testing.T) {
	record := swapWithEverything()
	record.TimeoutPolicyVersion = nil
	record.SecretHex = nil
	record.BlakeFundingTxID = nil
	record.BTCFundingTxID = nil
	record.BTCCLaimTxID = nil
	record.BlakeClaimTxID = nil

	got := renderSwap(record)

	for _, unwanted := range []string{"BTC window:", "Bob recovery:", "BLAKE window:"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("a non-v1.5 record printed %q:\n%s", unwanted, got)
		}
	}

	for _, wanted := range []string{
		"secret:         None\n",
		"BLAKE funding:  -\n",
		"BTC claim:      -\n",
	} {
		if !strings.Contains(got, wanted) {
			t.Fatalf("output is missing %q:\n%s", wanted, got)
		}
	}
}

// TestRenderSwapListMatchesTheReference pins the list layout, including the second
// section: the reference lists the same participant-local records under both roles.
func TestRenderSwapListMatchesTheReference(t *testing.T) {
	records := []*model.Swap{
		{SwapID: "e333364be8ee", State: "OFFERED"},
		{SwapID: "aaaa00000001", State: "COMPLETE"},
	}

	got := renderSwapList(records)

	want := "\nALICE\n" +
		"=====\n" +
		"e333364be8ee OFFERED\n" +
		"aaaa00000001 COMPLETE\n" +
		"\n" +
		"BOB\n" +
		"===\n" +
		"e333364be8ee OFFERED\n" +
		"aaaa00000001 COMPLETE\n"

	if got != want {
		t.Fatalf("renderSwapList mismatch:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

// TestRenderSwapListEmptyMatchesTheReference: an empty tree says so in one line.
func TestRenderSwapListEmptyMatchesTheReference(t *testing.T) {
	if got := renderSwapList(nil); got != "No swaps.\n" {
		t.Fatalf("empty list = %q", got)
	}
}
