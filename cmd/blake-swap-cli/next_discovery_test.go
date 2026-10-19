//go:build !js

// Parity: tests/swap_driver_surface_tests.py

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/coord"
	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/fund"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/relaystub"
	"github.com/gjbrr/blake-swap/pkg/store"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// The reference's `next` is the command an autonomous scheduler calls in a loop, so what
// these tests pin is the loop: which single step it chooses, what it prints while choosing,
// and — for every refusal — that it performed nothing at all.
//
// The whole file runs on a loopback relay and a local message spool, so it needs no node.
// The chain surface `next` reads is only ever asked for a height and an empty wallet scan,
// which is what nextFakeNode answers.

// ---------------------------------------------------------------- chain surface

// nextFakeNode is one chain: enough of a node for the decisions `next` makes.
//
// The embedded interface is nil on purpose. A method this file did not implement is a
// method `next` should not be reaching on an unfunded negotiation, and embedding nil makes
// that a loud panic in the test rather than a silently plausible zero value.
type nextFakeNode struct {
	swap.Node
	height int64
}

func (n nextFakeNode) Height(context.Context) (int64, error) { return n.height, nil }

func (n nextFakeNode) DescriptorInfo(_ context.Context, body string) (*chain.DescriptorInfo, error) {
	sum := sha256.Sum256([]byte(body))

	return &chain.DescriptorInfo{
		Descriptor: body + "#" + hex.EncodeToString(sum[:4]),
		Checksum:   hex.EncodeToString(sum[:4]),
		IsSolvable: true,
	}, nil
}

func (n nextFakeNode) DeriveAddresses(_ context.Context, descriptor string) ([]string, error) {
	sum := sha256.Sum256([]byte(descriptor))

	return []string{"bcrt1q" + hex.EncodeToString(sum[:10])}, nil
}

func (n nextFakeNode) BlockHash(_ context.Context, height int64) (string, error) {
	return fmt.Sprintf("%064x", height), nil
}

func (n nextFakeNode) BlockTxs(_ context.Context, hash string) (*chain.VerboseBlock, error) {
	return &chain.VerboseBlock{Hash: hash}, nil
}

func (n nextFakeNode) Bind(name string) chain.Wallet { return nextFakeWallet{name: name} }

// nextFakeWallet is a wallet with a balance and nothing else.
type nextFakeWallet struct {
	chain.Wallet
	name string
}

func (w nextFakeWallet) Name() string { return w.name }

func (w nextFakeWallet) Balances(context.Context) (*chain.Balances, error) {
	return chain.BalancesFromTrusted("5"), nil
}

// NewAddress and AddressInfo are what the engine asks a wallet for when it needs a
// participant's contract key. Deriving both from the wallet name keeps them stable within a
// test while keeping two participants' keys distinct.
func (w nextFakeWallet) NewAddress(context.Context) (string, error) {
	sum := sha256.Sum256([]byte(w.name))

	return "bcrt1q" + hex.EncodeToString(sum[:20]), nil
}

func (w nextFakeWallet) AddressInfo(_ context.Context, address string) (*chain.AddressInfo, error) {
	sum := sha256.Sum256([]byte(w.name))

	// A compressed secp256k1 key is 02 or 03 followed by an x-coordinate. Clearing the top
	// bit keeps the x below the field modulus for every name, so the fake key is well formed
	// without the test pretending to own it.
	sum[0] &= 0x7f

	return &chain.AddressInfo{
		Address:  address,
		Pubkey:   "02" + hex.EncodeToString(sum[:32]),
		IsMine:   true,
		Solvable: true,
	}, nil
}

// ---------------------------------------------------------------- participants

// nextParticipant is one side of a scenario: a state root, an identity, an engine and a
// message spool shared with the other side.
type nextParticipant struct {
	name     string
	role     string
	state    string
	messages string
	key      *nostr.Key
	engine   *swap.Engine
}

// newNextParticipant writes one participant's state root and engine.
func newNextParticipant(t *testing.T, name, role, secret, messages string) *nextParticipant {
	t.Helper()

	state := t.TempDir()

	key, err := nostr.NewKey(secret)

	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}

	records := store.NewFile(state)
	records.SetMessageDirectory(messages)

	node := nextFakeNode{height: 100}

	engine := &swap.Engine{
		Store:       records,
		Intents:     fund.NewFileStore(filepath.Join(state, "funding-intents")),
		Blake:       node,
		BTC:         node,
		Identity:    key,
		Transport:   &coord.FileTransport{Spool: records, Sender: key.PublicKey()},
		Role:        role,
		BlakeWallet: role + "-blake",
		BTCWallet:   role + "-btc",
	}

	return &nextParticipant{
		name: name, role: role, state: state, messages: messages,
		key: key, engine: engine,
	}
}

// activate makes this participant the one the package-level helpers configure themselves
// from.
//
// Every market command in this package reads its state root, its relay list and its cache
// location from the environment at call time, exactly as the reference does. A test that
// runs two participants therefore switches between them here rather than threading a
// configuration through code that must keep behaving like the CLI.
func (p *nextParticipant) activate(t *testing.T) {
	t.Helper()

	t.Setenv("BLAKE_SWAP_STATE_DIR", p.state)
	t.Setenv("BLAKE_SWAP_MESSAGE_DIR", p.messages)
}

// nextRig is two participants and a relay: the arrangement the reference suite uses.
type nextRig struct {
	t        *testing.T
	stub     *relaystub.Stub
	messages string
	alice    *nextParticipant
	bob      *nextParticipant
}

// newNextRig starts a loopback relay and two identities.
func newNextRig(t *testing.T) *nextRig {
	t.Helper()

	stub, err := relaystub.New()

	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(stub.Close)

	messages := t.TempDir()

	// The fee overrides are what let a market offer be built without a node: the minimum
	// settlement-feasible BTC leg is derived from the fee policy, and an exact fee makes
	// that derivation local.
	t.Setenv("BLAKE_SWAP_BTC_SPEND_FEE_SATS", "1000")
	t.Setenv("BLAKE_SWAP_BLAKE_SPEND_FEE_SATS", "1000")
	t.Setenv("BLAKE_SWAP_NOSTR_RELAYS", stub.URL)
	t.Setenv(nextFreshEnv, "3600")

	return &nextRig{
		t:        t,
		stub:     stub,
		messages: messages,
		alice:    newNextParticipant(t, "maker", "alice", strings.Repeat("31", 32), messages),
		bob:      newNextParticipant(t, "taker", "bob", strings.Repeat("42", 32), messages),
	}
}

// publishOffer advertises an offer from one participant and returns its event id.
//
// The id is read from the command's own confirmation block rather than from the relay,
// because a participant may already have advertised other offers and "the newest event" is
// not the same question as "the event this call published".
func (r *nextRig) publishOffer(who *nextParticipant, blake, sats, minutes string) string {
	r.t.Helper()

	who.activate(r.t)

	output, err := captureStdout(r.t, func() error {
		return runPublishMarketOffer(context.Background(), who.engine,
			[]string{"--blake", blake, "--sats", sats, "--minutes", minutes,
				"--direction", string(market.BlakeForBTC)})
	})

	if err != nil {
		r.t.Fatalf("%s publish-market-offer: %v\n%s", who.name, err, output)
	}

	return firstFieldValue(r.t, output, "offer id")
}

// takeOffer has one participant take an offer and returns the take's event id.
func (r *nextRig) takeOffer(who *nextParticipant, offerID string) string {
	r.t.Helper()

	who.activate(r.t)

	output, err := captureStdout(r.t, func() error {
		return runTakeMarketOffer(context.Background(), who.engine, []string{offerID})
	})

	if err != nil {
		r.t.Fatalf("%s take-market-offer: %v\n%s", who.name, err, output)
	}

	return firstFieldValue(r.t, output, "take id")
}

// acceptTake has the maker accept a take, which creates the swap and publishes the match.
func (r *nextRig) acceptTake(who *nextParticipant, takeID string) (swapID, matchID string) {
	r.t.Helper()

	who.activate(r.t)

	output, err := captureStdout(r.t, func() error {
		return runAcceptMarketTake(context.Background(), who.engine, []string{takeID})
	})

	if err != nil {
		r.t.Fatalf("%s accept-market-take: %v\n%s", who.name, err, output)
	}

	return firstFieldValue(r.t, output, "swap id"), firstFieldValue(r.t, output, "market match")
}

// next runs the argument-less `next` for one participant and returns what it printed.
func (r *nextRig) next(who *nextParticipant) (string, error) {
	r.t.Helper()

	who.activate(r.t)

	return captureStdout(r.t, func() error {
		return runNextDiscovery(context.Background(), who.engine, nil)
	})
}

// relayEventIDs is the set of event ids the relay is holding.
func (r *nextRig) relayEventIDs() map[string]bool {
	ids := map[string]bool{}

	for _, stored := range r.stub.Events() {
		ids[stored.ID] = true
	}

	return ids
}

// localRecords lists the swap records in one participant's state root.
func localRecords(t *testing.T, participant *nextParticipant) []string {
	t.Helper()

	entries, err := os.ReadDir(participant.state)

	if err != nil {
		t.Fatal(err)
	}

	var ids []string

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		stem := strings.TrimSuffix(entry.Name(), ".json")

		if len(stem) == 12 {
			ids = append(ids, stem)
		}
	}

	return ids
}

// firstFieldValue reads the first "<label>: value" line of a confirmation block.
func firstFieldValue(t *testing.T, output, label string) string {
	t.Helper()

	for _, line := range strings.Split(output, "\n") {
		rest, ok := strings.CutPrefix(line, label+":")

		if !ok {
			continue
		}

		if value := strings.TrimSpace(rest); value != "" {
			return value
		}
	}

	t.Fatalf("no %q in output:\n%s", label, output)

	return ""
}

// captureStdout runs one command and returns everything it printed.
//
// `next` prints with fmt.Print to os.Stdout, so a test that wants to compare whole blocks
// byte for byte needs the real stream rather than a renderer's return value.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()

	file, err := os.CreateTemp(t.TempDir(), "stdout")

	if err != nil {
		t.Fatal(err)
	}

	original := os.Stdout
	os.Stdout = file

	// The restore is deferred, so a failure inside the command restores the real stream
	// before the panic unwinds: a test that left os.Stdout pointing at a closed file would
	// break every test after it.
	defer func() { os.Stdout = original }()

	runErr := fn()

	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(file.Name())

	if err != nil {
		t.Fatal(err)
	}

	return string(raw), runErr
}

// blakeUnits parses a coin amount the way the CLI does, so a test never invents base units.
func blakeUnits(t *testing.T, value string) amount.Blake {
	t.Helper()

	parsed, err := amount.ParseBlake(value)

	if err != nil {
		t.Fatal(err)
	}

	return parsed
}

// assertOutputIs compares a command's whole output with the expected block.
func assertOutputIs(t *testing.T, got, want string) {
	t.Helper()

	if got == want {
		return
	}

	t.Fatalf("output mismatch:\n--- got ---\n%q\n--- want ---\n%q", got, want)
}

// nextHeader is the block every performed step starts with.
func nextHeader(label string) string {
	return "\nNEXT SAFE STEP\n" +
		"==============\n" +
		label + "\n" +
		"\n" +
		"PASS: this coordination step does not itself fund, claim, or refund an HTLC\n" +
		"\n"
}

// nextNothingToDo is the whole quiet block, with no history to report.
const nextNothingToDo = "\nNOTHING TO DO\n" +
	"=============\n" +
	"No unambiguous current coordination, funding-ready trade, or resumable settlement was found.\n" +
	"\n" +
	"Use `scripts/blake market` to browse offers or `scripts/blake swaps` for the dashboard.\n"

// nextTail is the block every performed step ends with.
const nextTail = "\nPASS: coordination step completed\n" +
	"\n" +
	"Run `blake next` again to see what is safe now.\n"

// ---------------------------------------------------------------- scenarios

// TestNextTakesTheOnlyOpenOffer is the reference's first automatic step: an offer from
// somebody else, taken by publishing a signed take and nothing more.
func TestNextTakesTheOnlyOpenOffer(t *testing.T) {
	rig := newNextRig(t)

	offerID := rig.publishOffer(rig.alice, "10", "50000", "30")

	before := rig.relayEventIDs()

	output, err := rig.next(rig.bob)

	if err != nil {
		t.Fatalf("next: %v\n%s", err, output)
	}

	label := fmt.Sprintf("Take open offer %s for 10.0 BLAKE / 50,000 sats", offerID[:12])

	if !strings.HasPrefix(output, nextHeader(label)) {
		t.Fatalf("the step block is not the reference's:\n%q", output)
	}

	if !strings.HasSuffix(output, nextTail) {
		t.Fatalf("the closing block is not the reference's:\n%q", output)
	}

	// Exactly one new event, and it is this taker's take of that offer.
	added := []event.Event{}

	for _, stored := range rig.stub.Events() {
		if !before[stored.ID] {
			added = append(added, stored)
		}
	}

	if len(added) != 1 {
		t.Fatalf("next published %d events, want 1: %v", len(added), added)
	}

	take := added[0]

	if !take.HasTag("t", marketflow.TakeTag) {
		t.Fatalf("the published event is not a market take: %v", take.Tags)
	}

	if take.Pubkey != rig.bob.key.PublicKey() {
		t.Fatal("the take was not signed by this taker")
	}

	if !take.HasTag("e", offerID) {
		t.Fatalf("the take does not reference the offer: %v", take.Tags)
	}

	// The whole block, byte for byte: header, the take command's own output, then the
	// closing lines. Fixing the order is what keeps the two engines comparable.
	want := nextHeader(label) +
		renderTaken(offerID, take.ID, rig.alice.key.PublicKey(), rig.bob.key.PublicKey(),
			blakeUnits(t, "10"), 50_000)

	assertOutputIs(t, output, want+nextTail)
}

// TestNextAcceptsTheOnlyValidTake is the maker's side: one take on one open offer is
// accepted, which binds the negotiation and moves nothing.
func TestNextAcceptsTheOnlyValidTake(t *testing.T) {
	rig := newNextRig(t)

	offerID := rig.publishOffer(rig.alice, "10", "50000", "30")
	takeID := rig.takeOffer(rig.bob, offerID)

	output, err := rig.next(rig.alice)

	if err != nil {
		t.Fatalf("next: %v\n%s", err, output)
	}

	label := fmt.Sprintf("Accept take %s on offer %s", takeID[:12], offerID[:12])

	if !strings.HasPrefix(output, nextHeader(label)) {
		t.Fatalf("the step block is not the reference's:\n%q", output)
	}

	if !strings.HasSuffix(output, nextTail) {
		t.Fatalf("the closing block is not the reference's:\n%q", output)
	}

	// The step really happened: the swap exists and both selection records are durable.
	records := localRecords(t, rig.alice)

	if len(records) != 1 {
		t.Fatalf("accepting the take created %d records, want 1: %v", len(records), records)
	}

	selection := filepath.Join(rig.alice.state, selectionsDirectory, "offer-"+offerID+".json")

	if !fileExists(selection) {
		t.Fatalf("the offer selection was not written: %s", selection)
	}

	if !fileExists(filepath.Join(rig.alice.state, selectionsDirectory, takeID+".json")) {
		t.Fatal("the take selection was not written")
	}
}

// TestNextImportsTheFreshMatch is the taker's second step: the maker's match is fresh (it
// is dated now), so importing it is the one unambiguous coordination action.
func TestNextImportsTheFreshMatch(t *testing.T) {
	rig := newNextRig(t)

	offerID := rig.publishOffer(rig.alice, "10", "50000", "30")
	takeID := rig.takeOffer(rig.bob, offerID)
	swapID, matchID := rig.acceptTake(rig.alice, takeID)

	output, err := rig.next(rig.bob)

	if err != nil {
		t.Fatalf("next: %v\n%s", err, output)
	}

	label := fmt.Sprintf("Import fresh market match %s for swap %s", matchID[:12], swapID)

	if !strings.HasPrefix(output, nextHeader(label)) {
		t.Fatalf("the step block is not the reference's:\n%q", output)
	}

	if !strings.HasSuffix(output, nextTail) {
		t.Fatalf("the closing block is not the reference's:\n%q", output)
	}

	records := localRecords(t, rig.bob)

	if len(records) != 1 || records[0] != swapID {
		t.Fatalf("the taker imported %v, want [%s]", records, swapID)
	}
}

// TestNextImportsThePendingAcceptance is Alice's import of Bob's authenticated acceptance.
func TestNextImportsThePendingAcceptance(t *testing.T) {
	rig := newNextRig(t)

	offerID := rig.publishOffer(rig.alice, "10", "50000", "30")
	takeID := rig.takeOffer(rig.bob, offerID)
	swapID, _ := rig.acceptTake(rig.alice, takeID)

	// Bob's own next imports the match, which is what publishes his acceptance.
	if _, err := rig.next(rig.bob); err != nil {
		t.Fatal(err)
	}

	output, err := rig.next(rig.alice)

	if err != nil {
		t.Fatalf("next: %v\n%s", err, output)
	}

	label := "Import Bob acceptance for swap " + swapID

	if !strings.HasPrefix(output, nextHeader(label)) {
		t.Fatalf("the step block is not the reference's:\n%q", output)
	}

	if !strings.HasSuffix(output, nextTail) {
		t.Fatalf("the closing block is not the reference's:\n%q", output)
	}

	record, err := store.NewFile(rig.alice.state).Load(context.Background(), swapID)

	if err != nil {
		t.Fatal(err)
	}

	// Bob's settlement keys are now pinned locally, which is the whole content of the
	// import. They are Bob's; Alice's own keys were already there.
	if record.BlakeBobPub == nil || record.BTCBobPub == nil {
		t.Fatalf("Bob's settlement keys were not recorded: %+v", record)
	}

	if *record.NostrBobPub != rig.bob.key.PublicKey() {
		t.Fatal("the imported acceptance is bound to another identity")
	}
}

// TestNextReportsReadyToFund stops at the economic boundary.
func TestNextReportsReadyToFund(t *testing.T) {
	rig := newNextRig(t)

	offerID := rig.publishOffer(rig.alice, "10", "50000", "30")
	takeID := rig.takeOffer(rig.bob, offerID)
	swapID, _ := rig.acceptTake(rig.alice, takeID)

	if _, err := rig.next(rig.bob); err != nil {
		t.Fatal(err)
	}

	if _, err := rig.next(rig.alice); err != nil {
		t.Fatal(err)
	}

	before := rig.relayEventIDs()

	output, err := rig.next(rig.alice)

	if err != nil {
		t.Fatalf("next: %v\n%s", err, output)
	}

	want := "\nREADY TO FUND\n" +
		"=============\n" +
		"swap:  " + swapID + "\n" +
		"trade: 10.0 BLAKE / 50,000 sats\n" +
		"\n" +
		"All authenticated negotiation and settlement keys are in place.\n" +
		"\n" +
		"STOP: funding is an explicit economic commitment.\n" +
		"Run:\n" +
		"  scripts/blake fund\n" +
		"\n" +
		"PASS: next did not move funds\n"

	assertOutputIs(t, output, want)

	// Nothing executed: no new event, and the record is still unfunded.
	if after := rig.relayEventIDs(); len(after) != len(before) {
		t.Fatalf("next published %d event(s) while reporting readiness",
			len(after)-len(before))
	}

	record, err := store.NewFile(rig.alice.state).Load(context.Background(), swapID)

	if err != nil {
		t.Fatal(err)
	}

	if record.BlakeFundingTxID != nil || record.BTCFundingTxID != nil {
		t.Fatal("next funded a swap it was only supposed to report on")
	}
}

// TestNextSaysNothingToDo is the empty state: no records, no offers, no decisions.
func TestNextSaysNothingToDo(t *testing.T) {
	rig := newNextRig(t)

	output, err := rig.next(rig.bob)

	if err != nil {
		t.Fatalf("next: %v\n%s", err, output)
	}

	assertOutputIs(t, output, nextNothingToDo)
}

// TestNextRefusesToChooseBetweenTwoActions is the refusal that gives the command its
// value: two live negotiations, both listed, neither performed.
func TestNextRefusesToChooseBetweenTwoActions(t *testing.T) {
	rig := newNextRig(t)

	first := rig.publishOffer(rig.alice, "10", "50000", "30")
	second := rig.publishOffer(rig.alice, "11", "60000", "31")

	before := rig.relayEventIDs()

	output, err := rig.next(rig.bob)

	if err != nil {
		t.Fatalf("next: %v\n%s", err, output)
	}

	head := "\nMULTIPLE SAFE COORDINATION STEPS\n" +
		"================================\n" +
		"More than one authenticated negotiation needs attention. Refusing to choose automatically.\n" +
		"\n"

	tail := "\nResolve the intended trade explicitly:\n" +
		"  take <offer-prefix>\n" +
		"  accept-take <offer-prefix>\n" +
		"  accept-match <match-prefix>\n" +
		"  engine accept-reverse-swap-offer <match-id>\n" +
		"\n" +
		"PASS: no funds moved and no ambiguous choice was made\n"

	if !strings.HasPrefix(output, head) {
		t.Fatalf("the banner is not the reference's:\n%q", output)
	}

	if !strings.HasSuffix(output, tail) {
		t.Fatalf("the closing block is not the reference's:\n%q", output)
	}

	// The two candidates are listed newest first, and two offers published in the same
	// second are ordered by event id, which a test cannot predict. Both orders are the
	// reference's; a missing or altered line is not.
	firstLine := "Take open offer " + first[:12] + " for 10.0 BLAKE / 50,000 sats\n"
	secondLine := "Take open offer " + second[:12] + " for 11.0 BLAKE / 60,000 sats\n"

	middle := strings.TrimSuffix(strings.TrimPrefix(output, head), tail)

	if middle != "1. "+secondLine+"2. "+firstLine &&
		middle != "1. "+firstLine+"2. "+secondLine {
		t.Fatalf("the numbered list is not the reference's:\n%q", middle)
	}

	// Nothing was performed: the relay is unchanged and no record was created.
	after := rig.relayEventIDs()

	if len(after) != len(before) {
		t.Fatalf("next published %d event(s) while refusing to choose", len(after)-len(before))
	}

	if records := localRecords(t, rig.bob); len(records) != 0 {
		t.Fatalf("next created local state while refusing to choose: %v", records)
	}
}

// TestNextDoesNotRepeatATakeItAlreadyPublished is the duplicate-take refusal: the prior take
// is on the relay, so the offer is not surfaced as work again.
func TestNextDoesNotRepeatATakeItAlreadyPublished(t *testing.T) {
	rig := newNextRig(t)

	offerID := rig.publishOffer(rig.alice, "10", "50000", "30")

	if _, err := rig.next(rig.bob); err != nil {
		t.Fatal(err)
	}

	before := rig.relayEventIDs()

	output, err := rig.next(rig.bob)

	if err != nil {
		t.Fatalf("next: %v\n%s", err, output)
	}

	assertOutputIs(t, output, nextNothingToDo)

	if after := rig.relayEventIDs(); len(after) != len(before) {
		t.Fatalf("next published a duplicate take for %s", offerID[:12])
	}
}

// TestNextDoesNotResurfaceADurablySelectedOffer: a selection record means this offer
// already produced a swap, so its take is not offered again.
func TestNextDoesNotResurfaceADurablySelectedOffer(t *testing.T) {
	rig := newNextRig(t)

	offerID := rig.publishOffer(rig.alice, "10", "50000", "30")
	takeID := rig.takeOffer(rig.bob, offerID)

	// The durable record an accepted take leaves behind, written without accepting it: the
	// suppression must come from the record, not from the swap it would have created.
	selection := filepath.Join(rig.alice.state, selectionsDirectory, "offer-"+offerID+".json")

	if err := writeSelection(selection, map[string]any{
		"version":  1,
		"offer_id": offerID,
		"take_id":  takeID,
		"swap_id":  "aaaa00000001",
	}); err != nil {
		t.Fatal(err)
	}

	before := rig.relayEventIDs()

	output, err := rig.next(rig.alice)

	if err != nil {
		t.Fatalf("next: %v\n%s", err, output)
	}

	assertOutputIs(t, output, nextNothingToDo)

	if after := rig.relayEventIDs(); len(after) != len(before) {
		t.Fatal("next accepted a take for an offer that already has a durable selection")
	}

	if records := localRecords(t, rig.alice); len(records) != 0 {
		t.Fatalf("next created local state for a durably selected offer: %v", records)
	}
}

// TestNextRefusesAFreshnessWindowBelowTheMinimum pins the reference's refusal, which is a
// refusal rather than a clamp: a window that would reject live work is a misconfiguration.
func TestNextRefusesAFreshnessWindowBelowTheMinimum(t *testing.T) {
	for _, value := range []string{"0", "1", "59"} {
		t.Run(value, func(t *testing.T) {
			rig := newNextRig(t)

			t.Setenv(nextFreshEnv, value)

			output, err := rig.next(rig.bob)

			if err == nil {
				t.Fatalf("a %s-second window was accepted:\n%s", value, output)
			}

			if err.Error() != "BLAKE_SWAP_NEXT_FRESH_SECONDS must be at least 60" {
				t.Fatalf("refusal = %q, want the reference's message", err.Error())
			}

			if output != "" {
				t.Fatalf("a refused window still printed:\n%q", output)
			}
		})
	}
}

// TestNextReportsAnOldMatchAsHistorical: a validated match outside the automatic window is
// reported and not imported.
func TestNextReportsAnOldMatchAsHistorical(t *testing.T) {
	rig := newNextRig(t)

	// The window is a minute; the match below is nearly two hours old.
	t.Setenv(nextFreshEnv, "60")

	now := time.Now().Unix()
	swapID := "abcdef012345"

	offerEvent, offer, err := marketflow.BuildOffer(rig.alice.key, marketflow.BuildOfferOptions{
		BlakeAmount: blakeUnits(t, "10"),
		BTCSats:     50_000,
		Minutes:     480,
		Direction:   market.BlakeForBTC,
		CreatedAt:   now - 7200,
	})

	if err != nil {
		t.Fatal(err)
	}

	listed, err := marketflow.ParseOffer(offerEvent, time.Now())

	if err != nil {
		t.Fatal(err)
	}

	rig.stub.Inject(offerEvent)

	takeEvent, take, err := marketflow.BuildTake(rig.bob.key, marketflow.BuildTakeOptions{
		Offer:     *listed,
		CreatedAt: now - 7100,
	})

	if err != nil {
		t.Fatal(err)
	}

	// The taker's own take is published through its own client so it lands in the taker's
	// verified cache. That is what the real take command does, and it is what keeps the
	// still-open offer from being surfaced as a second take.
	rig.bob.activate(t)

	if _, err := marketClient().Publish(context.Background(), takeEvent); err != nil {
		t.Fatal(err)
	}

	matchEvent, _, err := marketflow.BuildMatch(rig.alice.key, marketflow.BuildMatchOptions{
		Offer:       *listed,
		OfferEvent:  offerEvent,
		TakeEvent:   takeEvent,
		Taker:       &take,
		SwapID:      swapID,
		BlakeAmount: offer.BlakeAmount,
		BTCSats:     offer.BTCSats,
		CreatedAt:   now - 7000,
	})

	if err != nil {
		t.Fatal(err)
	}

	rig.stub.Inject(matchEvent)

	before := rig.relayEventIDs()

	output, err := rig.next(rig.bob)

	if err != nil {
		t.Fatalf("next: %v\n%s", err, output)
	}

	want := "\nNOTHING TO DO\n" +
		"=============\n" +
		"No unambiguous current coordination, funding-ready trade, or resumable settlement was found.\n" +
		"\n" +
		"HISTORICAL MATCHES IGNORED\n" +
		"==========================\n" +
		"1 valid unimported match(es) were outside the automatic 1-minute window.\n" +
		"They remain available explicitly with `scripts/blake accept-match <prefix>`.\n" +
		"\n" +
		"Use `scripts/blake market` to browse offers or `scripts/blake swaps` for the dashboard.\n"

	assertOutputIs(t, output, want)

	if after := rig.relayEventIDs(); len(after) != len(before) {
		t.Fatal("next performed a step on a historical match")
	}

	if records := localRecords(t, rig.bob); len(records) != 0 {
		t.Fatalf("next imported a historical match: %v", records)
	}
}
