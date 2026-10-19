// Command blake-swap-cli is the Go engine's command-line surface.
//
// It is a drop-in replacement for the Python engine's CLI: the same command names,
// the same positional arguments, and the same environment configuration, so the
// existing suites and a Python participant on the other side of a swap do not know
// which implementation they are talking to.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/coord"
	"github.com/gjbrr/blake-swap/pkg/fund"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/reconcile"
	"github.com/gjbrr/blake-swap/pkg/state"
	"github.com/gjbrr/blake-swap/pkg/store"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	command := os.Args[1]

	// --help and -h answer before anything is built: a usage request must not require a
	// participant key, a datadir or a relay, which is exactly the situation somebody
	// typing it is in.
	if command == "-h" || command == "--help" || command == "help" {
		usage()

		return
	}

	args := os.Args[2:]

	// A market read talks to relays, not to nodes, so it runs before the engine is
	// built: looking for an offer must not require a regtest datadir and two nodes.
	if isMarketCommand(command) {
		if err := run(context.Background(), nil, command, args); err != nil {
			fail(err)
		}

		return
	}

	// These commands sign or read, but they touch no chain. Building an engine here
	// would demand a regtest datadir and two nodes for a command the reference runs on
	// a laptop with neither, so only the identity is loaded.
	if isIdentityOnlyCommand(command) {
		identity, err := loadIdentity(stateDirectory())

		if err != nil {
			fail(err)
		}

		if err := run(context.Background(), &swap.Engine{Identity: identity}, command, args); err != nil {
			fail(err)
		}

		return
	}

	engine, cleanup, err := buildEngine()

	if err != nil {
		fail(err)
	}

	defer cleanup()

	ctx := context.Background()

	if err := run(ctx, engine, command, args); err != nil {
		fail(err)
	}
}

// exitError is an error that carries the process exit code it wants.
//
// A supervisor reads exit codes, so the two the reference distinguishes — a security
// failure and an exhausted failure budget — are reported as what they are rather than
// flattened into a generic failure.
type exitError struct {
	code    int
	message string
}

func (e exitError) Error() string {
	if e.message == "" {
		return fmt.Sprintf("exit %d", e.code)
	}

	return e.message
}

// fail prints an error the way an operator (or a test) expects to read it.
func fail(err error) {
	var exit exitError

	if errors.As(err, &exit) {
		if exit.message != "" {
			fmt.Fprintln(os.Stderr, "ERROR:", exit.message)
		}

		os.Exit(exit.code)
	}

	fmt.Fprintln(os.Stderr, "ERROR:", err)
	os.Exit(1)
}

// usage lists the implemented commands.
func usage() {
	fmt.Fprint(os.Stderr, `usage: blake-swap-cli <command> [arguments]

commands:
  new --blake AMOUNT --sats SATS [--blake-timeout H --btc-timeout H]
  show SWAP_ID | list | status
  advance SWAP_ID STATE --role ROLE     (BLAKE_SWAP_TEST_MODE=1 only)
  accept-take OFFER_ID
  accept-match [MATCH_ID_OR_PREFIX]
  accept-market-take TAKE_ID
  accept-market-match MATCH_ID
  accept-reverse-market-match MATCH_ID
  accept-reverse-swap-offer MATCH_ID
  publish-offer SWAP_ID
  accept-offer SWAP_ID [--expected-alice-pub HEX] [--allow-unpinned-author]
  receive-accept SWAP_ID
  fund
  fund-blake SWAP_ID | fund-btc SWAP_ID
  verify-blake SWAP_ID | verify-btc SWAP_ID
  publish-blake-funded SWAP_ID | receive-blake-funded SWAP_ID
  publish-btc-funded SWAP_ID | receive-btc-funded SWAP_ID
  claim-btc SWAP_ID | claim-blake SWAP_ID
  refund-btc SWAP_ID | refund-blake SWAP_ID
  extract-secret SWAP_ID
  publish-btc-claimed SWAP_ID | receive-btc-claimed SWAP_ID
  publish-blake-claimed SWAP_ID | receive-blake-claimed SWAP_ID
  publish-btc-refunded SWAP_ID | receive-btc-refunded SWAP_ID
  complete SWAP_ID
  reconcile SWAP_ID [--json]
  next SWAP_ID
  run SWAP_ID [--max-steps N] [--fee-rate SAT_PER_VB]
  agent SWAP_ID --role alice|bob [--interval S] [--max-ticks N]
  resume [SWAP_ID_PREFIX] [--max-steps N] [--fee-rate SAT_PER_VB]
  watch-htlc SWAP_ID
  recovery-plan SWAP_ID
  refresh-timeouts SWAP_ID [--clear-stale-intent]
  sync-funding SWAP_ID
  sync-settlement SWAP_ID
  dashboard [--json]
  resume-all

market (relays, no nodes):
  market [--limit N]
  market-state [--limit N] [--json]
  publish-market-offer --blake AMOUNT --sats SATS [--minutes N] [--direction D]
  cancel-market-offer OFFER_ID
  inspect-market-take OFFER_ID
  take-market-offer OFFER_ID
  market-takes OFFER_ID [--limit N] [--json]
  accept-take OFFER_ID
  accept-match [MATCH_ID_OR_PREFIX]
  accept-market-take TAKE_ID
  accept-market-match MATCH_ID
  accept-reverse-market-match MATCH_ID
  accept-reverse-swap-offer MATCH_ID
  publish-market-match TAKE_ID
  market-matches [--limit N]
  ignore-match MATCH_ID
  dismiss SWAP_ID
  rebroadcast-nostr-event EVENT_ID

environment:
  BLAKE_SWAP_STATE_DIR            records and the verified relay cache
  BLAKE_SWAP_PARTICIPANT_KEY_FILE the Nostr identity this participant signs with
  BLAKE_SWAP_NOSTR_RELAYS         comma-separated relays for market commands
  BLAKE_SWAP_BTC_DATADIR          Bitcoin node datadir (RPC cookie and port are read from it)
  BLAKE_SWAP_BLAKE_DATADIR        BLAKE node datadir
  BLAKE_SWAP_RPC_TIMEOUT_SECONDS  node RPC deadline (default 120)
  BLAKE_SWAP_{BTC,BLAKE}_SPEND_FEE_SATS / _RATE  settlement fee overrides
`)
}

// run dispatches one command.
func run(ctx context.Context, engine *swap.Engine, command string, args []string) error {
	switch command {
	case "market", "market-state":
		return runMarketCommand(ctx, command, args)
	case "publish-market-offer":
		return runPublishMarketOffer(ctx, engine, args)
	case "cancel-market-offer":
		return runCancelMarketOffer(ctx, engine, args)
	case "inspect-market-take":
		return runInspectMarketTake(ctx, engine, args)
	case "take-market-offer":
		return runTakeMarketOffer(ctx, engine, args)
	case "market-takes":
		return runMarketTakes(ctx, engine, args)
	case "rebroadcast-nostr-event":
		return runRebroadcastNostrEvent(ctx, args)
	case "market-matches":
		return runMarketMatches(ctx, engine, args)
	case "ignore-match":
		return runIgnoreMatch(ctx, engine, args)
	case "dismiss":
		return runDismiss(ctx, engine, args)
	case "publish-market-match":
		return runPublishMarketMatch(ctx, engine, args)
	case "accept-market-take":
		return runAcceptMarketTake(ctx, engine, args)
	case "accept-take":
		return runAcceptTake(ctx, engine, args)
	case "accept-market-match":
		return runAcceptMarketMatch(ctx, engine, args)
	case "accept-reverse-market-match":
		return runAcceptReverseMarketMatch(ctx, engine, args)
	case "accept-match":
		return runAcceptMatch(ctx, engine, args)
	case "accept-reverse-swap-offer":
		return runAcceptReverseSwapOffer(ctx, engine, args)
	case "new":
		return runNew(ctx, engine, args)
	case "show":
		return runShow(ctx, engine, args)
	case "list":
		return runList(ctx, engine)
	case "status":
		return runStatus(ctx, engine)
	case "advance":
		return runAdvance(ctx, engine, args)
	case "publish-offer":
		return runPublishOffer(ctx, engine, args)
	case "accept-offer":
		return runAcceptOffer(ctx, engine, args)
	case "receive-accept":
		return runReceiveAccept(ctx, engine, args)
	case "fund":
		return runFundReadySwap(ctx, engine, args)
	case "fund-blake":
		return runFund(ctx, engine, args, chain.Blake)
	case "fund-btc":
		return runFund(ctx, engine, args, chain.BTC)
	case "verify-blake":
		return runVerify(ctx, engine, args, chain.Blake)
	case "verify-btc":
		return runVerify(ctx, engine, args, chain.BTC)
	case "publish-blake-funded":
		return runAnnounce(ctx, engine, args, engine.AnnounceBlakeFunding, "blake-funding")
	case "receive-blake-funded":
		return runAnnounce(ctx, engine, args, engine.ReceiveBlakeFunding, "blake-funding")
	case "publish-btc-funded":
		return runAnnounce(ctx, engine, args, engine.AnnounceBTCFunding, "btc-funding")
	case "receive-btc-funded":
		return runAnnounce(ctx, engine, args, engine.ReceiveBTCFunding, "btc-funding")
	case "claim-btc":
		return runSettle(ctx, engine, args, engine.ClaimBTC, "btc-claim")
	case "claim-blake":
		return runSettle(ctx, engine, args, engine.ClaimBlake, "blake-claim")
	case "refund-btc":
		return runSettle(ctx, engine, args, engine.RefundBTC, "btc-refund")
	case "refund-blake":
		return runSettle(ctx, engine, args, engine.RefundBlake, "blake-refund")
	case "extract-secret":
		return runExtractSecret(ctx, engine, args)
	case "publish-btc-claimed":
		return runSettlement(ctx, engine, args, engine.AnnounceBTCCLaimed, "btc-claimed")
	case "receive-btc-claimed":
		return runSettlement(ctx, engine, args, engine.ReceiveBTCCLaimed, "btc-claimed")
	case "publish-blake-claimed":
		return runSettlement(ctx, engine, args, engine.AnnounceBlakeClaimed, "blake-claimed")
	case "receive-blake-claimed":
		return runSettlement(ctx, engine, args, engine.ReceiveBlakeClaimed, "blake-claimed")
	case "publish-btc-refunded":
		return runSettlement(ctx, engine, args, engine.AnnounceBTCRefunded, "btc-refunded")
	case "receive-btc-refunded":
		return runSettlement(ctx, engine, args, engine.ReceiveBTCRefunded, "btc-refunded")
	case "complete":
		return runComplete(ctx, engine, args)
	case "reconcile":
		return runReconcile(ctx, engine, args)
	case "next":
		return runNext(ctx, engine, args)
	case "run":
		return runDriver(ctx, engine, args)
	case "agent":
		return runAgent(ctx, engine, args)
	case "resume":
		return runResume(ctx, engine, args)
	case "watch-htlc":
		return runWatchHTLC(ctx, engine, args)
	case "recovery-plan":
		return runRecoveryPlan(ctx, engine, args)
	case "refresh-timeouts":
		return runRefreshTimeouts(ctx, engine, args)
	case "sync-funding":
		return runSync(ctx, engine, args, engine.SyncFunding, "FUNDING")
	case "sync-settlement":
		return runSync(ctx, engine, args, engine.SyncSettlement, "SETTLEMENT")
	case "dashboard":
		return runDashboard(ctx, engine, args)
	case "resume-all":
		return runResumeAll(ctx, engine)
	}

	return fmt.Errorf("unknown command: %s", command)
}

// ---------------------------------------------------------------- commands

func runNew(ctx context.Context, engine *swap.Engine, args []string) error {
	flags := flag.NewFlagSet("new", flag.ContinueOnError)

	blakeAmount := flags.String("blake", "", "BLAKE amount")
	sats := flags.Int64("sats", 0, "Bitcoin sats")
	blakeTimeout := flags.Int64("blake-timeout", 0, "explicit BLAKE timeout height")
	btcTimeout := flags.Int64("btc-timeout", 0, "explicit Bitcoin timeout height")
	swapID := flags.String("swap-id", "", "explicit swap id")

	if err := flags.Parse(args); err != nil {
		return err
	}

	if *blakeAmount == "" || *sats == 0 {
		return errors.New("new requires --blake and --sats")
	}

	parsed, err := amount.ParseBlake(*blakeAmount)

	if err != nil {
		return err
	}

	result, err := engine.New(ctx, swap.NewRequest{
		BlakeAmount:  parsed,
		BTCSats:      *sats,
		SwapID:       *swapID,
		BlakeTimeout: *blakeTimeout,
		BTCTimeout:   *btcTimeout,
		Random:       randReader(),
	})

	if err != nil {
		return err
	}

	// The identifier is the one thing a script needs from this command.
	fmt.Println("swap id:", result.Swap.SwapID)
	fmt.Println("state:  ", result.Swap.State)
	fmt.Println("blake:  ", amount.Blake(result.Swap.BlakeAmountBaseUnits).Format())
	fmt.Println("sats:   ", result.Swap.BTCSats)
	fmt.Println("btc timeout:  ", result.Swap.BTCTimeout)
	fmt.Println("blake timeout:", result.Swap.BlakeTimeout)

	for _, archived := range result.Archived {
		fmt.Println("archived:", archived)
	}

	return nil
}

func runShow(ctx context.Context, engine *swap.Engine, args []string) error {
	if len(args) < 1 {
		return errors.New("show requires a swap id")
	}

	record, err := engine.Show(ctx, args[0])

	if err != nil {
		return err
	}

	printRecord(record)

	return nil
}

func runList(ctx context.Context, engine *swap.Engine) error {
	records, err := engine.List(ctx)

	if err != nil {
		return err
	}

	if len(records) == 0 {
		fmt.Println("No swaps.")

		return nil
	}

	fmt.Print(renderSwapList(records))

	return nil
}

// renderSwapList is the reference's list output.
//
// It prints one section per protocol role over the *same* directory, because a
// participant's state tree is already participant-local: protocol Alice's and protocol
// Bob's records of one participant live side by side, and the reference lists them twice
// under both headings. That reads oddly, and it is what a drop-in replacement has to
// print, because the two engines' outputs are compared by tests and by operators.
func renderSwapList(records []*model.Swap) string {
	var out strings.Builder

	if len(records) == 0 {
		return "No swaps.\n"
	}

	for _, role := range []string{"alice", "bob"} {
		out.WriteString("\n")
		out.WriteString(strings.ToUpper(role))
		out.WriteString("\n")
		out.WriteString(strings.Repeat("=", len(role)))
		out.WriteString("\n")

		for _, record := range records {
			fmt.Fprintf(&out, "%s %s\n", record.SwapID, record.State)
		}
	}

	return out.String()
}

func runStatus(ctx context.Context, engine *swap.Engine) error {
	status, err := engine.Status(ctx)

	if err != nil {
		return err
	}

	fmt.Println("CHAIN STATUS")
	fmt.Println("============")
	fmt.Println("BLAKE height:  ", status.Blake)
	fmt.Println("Bitcoin height:", status.BTC)

	return nil
}

func runAdvance(ctx context.Context, engine *swap.Engine, args []string) error {
	flags := flag.NewFlagSet("advance", flag.ContinueOnError)

	role := flags.String("role", "alice", "protocol role")

	positional, err := parsePositional(flags, args, 2)

	if err != nil {
		return err
	}

	if len(positional) != 2 {
		return errors.New("advance requires a swap id and a target state")
	}

	from, to, err := engine.Advance(ctx, positional[0], *role, positional[1])

	if err != nil {
		return err
	}

	fmt.Printf("%s -> %s\n", from, to)

	return nil
}

func runPublishOffer(ctx context.Context, engine *swap.Engine, args []string) error {
	if len(args) < 1 {
		return errors.New("publish-offer requires a swap id")
	}

	result, err := engine.PublishOffer(ctx, args[0])

	if err != nil {
		return err
	}

	fmt.Print(renderOfferPublished(args[0], result.Location))

	return nil
}

// renderOfferPublished is the reference's publish-offer confirmation.
//
// The last line is the one that matters: the offer carries public terms and the
// contract keys' public halves, and publishing it must not carry the secret.
func renderOfferPublished(swapID, location string) string {
	return "\nOFFER PUBLISHED\n" +
		"===============\n" +
		"swap id: " + swapID + "\n" +
		"message: " + location + "\n" +
		"\n" +
		"PASS: secret_hex was NOT published\n"
}

func runAcceptOffer(ctx context.Context, engine *swap.Engine, args []string) error {
	flags := flag.NewFlagSet("accept-offer", flag.ContinueOnError)

	expectedAlice := flags.String("expected-alice-pub", "", "pinned maker identity")
	allowUnpinned := flags.Bool("allow-unpinned-author", false, "accept any author")
	expectedBlake := flags.String("expected-blake-amount", "", "pinned BLAKE amount")
	expectedSats := flags.Int64("expected-btc-sats", 0, "pinned Bitcoin sats")

	positional, err := parsePositional(flags, args, 1)

	if err != nil {
		return err
	}

	if len(positional) != 1 {
		return errors.New("accept-offer requires a swap id")
	}

	request := swap.AcceptRequest{
		SwapID:              positional[0],
		ExpectedAlicePub:    *expectedAlice,
		AllowUnpinnedAuthor: *allowUnpinned,
	}

	if *expectedBlake != "" {
		parsed, err := amount.ParseBlake(*expectedBlake)

		if err != nil {
			return err
		}

		request.ExpectedBlakeAmount = &parsed
	}

	if *expectedSats != 0 {
		request.ExpectedBTCSats = expectedSats
	}

	result, err := engine.AcceptOffer(ctx, request)

	if err != nil {
		return err
	}

	fmt.Printf("accept message: %s\n", result.Location)
	fmt.Print(renderOfferAccepted(
		result.Swap.SwapID, recordPath(result.Swap.SwapID), result.Timeout,
	))

	return nil
}

// renderOfferAccepted is the reference's accept-offer confirmation.
//
// The timeout lines are the part an operator is meant to read: they say how much of
// Alice's Bitcoin window is left after Bob's own measurements, and how much recovery room
// that leaves him. They are hours to two decimals because that is how a person judges
// whether a trade is safe to fund.
func renderOfferAccepted(swapID, bobStatePath string, check *swap.TimeoutCheck) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("OFFER ACCEPTED\n")
	out.WriteString("==============\n")
	fmt.Fprintf(&out, "swap id: %s\n", swapID)
	fmt.Fprintf(&out, "Bob state: %s\n", bobStatePath)
	out.WriteString("\n")
	out.WriteString("PASS: Bob has hash H\n")
	out.WriteString("PASS: Bob does NOT have secret S\n")

	if check != nil {
		out.WriteString("PASS: Bob independently validated v1.5 timeout safety\n")
		fmt.Fprintf(&out, "BTC remaining safety: %s h\n", hoursOf(check.BTCRemainingSeconds))
		fmt.Fprintf(&out, "cross-chain recovery gap: %s h\n", hoursOf(check.ActualRecoveryGap))
		fmt.Fprintf(&out, "Bob required recovery: %s h\n", hoursOf(check.RequiredRecovery))
	}

	return out.String()
}

// hoursOf renders seconds as hours to two decimals.
func hoursOf(seconds int64) string {
	return fmt.Sprintf("%.2f", float64(seconds)/3600)
}

// recordPath is where one swap record lives in this participant's state tree.
func recordPath(swapID string) string {
	return filepath.Join(stateDirectory(), swapID+".json")
}

func runReceiveAccept(ctx context.Context, engine *swap.Engine, args []string) error {
	if len(args) < 1 {
		return errors.New("receive-accept requires a swap id")
	}

	if _, err := engine.ReceiveAccept(ctx, args[0]); err != nil {
		return err
	}

	record, err := engine.Show(ctx, args[0])

	if err != nil {
		return err
	}

	fmt.Print(renderAcceptReceived(args[0], record))

	return nil
}

// renderAcceptReceived is the reference's accept-received confirmation.
//
// The last line is the whole point of the message: Alice learns Bob's two public
// contract keys and nothing else, so she can fund knowing he cannot claim without
// revealing his secret — because he does not have one.
func renderAcceptReceived(swapID string, record *model.Swap) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("ACCEPT RECEIVED\n")
	out.WriteString("===============\n")
	fmt.Fprintf(&out, "swap id: %s\n", swapID)
	out.WriteString("\n")

	if record != nil && record.BlakeBobPub != nil {
		fmt.Fprintf(&out, "Bob BLAKE pubkey: %s\n", *record.BlakeBobPub)
	}

	if record != nil && record.BTCBobPub != nil {
		fmt.Fprintf(&out, "Bob BTC pubkey:   %s\n", *record.BTCBobPub)
	}

	out.WriteString("\n")
	out.WriteString("PASS: Alice received only Bob public keys\n")

	return out.String()
}

// runFundReadySwap funds the only swap whose Bob acceptance is ready.
//
// Funding is the first command in this protocol that moves money, so it never chooses.
// With no candidate it says so; with more than one it lists them and stops. The single
// candidate case still goes through the ordinary fund-blake path, which keeps every
// validation, lock and crash-recovery rule that path already has.
func runFundReadySwap(ctx context.Context, engine *swap.Engine, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("fund takes no arguments")
	}

	key, err := engineKey(engine)

	if err != nil {
		return err
	}

	me := key.PublicKey()

	records, err := engine.List(ctx)

	if err != nil {
		return err
	}

	candidates := fundingCandidates(ctx, engine, records, me)

	if len(candidates) == 0 {
		fmt.Print(renderNoFundingReady())

		return nil
	}

	if len(candidates) > 1 {
		fmt.Print(renderMultipleFundingReady(candidates))

		return nil
	}

	candidate := candidates[0]

	minimum, err := engine.BTCTradeMinimumSats(ctx)

	if err != nil {
		return err
	}

	if candidate.record.BTCSats < minimum {
		return fmt.Errorf(
			"swap %s BTC amount %d sats is too small; minimum settlement-feasible "+
				"amount is %d sats under the current settlement fee policy",
			candidate.record.SwapID, candidate.record.BTCSats, minimum,
		)
	}

	fmt.Print(renderFundingReady(candidate.record))

	if !candidate.imported {
		fmt.Print("Importing Bob's authenticated acceptance...\n")

		if err := runReceiveAccept(ctx, engine, []string{candidate.record.SwapID}); err != nil {
			return err
		}
	}

	updated, err := engine.Show(ctx, candidate.record.SwapID)

	if err != nil {
		return err
	}

	if updated.BlakeBobPub == nil || updated.BTCBobPub == nil || updated.NostrBobPub == nil {
		return fmt.Errorf("SECURITY FAILURE: Bob acceptance is not fully pinned")
	}

	fmt.Print(renderExplicitCommitment())

	return runFund(ctx, engine, []string{candidate.record.SwapID}, chain.Blake)
}

// fundingCandidate is one swap whose Bob acceptance is available.
type fundingCandidate struct {
	record   *model.Swap
	imported bool
}

// fundingCandidates finds the swaps this participant could fund right now.
//
// A swap is a candidate when it is Alice's, still OFFERED, has a pinned Bob identity, and
// either already holds Bob's settlement keys or has an authenticated accept message
// waiting. Discovery is read-only: the message is not consumed here.
func fundingCandidates(
	ctx context.Context, engine *swap.Engine, records []*model.Swap, me string,
) []fundingCandidate {
	candidates := make([]fundingCandidate, 0, len(records))

	for _, record := range records {
		if string(record.State) != "OFFERED" {
			continue
		}

		if record.NostrAlicePub == nil || *record.NostrAlicePub != me {
			continue
		}

		if record.NostrBobPub == nil || *record.NostrBobPub == "" {
			continue
		}

		if record.BlakeBobPub != nil && record.BTCBobPub != nil {
			candidates = append(candidates, fundingCandidate{record: record, imported: true})

			continue
		}

		if acceptAvailable(ctx, engine, record) {
			candidates = append(candidates, fundingCandidate{record: record})
		}
	}

	return candidates
}

// acceptAvailable reports whether Bob's authenticated acceptance is readable.
func acceptAvailable(ctx context.Context, engine *swap.Engine, record *model.Swap) bool {
	if engine.Transport == nil || record.NostrBobPub == nil {
		return false
	}

	message, err := engine.Transport.Receive(ctx, record.SwapID, "accept", coord.ReceiveOptions{
		ExpectedPubkey: *record.NostrBobPub,
		Mode:           engine.Mode,
	})

	if err != nil || message == nil {
		return false
	}

	if message.Body["type"] != "accept" {
		return false
	}

	if message.Body["swap_id"] != record.SwapID {
		return false
	}

	if message.Body["secret_hash"] != record.SecretHash {
		return false
	}

	for _, field := range []string{"blake_bob_pub", "btc_bob_pub"} {
		value, _ := message.Body[field].(string)

		if value == "" {
			return false
		}
	}

	return true
}

// renderNoFundingReady is the empty-candidate block.
func renderNoFundingReady() string {
	return "\nNO FUNDING-READY SWAP\n" +
		"=====================\n" +
		"No local protocol-Alice OFFERED swap has an authenticated Bob acceptance ready.\n" +
		"\n" +
		"The taker may still need to run `accept-match`.\n"
}

// renderMultipleFundingReady refuses to choose which trade receives money.
func renderMultipleFundingReady(candidates []fundingCandidate) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("MULTIPLE FUNDING-READY SWAPS\n")
	out.WriteString("============================\n")
	out.WriteString("Refusing to choose which trade receives funds.\n")
	out.WriteString("\n")

	for index, candidate := range candidates {
		fmt.Fprintf(&out, "%d. %s  %s BLAKE / %s sats\n",
			index+1,
			candidate.record.SwapID,
			pythonFloat(blakeFloat(candidate.record.BlakeAmount())),
			thousands(candidate.record.BTCSats),
		)
	}

	out.WriteString("\n")
	out.WriteString("Use `fund-blake <swap-id>` for the one you intend to commit.\n")

	return out.String()
}

// renderFundingReady names the trade about to be funded.
func renderFundingReady(record *model.Swap) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("FUNDING-READY SWAP\n")
	out.WriteString("==================\n")
	fmt.Fprintf(&out, "swap:  %s\n", record.SwapID)
	fmt.Fprintf(&out, "BLAKE: %s\n", pythonFloat(blakeFloat(record.BlakeAmount())))
	fmt.Fprintf(&out, "sats:  %s\n", thousands(record.BTCSats))
	out.WriteString("\n")

	return out.String()
}

// renderExplicitCommitment says, in as many words, that money is about to move.
func renderExplicitCommitment() string {
	return "\nEXPLICIT COMMITMENT\n" +
		"===================\n" +
		"Funding the BLAKE HTLC now. This moves funds.\n" +
		"\n"
}

func runFund(ctx context.Context, engine *swap.Engine, args []string, ch chain.Chain) error {
	if len(args) < 1 {
		return fmt.Errorf("fund-%s requires a swap id", chainSuffix(ch))
	}

	var (
		result *swap.FundingResult
		err    error
	)

	if ch == chain.Blake {
		result, err = engine.FundBlake(ctx, args[0])
	} else {
		result, err = engine.FundBTC(ctx, args[0])
	}

	if err != nil {
		return err
	}

	if result.Pending {
		fmt.Println("FUNDING BROADCAST")
		fmt.Println("=================")
		fmt.Println("txid: ", result.TxID)
		fmt.Println("vout: ", result.Vout)
		fmt.Println("block: pending")
		fmt.Println("state:", result.State)

		return nil
	}

	fmt.Println("FUNDED")
	fmt.Println("------")
	fmt.Println("txid: ", result.TxID)
	fmt.Println("vout: ", result.Vout)
	fmt.Println("block:", result.BlockHash)
	fmt.Println("state:", result.State)

	if !result.Watch.Imported && result.Watch.Reason != "" {
		fmt.Println("WARNING: the HTLC was not recorded in a watch-only wallet:",
			result.Watch.Reason)
	}

	return nil
}

func runVerify(ctx context.Context, engine *swap.Engine, args []string, ch chain.Chain) error {
	if len(args) < 1 {
		return fmt.Errorf("verify-%s requires a swap id", chainSuffix(ch))
	}

	var err error

	if ch == chain.Blake {
		err = engine.VerifyBlake(ctx, args[0])
	} else {
		err = engine.VerifyBTC(ctx, args[0])
	}

	if err != nil {
		return err
	}

	fmt.Println("PASS:", chainLabel(ch), "funding verified")

	return nil
}

func runAnnounce(
	ctx context.Context,
	engine *swap.Engine,
	args []string,
	call func(context.Context, string) (*swap.MessageResult, error),
	label string,
) error {
	if len(args) < 1 {
		return fmt.Errorf("%s requires a swap id", label)
	}

	result, err := call(ctx, args[0])

	if err != nil {
		return err
	}

	fmt.Println("MESSAGE", strings.ToUpper(label))
	fmt.Println("swap id: ", args[0])
	fmt.Println("message: ", result.Location)

	if result.Duplicate {
		fmt.Println("PASS: duplicate announcement is a safe no-op")
	}

	fmt.Println("state:   ", result.State)

	return nil
}

func runSettle(
	ctx context.Context,
	engine *swap.Engine,
	args []string,
	call func(context.Context, string) (*swap.SettlementResult, error),
	label string,
) error {
	flags := flag.NewFlagSet(label, flag.ContinueOnError)

	feeRate := flags.String("fee-rate", "", "settlement feerate in sat/vB")

	positional, err := parsePositional(flags, args, 1)

	if err != nil {
		return err
	}

	if len(positional) < 1 {
		return fmt.Errorf("%s requires a swap id", label)
	}

	if err := pinFeeRate(engine, *feeRate); err != nil {
		return err
	}

	result, err := call(ctx, positional[0])

	if err != nil {
		return err
	}

	if result.Pending {
		fmt.Println("SETTLEMENT BROADCAST")
		fmt.Println("====================")
		fmt.Println("txid: ", result.TxID)
		fmt.Println("block: pending")
		fmt.Println("state:", result.State)

		return nil
	}

	fmt.Println(strings.ToUpper(label))
	fmt.Println("================")
	fmt.Println("txid: ", result.TxID)
	fmt.Println("block:", result.BlockHash)
	fmt.Println("state:", result.State)

	if result.FeeSats > 0 {
		fmt.Println("fee:  ", result.FeeSats, "sats for", result.Vsize, "vB")
	}

	return nil
}

func runSettlement(
	ctx context.Context,
	engine *swap.Engine,
	args []string,
	call func(context.Context, string) (*swap.Announcement, error),
	label string,
) error {
	if len(args) < 1 {
		return fmt.Errorf("%s requires a swap id", label)
	}

	result, err := call(ctx, args[0])

	if err != nil {
		return err
	}

	fmt.Println(strings.ToUpper(label))
	fmt.Println("================")
	fmt.Println("swap id:", args[0])
	fmt.Println("txid:   ", result.TxID)
	fmt.Println("block:  ", result.BlockHash)
	fmt.Println("message:", result.Location)

	if result.Duplicate {
		fmt.Println("PASS: duplicate authenticated settlement was re-verified")
	}

	fmt.Println("state:  ", result.State)

	return nil
}

func runExtractSecret(ctx context.Context, engine *swap.Engine, args []string) error {
	if len(args) < 1 {
		return errors.New("extract-secret requires a swap id")
	}

	secret, err := engine.ExtractSecret(ctx, args[0])

	if err != nil {
		return err
	}

	fmt.Println("SECRET EXTRACTED")
	fmt.Println("================")
	fmt.Println("swap id:", args[0])
	fmt.Println("secret sha256 matches the swap hash")
	fmt.Println("secret length:", len(secret)/2, "bytes")

	return nil
}

func runComplete(ctx context.Context, engine *swap.Engine, args []string) error {
	flags := flag.NewFlagSet("complete", flag.ContinueOnError)

	role := flags.String("role", "alice", "protocol role")

	positional, err := parsePositional(flags, args, 1)

	if err != nil {
		return err
	}

	if len(positional) != 1 {
		return errors.New("complete requires a swap id")
	}

	previous := engine.Role
	engine.Role = *role

	defer func() { engine.Role = previous }()

	if err := engine.Complete(ctx, positional[0]); err != nil {
		return err
	}

	fmt.Println("SWAP COMPLETE")
	fmt.Println("=============")
	fmt.Println("swap id:", positional[0])
	fmt.Println("role:  ", *role)

	return nil
}

// runReconcile prints the protocol report, as JSON or as text.
func runReconcile(ctx context.Context, engine *swap.Engine, args []string) error {
	flags := flag.NewFlagSet("reconcile", flag.ContinueOnError)

	asJSON := flags.Bool("json", false, "machine-readable report")

	positional, err := parsePositional(flags, args, 1)

	if err != nil {
		return err
	}

	if len(positional) != 1 {
		return errors.New("reconcile requires a swap id")
	}

	report, err := engine.Reconcile(ctx, positional[0])

	if err != nil {
		return err
	}

	if *asJSON {
		encoded, err := json.MarshalIndent(reportJSON(report), "", "  ")

		if err != nil {
			return err
		}

		fmt.Println(string(encoded))

		return nil
	}

	printReconcile(report)

	return nil
}

// runNext prints the recommended action.
func runNext(ctx context.Context, engine *swap.Engine, args []string) error {
	if len(args) < 1 {
		return errors.New("next requires a swap id")
	}

	action, class, reason, err := engine.Next(ctx, args[0])

	if err != nil {
		return err
	}

	fmt.Println("swap id:", args[0])
	fmt.Println("next action:", action)
	fmt.Println("class:", class)
	fmt.Println("reason:", reason)

	return nil
}

// runResume finds the one swap with a live automatic path and hands it to the driver.
//
// It is a discovery command, not a settlement command: the reference scans this
// participant's records, reconciles each one, and refuses to guess when more than one swap
// could be resumed. What it hands the swap to is the autonomous driver, so `resume` and
// `run` cannot disagree about what to do next — the protocol has one decision table and this
// is not a second one.
func runResume(ctx context.Context, engine *swap.Engine, args []string) error {
	flags := flag.NewFlagSet("resume", flag.ContinueOnError)

	feeRate := flags.String("fee-rate", "", "settlement feerate in sat/vB")
	maxSteps := flags.Int("max-steps", 6, "maximum reconcile/action cycles for the driver")

	positional, err := parsePositional(flags, args, 1)

	if err != nil {
		return err
	}

	if *maxSteps < 1 || *maxSteps > 20 {
		return errors.New("--max-steps must be between 1 and 20")
	}

	if err := pinFeeRate(engine, *feeRate); err != nil {
		return err
	}

	key, err := engineKey(engine)

	if err != nil {
		return err
	}

	me := key.PublicKey()

	// The prefix is validated before the scan: a three-character prefix must be reported as
	// too short even when the state root is empty, which is the difference between a typo
	// and "no swap matches".
	explicit := ""

	if len(positional) > 0 {
		explicit, err = resumePrefix(positional[0])

		if err != nil {
			return err
		}
	}

	records, err := engine.List(ctx)

	if err != nil {
		return err
	}

	var resumable, attention []resumeItem

	for _, record := range records {
		if !isParty(record, me) {
			continue
		}

		if explicit != "" && !strings.HasPrefix(record.SwapID, explicit) {
			continue
		}

		report, err := engine.Reconcile(ctx, record.SwapID)

		if err != nil {
			attention = append(attention, resumeItem{
				record: record,
				reason: "reconciliation failed during discovery: " + err.Error(),
				failed: true,
			})

			continue
		}

		item := resumeItem{
			record: record,
			class:  report.DecisionClass,
			action: report.NextAction,
			reason: report.Reason,
		}

		if report.DecisionClass == reconcile.ClassManualAttention {
			attention = append(attention, item)

			continue
		}

		resumable = append(resumable, item)
	}

	if explicit != "" {
		return resumeOne(ctx, engine, explicit, resumable, attention, *maxSteps)
	}

	if len(resumable) == 0 {
		fmt.Print(renderNoResumable(attention))

		return nil
	}

	if len(resumable) > 1 {
		fmt.Print(renderMultipleResumable(resumable, attention))

		return nil
	}

	return handOverToDriver(ctx, engine, resumable[0], attention, *maxSteps)
}

// resumePrefix validates the optional swap-id prefix.
//
// The reference's minimum is four characters, and the check happens before the scan: a
// three-character prefix must be reported as too short even when the state root is empty,
// which is the difference between a typo and "no swap matches".
func resumePrefix(raw string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(raw))

	if value == "" {
		return "", nil
	}

	if len(value) < 4 {
		return "", errors.New("swap ID prefix is too short; use at least 4 characters")
	}

	return value, nil
}

// shouldHandOff reports whether a classified swap goes to the driver.
//
// A manual-attention swap, or one whose report could not be built, is not a candidate: the
// driver would only refuse it, and a command that says "resuming" and then refuses is worse
// than one that refuses.
func shouldHandOff(item resumeItem) bool {
	return !item.failed && item.class != reconcile.ClassManualAttention
}

// resumeItem is one swap the resume scan classified.
type resumeItem struct {
	record *model.Swap
	class  reconcile.Class
	action reconcile.Action
	reason string
	failed bool
}

// resumeOne handles an explicit prefix.
func resumeOne(
	ctx context.Context,
	engine *swap.Engine,
	prefix string,
	resumable, attention []resumeItem,
	maxSteps int,
) error {
	matches := append(append([]resumeItem{}, resumable...), attention...)

	if len(matches) == 0 {
		// A terminal swap is deliberately out of the live queue, but an explicit prefix
		// should still answer for it rather than claim it does not exist.
		terminal, err := engine.List(ctx)

		if err != nil {
			return err
		}

		var settled []*model.Swap

		for _, record := range terminal {
			if !strings.HasPrefix(record.SwapID, prefix) {
				continue
			}

			if record.State == state.Complete || record.State == state.BlakeRefunded {
				settled = append(settled, record)
			}
		}

		switch len(settled) {
		case 0:
			return fmt.Errorf("No live local swap matches prefix '%s'", prefix)
		case 1:
			fmt.Print(renderTerminal(settled[0]))

			return nil
		}

		return fmt.Errorf(
			"More than one terminal local swap matches prefix '%s'; use a longer prefix", prefix,
		)
	}

	if len(matches) > 1 {
		fmt.Print(renderAmbiguousResume(matches))

		return errors.New("Swap prefix is ambiguous; use more characters")
	}

	item := matches[0]

	if !shouldHandOff(item) {
		fmt.Print(renderResumeAttention(item))

		return nil
	}

	return handOverToDriver(ctx, engine, item, nil, maxSteps)
}

// handOverToDriver prints what is about to happen and runs the driver.
func handOverToDriver(
	ctx context.Context, engine *swap.Engine, item resumeItem, attention []resumeItem, maxSteps int,
) error {
	fmt.Print(renderResuming(item, attention))

	return runDriver(ctx, engine, driverHandoffArgs(item.record.SwapID, maxSteps))
}

// driverHandoffArgs is what resume passes to the driver.
//
// The budget is forwarded explicitly because the caller asked for it: a resume that silently
// used the driver's default would ignore the one knob an operator has over how much the
// automatic path may do in one invocation.
func driverHandoffArgs(swapID string, maxSteps int) []string {
	return []string{swapID, "--max-steps", strconv.Itoa(maxSteps)}
}

// renderNoResumable is what an empty resume queue prints.
func renderNoResumable(attention []resumeItem) string {
	out := "\nNO RESUMABLE SWAPS\n" +
		"==================\n" +
		"No swap currently has an automatic recovery, settlement, or waiting path.\n"

	if len(attention) > 0 {
		out += "\nNEEDS MANUAL ATTENTION\n" +
			"======================\n"

		for _, item := range attention {
			out += item.record.SwapID + "  " + string(item.record.State) + "  " + item.reason + "\n"
		}
	}

	return out
}

// renderMultipleResumable refuses to guess which swap the operator meant.
func renderMultipleResumable(resumable, attention []resumeItem) string {
	out := "\nMULTIPLE ACTIVE SWAPS\n" +
		"=====================\n" +
		"More than one swap has a live automatic path.\n" +
		"Refusing to guess which one you intend to resume.\n" +
		"\n" +
		"Swap          State             Next\n" +
		"------------  ----------------  ----------------\n"

	for _, item := range resumable {
		out += padRight(item.record.SwapID, 12) + "  " +
			padRight(string(item.record.State), 16) + "  " +
			padRight(string(item.action), 16) + "\n"
	}

	if len(attention) > 0 {
		out += "\nOther records needing manual attention:\n"

		for _, item := range attention {
			out += "  " + item.record.SwapID + "  " + string(item.record.State) + "\n"
		}
	}

	out += "\nRun `resume <short-swap-prefix>` for the active swap you want.\n"

	return out
}

// renderResuming names the swap and the action before the driver starts.
func renderResuming(item resumeItem, attention []resumeItem) string {
	out := "\nRESUMING SWAP\n" +
		"=============\n" +
		"swap:        " + item.record.SwapID + "\n" +
		"checkpoint:  " + string(item.record.State) + "\n" +
		"next:        " + string(item.action) + "\n"

	if len(attention) > 0 {
		out += fmt.Sprintf(
			"\nNOTE: %d unrelated record(s) need manual attention but do not block this swap.\n",
			len(attention),
		)
	}

	out += "\nHanding this swap to the existing autonomous driver.\n\n"

	return out
}

// renderResumeAttention refuses to hand a manual-attention swap to the driver.
func renderResumeAttention(item resumeItem) string {
	return "\nMANUAL ATTENTION REQUIRED\n" +
		"=========================\n" +
		"swap:   " + item.record.SwapID + "\n" +
		"state:  " + string(item.record.State) + "\n" +
		"reason: " + item.reason + "\n" +
		"\n" +
		"Refusing to hand a manual-attention swap to the autonomous runner.\n"
}

// renderAmbiguousResume lists the candidates a prefix matched.
func renderAmbiguousResume(matches []resumeItem) string {
	out := "\nAMBIGUOUS SWAP PREFIX\n" +
		"=====================\n"

	for _, item := range matches {
		out += item.record.SwapID + " " + string(item.class) + " " + string(item.action) + "\n"
	}

	return out
}

// renderTerminal answers for a swap that is already finished.
func renderTerminal(record *model.Swap) string {
	title, underline := "SWAP ALREADY SETTLED", "===================="

	if record.State == state.BlakeRefunded {
		title, underline = "SWAP ALREADY REFUNDED", "====================="
	}

	return "\n" + title + "\n" + underline + "\n" +
		"swap:  " + record.SwapID + "\n" +
		"state: " + string(record.State) + "\n" +
		"\n" +
		"No action required.\n"
}

// padRight pads a value to a column width.
func padRight(value string, width int) string {
	for len(value) < width {
		value += " "
	}

	return value
}

// runWatchHTLC records the HTLC in the watch-only wallet.
func runWatchHTLC(ctx context.Context, engine *swap.Engine, args []string) error {
	if len(args) < 1 {
		return errors.New("watch-htlc requires a swap id")
	}

	status, err := engine.WatchHTLC(ctx, args[0])

	if err != nil {
		return err
	}

	fmt.Println("WATCH-ONLY HTLC")
	fmt.Println("===============")
	fmt.Println("chain:  ", resultChainLabel(status.Chain))
	fmt.Println("wallet: ", status.Wallet)
	fmt.Println("address:", status.Address)

	if status.Imported {
		fmt.Println("PASS: descriptor imported; this HTLC stays solvable without the signing wallet")
	} else {
		fmt.Println("WARNING: the HTLC was NOT recorded in a watch-only wallet:", status.Reason)
	}

	return nil
}

// reportJSON renders the report with the reference's key names, so a Python
// dashboard and a Go one read the same document.
func reportJSON(report *reconcile.Report) map[string]any {
	observation := func(observation reconcile.Observation) map[string]any {
		out := map[string]any{
			"status": string(observation.Status),
		}

		if observation.Branch != reconcile.BranchNone {
			out["branch"] = string(observation.Branch)
		}

		if observation.TxID != "" {
			out["txid"] = observation.TxID
			out["vout"] = observation.Vout
		}

		if observation.BlockHash != "" {
			out["blockhash"] = observation.BlockHash
		}

		if observation.Confirmations > 0 {
			out["confirmations"] = observation.Confirmations
		}

		if observation.Reason != "" {
			out["reason"] = observation.Reason
		}

		return out
	}

	readiness := map[string]any{
		"checked": report.Readiness.Checked,
		"action":  string(report.Readiness.Action),
	}

	if report.Readiness.Ready != nil {
		readiness["ready"] = *report.Readiness.Ready
	}

	if report.Readiness.Wallet != "" {
		readiness["wallet"] = report.Readiness.Wallet
	}

	if report.Readiness.Trusted != nil {
		readiness["trusted_balance"] = report.Readiness.Trusted.String()
	}

	if report.Readiness.Required != nil {
		readiness["required_balance"] = report.Readiness.Required.String()
	}

	if len(report.Readiness.Reasons) > 0 {
		readiness["reasons"] = report.Readiness.Reasons
	}

	document := map[string]any{
		"swap_id":             report.SwapID,
		"participant_pubkey":  report.ParticipantPubkey,
		"protocol_role":       string(report.ProtocolRole),
		"counterparty_pubkey": report.Counterparty,
		"checkpoint_state":    string(report.CheckpointState),
		"decision_class":      string(report.DecisionClass),
		"negotiation_status":  report.NegotiationStatus,
		"negotiation_reason":  report.NegotiationReason,
		"secret": map[string]any{
			"known":        report.Secret.Known,
			"source":       report.Secret.Source,
			"hash_matches": report.Secret.HashMatches,
		},
		"blake":       observation(report.Blake),
		"btc":         observation(report.BTC),
		"next_action": string(report.NextAction),
		"reason":      report.Reason,
		"read_only":   true,
	}

	// A negotiation that is not ready was not observed, so it has no heights, no
	// timeouts and no readiness: those are facts about a handshake that never
	// completed. Reporting zeros for them would read as a real measurement.
	if report.NegotiationStatus != "ready" {
		return document
	}

	document["heights"] = map[string]any{
		"blake": report.Heights.Blake,
		"btc":   report.Heights.BTC,
	}
	document["timeouts"] = map[string]any{
		"blake":                  report.Timeouts.BlakeTimeout,
		"btc":                    report.Timeouts.BTCTimeout,
		"blake_blocks_remaining": report.Timeouts.BlakeBlocksRemaining,
		"btc_blocks_remaining":   report.Timeouts.BTCBlocksRemaining,
	}
	document["readiness"] = readiness

	return document
}

// printReconcile is the human form of the report.
func printReconcile(report *reconcile.Report) {
	fmt.Println()
	fmt.Println("BLAKE SWAP RECONCILIATION")
	fmt.Println("=========================")
	fmt.Println("swap id:          ", report.SwapID)
	fmt.Println("participant:      ", report.ParticipantPubkey)
	fmt.Println("protocol role:    ", report.ProtocolRole)
	fmt.Println("counterparty:     ", report.Counterparty)
	fmt.Println("checkpoint state: ", report.CheckpointState)
	fmt.Println()
	fmt.Println("CHAIN REALITY")
	fmt.Println("-------------")
	fmt.Println("BLAKE:", chainObservation(report.Blake))
	fmt.Println("BTC:  ", chainObservation(report.BTC))
	fmt.Println()
	fmt.Println("SECRET")
	fmt.Println("------")
	fmt.Println("known: ", yesNo(report.Secret.Known))
	fmt.Println("source:", report.Secret.Source)
	fmt.Println()
	fmt.Println("TIMEOUT DISTANCE")
	fmt.Println("----------------")

	// The distances are measurements, and a swap whose handshake never completed was not
	// measured. Printing zeros here would read as "the deadlines are already reached".
	if report.NegotiationStatus != "ready" {
		fmt.Println("not evaluated")
	} else {
		fmt.Println("BTC blocks:  ", report.Timeouts.BTCBlocksRemaining)
		fmt.Println("BLAKE blocks:", report.Timeouts.BlakeBlocksRemaining)
	}
	fmt.Println()
	fmt.Println("NEXT SAFE ACTION")
	fmt.Println("----------------")
	fmt.Println(report.NextAction)
	fmt.Println()
	fmt.Println("reason:")
	fmt.Println(report.Reason)
	fmt.Println()
	fmt.Println("PASS: reconciliation was read-only; no funds or messages were sent")

	if report.Readiness.Checked {
		fmt.Println()
		fmt.Println("EXECUTION READINESS")
		fmt.Println("-------------------")

		if report.Readiness.Ready != nil {
			fmt.Println("ready:", yesNo(*report.Readiness.Ready))
		}

		fmt.Println("wallet:", report.Readiness.Wallet)

		for _, reason := range report.Readiness.Reasons {
			fmt.Println("reason:", reason)
		}

		fmt.Println()
	}
}

// chainObservation renders one chain's answer.
func chainObservation(observation reconcile.Observation) string {
	text := string(observation.Status)

	if observation.Branch != reconcile.BranchNone {
		text += " / " + string(observation.Branch)
	}

	return text
}

// yesNo renders a boolean the way the reference does.
func yesNo(value bool) string {
	if value {
		return "yes"
	}

	return "no"
}

// resultChainLabel is a chain's display name.
func resultChainLabel(ch chain.Chain) string { return chainLabel(ch) }

// runRecoveryPlan prints how to recover an HTLC whose keys are gone.
func runRecoveryPlan(ctx context.Context, engine *swap.Engine, args []string) error {
	if len(args) < 1 {
		return errors.New("recovery-plan requires a swap id")
	}

	plan, err := engine.RecoveryPlan(ctx, args[0])

	if err != nil {
		return err
	}

	fmt.Println()
	fmt.Println("HTLC RECOVERY PLAN")
	fmt.Println("==================")
	fmt.Println("swap:  ", plan.SwapID)
	fmt.Println("state: ", plan.State)
	fmt.Println("role:  ", plan.Role)
	fmt.Println()
	fmt.Println("Read-only. Nothing is imported, signed, funded or broadcast by this command.")

	for _, leg := range plan.Legs {
		label := chainLabel(leg.Chain)

		fmt.Println()
		fmt.Println(strings.ToUpper(label))
		fmt.Println(strings.Repeat("-", len(label)))

		if leg.Descriptor == "" {
			fmt.Println("not derivable:", leg.Reason)

			continue
		}

		fmt.Println("descriptor:  ", leg.Descriptor)
		fmt.Println("address:     ", leg.Address)
		fmt.Println("amount:      ", leg.Amount)
		fmt.Println("timeout:     ", leg.Timeout)

		if leg.FundingTxID != "" {
			fmt.Println("funding:     ", leg.FundingTxID)
		}

		if leg.OurClaimKey != "" {
			fmt.Println("claim key:   ", leg.OurClaimKey)
		}

		if leg.OurRefundKey != "" {
			fmt.Println("refund key:  ", leg.OurRefundKey)
		}

		switch {
		case leg.Solvable == nil:
			fmt.Println("solvable:     unknown", leg.Reason)
		case *leg.Solvable:
			fmt.Println("solvable:     yes, via wallet", leg.SolvableWallet)
		default:
			fmt.Println("solvable:     NO")
			fmt.Println("warning:      no loaded wallet can spend this HTLC")
			fmt.Println("              import the descriptor with `watch-htlc`, or substitute your key")
		}
	}

	fmt.Println()

	return nil
}

// runRefreshTimeouts recomputes an unfunded offer's deadlines.
func runRefreshTimeouts(ctx context.Context, engine *swap.Engine, args []string) error {
	flags := flag.NewFlagSet("refresh-timeouts", flag.ContinueOnError)

	clearStale := flags.Bool("clear-stale-intent", false, "clear a stale funding intent")

	positional, err := parsePositional(flags, args, 1)

	if err != nil {
		return err
	}

	if len(positional) != 1 {
		return errors.New("refresh-timeouts requires a swap id")
	}

	result, err := engine.RefreshTimeouts(ctx, positional[0], *clearStale)

	if err != nil {
		return err
	}

	for _, cleared := range result.ClearedIntents {
		fmt.Println("CLEARED STALE FUNDING INTENT:", cleared)
	}

	fmt.Println()
	fmt.Println("TIMEOUTS REFRESHED")
	fmt.Println("==================")
	fmt.Println("swap id:          ", result.SwapID)
	fmt.Println("Bitcoin height:   ", result.BTCHeight)
	fmt.Println("BLAKE height:     ", result.BlakeHeight)
	fmt.Println()
	fmt.Println("old BTC timeout:  ", result.OldBTCTimeout)
	fmt.Println("new BTC timeout:  ", result.NewBTCTimeout)
	fmt.Println("old BLAKE timeout:", result.OldBlakeTimeout)
	fmt.Println("new BLAKE timeout:", result.NewBlakeTimeout)

	return nil
}

// runSync reconciles one swap's state against confirmed chain history.
func runSync(
	ctx context.Context,
	engine *swap.Engine,
	args []string,
	call func(context.Context, string) (*swap.SyncResult, error),
	label string,
) error {
	if len(args) < 1 {
		return fmt.Errorf("%s reconciliation requires a swap id", strings.ToLower(label))
	}

	result, err := call(ctx, args[0])

	if err != nil {
		return err
	}

	fmt.Println(label, "CHAIN RECONCILIATION")
	fmt.Println("=====================")
	fmt.Println("swap id:", result.SwapID)
	fmt.Println("start:  ", result.Start)
	fmt.Println("end:    ", result.End)

	if len(result.Recovered) == 0 {
		fmt.Println("recovered: nothing")
	} else {
		fmt.Println("recovered:", strings.Join(result.Recovered, ", "))
	}

	return nil
}

// runDashboard summarises every local swap.
func runDashboard(ctx context.Context, engine *swap.Engine, args []string) error {
	flags := flag.NewFlagSet("dashboard", flag.ContinueOnError)

	asJSON := flags.Bool("json", false, "emit the machine-readable dashboard")

	if err := flags.Parse(args); err != nil {
		return err
	}

	entries, err := engine.Dashboard(ctx)

	if err != nil {
		return err
	}

	summary := swap.Classify(entries)
	summary.Archived = engine.Archive(ctx)

	if *asJSON {
		encoded, err := json.MarshalIndent(dashboardJSON(summary), "", "  ")

		if err != nil {
			return err
		}

		fmt.Println(string(encoded))

		return nil
	}

	fmt.Print(renderDashboard(summary))

	return nil
}

// dashboardJSON is the reference's dashboard document.
//
// A dashboard is read by things other than people — a widget, a monitor, a script that
// alerts on "needs attention" — so its shape is a contract and every field is named once,
// here, rather than by whoever prints it.
func dashboardJSON(summary swap.Dashboard) map[string]any {
	item := func(entry swap.DashboardEntry) map[string]any {
		return map[string]any{
			"swap_id":        entry.SwapID,
			"state":          string(entry.State),
			"protocol_role":  string(entry.Role),
			"decision_class": string(entry.Class),
			"next_action":    string(entry.Action),
			"reason":         entry.Reason,
			"status":         entry.Outcome,
			"action":         dashboardActionText(entry.Action),
			"trade": map[string]any{
				// Both amounts, as everywhere else: a UI wants the display value and a
				// settlement wants the exact base units.
				"blake_amount":            json.Number(pythonFloat(dashboardFloat(entry.BlakeAmount))),
				"blake_amount_base_units": int64(entry.BlakeAmount),
				"btc_sats":                entry.BTCSats,
			},
			"chains": map[string]any{
				"blake_status": string(entry.BlakeStatus),
				"btc_status":   string(entry.BTCStatus),
				// Solvability is not part of the record: it is a question put to the
				// wallets, so a dashboard that reports it must have asked them.
				"blake_solvable": nil,
				"btc_solvable":   nil,
			},
			"transactions": map[string]any{
				"blake_funding_txid": nilOrValue(entry.TxIDs["blake_funding_txid"]),
				"btc_funding_txid":   nilOrValue(entry.TxIDs["btc_funding_txid"]),
				"btc_claim_txid":     nilOrValue(entry.TxIDs["btc_claim_txid"]),
				"blake_claim_txid":   nilOrValue(entry.TxIDs["blake_claim_txid"]),
				"btc_refund_txid":    nilOrValue(entry.TxIDs["btc_refund_txid"]),
				"blake_refund_txid":  nilOrValue(entry.TxIDs["blake_refund_txid"]),
			},
		}
	}

	items := func(entries []swap.DashboardEntry) []map[string]any {
		out := make([]map[string]any, 0, len(entries))

		for _, entry := range entries {
			out = append(out, item(entry))
		}

		return out
	}

	archived := make([]map[string]any, 0, len(summary.Archived))

	for _, entry := range summary.Archived {
		archived = append(archived, map[string]any{
			"swap_id":      entry.SwapID,
			"state":        string(entry.State),
			"blake_amount": dashboardFloat(entry.BlakeAmount),
			"btc_sats":     entry.BTCSats,
		})
	}

	return map[string]any{
		"version":   1,
		"active":    items(summary.Active),
		"attention": items(summary.Attention),
		"settled":   items(summary.Settled),
		"archived":  archived,
		"summary": map[string]any{
			"active":    len(summary.Active),
			"attention": len(summary.Attention),
			"settled":   len(summary.Settled),
			"archived":  len(summary.Archived),
		},
	}
}

// nilOrValue keeps "this swap has no such transaction" and "it has an empty one" distinct.
//
// The reference writes null for an absent transaction, and a client that received an empty
// string would have to guess which of the two it was.
func nilOrValue(value string) any {
	if value == "" {
		return nil
	}

	return value
}

// dashboardFloat is the display value of a BLAKE amount, as a float.
func dashboardFloat(value amount.Blake) float64 {
	return blakeFloat(value)
}

// dashboardActionText is the command a person would run for an action.
func dashboardActionText(action reconcile.Action) string {
	switch action {
	case reconcile.ActionSyncFunding, reconcile.ActionSyncSettlement,
		reconcile.ActionComplete, reconcile.ActionFundBTC, reconcile.ActionClaimBTC,
		reconcile.ActionClaimBlake, reconcile.ActionRefundBTC, reconcile.ActionRefundBlake:
		return "run resume"
	case reconcile.ActionFundBlake:
		return "run fund"
	case reconcile.ActionWait:
		return "wait"
	case reconcile.ActionManualAttention:
		return "inspect"
	case reconcile.ActionNone, "":
		return "-"
	}

	return string(action)
}

// renderDashboard is the reference's human view: three sections and an archive count.
func renderDashboard(summary swap.Dashboard) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("BLAKE SWAP DASHBOARD\n")
	out.WriteString("====================\n")

	out.WriteString("\nACTIVE\n------\n")

	if len(summary.Active) == 0 {
		out.WriteString("No active swaps.\n")
	} else {
		out.WriteString("Swap          Status                    Action       Trade\n")
		out.WriteString("------------  ------------------------  -----------  -------------------------\n")

		for _, entry := range summary.Active {
			fmt.Fprintf(&out, "%-12s  %-24s  %-11s  %s\n",
				entry.SwapID, entry.Outcome, dashboardActionText(entry.Action),
				tradeText(entry),
			)
		}
	}

	out.WriteString("\nNEEDS ATTENTION\n---------------\n")

	if len(summary.Attention) == 0 {
		out.WriteString("No swaps need manual attention.\n")
	} else {
		for _, entry := range summary.Attention {
			fmt.Fprintf(&out, "%s  %s  %s\n", entry.SwapID, entry.Outcome, entry.Reason)
		}
	}

	out.WriteString("\nSETTLED\n-------\n")

	if len(summary.Settled) == 0 {
		out.WriteString("No settled swaps.\n")
	} else {
		for _, entry := range summary.Settled {
			fmt.Fprintf(&out, "%s  %s  %s\n", entry.SwapID, entry.Outcome, tradeText(entry))
		}
	}

	out.WriteString("\nARCHIVED\n--------\n")

	if len(summary.Archived) == 0 {
		out.WriteString("No archived swaps.\n")
	} else {
		for _, entry := range summary.Archived {
			fmt.Fprintf(&out, "%s  %s  %s\n", entry.SwapID, entry.Outcome, tradeText(entry))
		}
	}

	fmt.Fprintf(&out, "\nSUMMARY\n-------\n")
	fmt.Fprintf(&out, "active:     %d\n", len(summary.Active))
	fmt.Fprintf(&out, "attention:  %d\n", len(summary.Attention))
	fmt.Fprintf(&out, "settled:    %d\n", len(summary.Settled))
	fmt.Fprintf(&out, "archived:   %d\n", len(summary.Archived))

	return out.String()
}

// tradeText is the trade as one line.
func tradeText(entry swap.DashboardEntry) string {
	return pythonFloat(blakeFloat(entry.BlakeAmount)) + " BLAKE / " +
		thousands(entry.BTCSats) + " sats"
}

// runResumeAll resumes every swap that has an action.
func runResumeAll(ctx context.Context, engine *swap.Engine) error {
	results, err := engine.ResumeAll(ctx)

	if err != nil {
		return err
	}

	fmt.Println()
	fmt.Println("RESUME ALL")
	fmt.Println("==========")

	performed := 0

	for _, result := range results {
		if result.Performed {
			performed++

			fmt.Printf("%s  performed %-12s txid=%s\n",
				result.SwapID, result.Action, result.TxID)

			continue
		}

		fmt.Printf("%s  no action (%s)\n", result.SwapID, result.Action)
	}

	fmt.Println()
	fmt.Println("actions performed:", performed)

	return nil
}

// ---------------------------------------------------------------- helpers

// parsePositional parses flags and returns the positional arguments, allowing
// flags and positionals to be interleaved the way the reference CLI accepts them.
func parsePositional(flags *flag.FlagSet, args []string, want int) ([]string, error) {
	var (
		positional []string
		rest       []string
	)

	// Flags may appear before or after the positionals.
	for i := 0; i < len(args); i++ {
		arg := args[i]

		if !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)

			continue
		}

		rest = append(rest, arg)

		name := strings.TrimLeft(arg, "-")

		if strings.Contains(name, "=") {
			continue
		}

		if isBoolFlag(flags, name) {
			continue
		}

		if i+1 < len(args) {
			rest = append(rest, args[i+1])
			i++
		}
	}

	if err := flags.Parse(rest); err != nil {
		return nil, err
	}

	if len(positional) != want {
		return positional, nil
	}

	return positional, nil
}

// isBoolFlag reports whether a flag is a boolean, so its value is not consumed.
func isBoolFlag(flags *flag.FlagSet, name string) bool {
	found := flags.Lookup(name)

	if found == nil {
		return false
	}

	type boolFlag interface{ IsBoolFlag() bool }

	boolean, ok := found.Value.(boolFlag)

	return ok && boolean.IsBoolFlag()
}

// printRecord renders one swap the way the reference CLI does, one fact per line.
// printRecord prints one swap the way the reference does.
//
// The shape is the reference's, including its alignment: an operator reads this output
// beside the Python engine's, and a field that moved or a label that shortened would make
// the two impossible to compare.
func printRecord(record *model.Swap) {
	fmt.Print(renderSwap(record))
}

// renderSwap is the reference's print_swap, byte for byte.
func renderSwap(record *model.Swap) string {
	var out strings.Builder

	secret := "None"

	if record.SecretHex != nil {
		secret = *record.SecretHex
	}

	out.WriteString("\n")
	out.WriteString("ATOMIC SWAP\n")
	out.WriteString("===========\n")
	fmt.Fprintf(&out, "swap id:        %s\n", record.SwapID)
	fmt.Fprintf(&out, "state:          %s\n", record.State)
	out.WriteString("\n")
	out.WriteString("TRADE\n")
	out.WriteString("-----\n")
	fmt.Fprintf(&out, "BLAKE:          %s\n", pythonFloat(blakeFloat(record.BlakeAmount())))
	fmt.Fprintf(&out, "BTC sats:       %d\n", record.BTCSats)
	out.WriteString("\n")
	out.WriteString("HASHLOCK\n")
	out.WriteString("--------\n")
	fmt.Fprintf(&out, "secret:         %s\n", secret)
	fmt.Fprintf(&out, "SHA256:         %s\n", record.SecretHash)
	out.WriteString("\n")
	out.WriteString("TIMEOUTS\n")
	out.WriteString("--------\n")
	fmt.Fprintf(&out, "BTC timeout:    %d\n", record.BTCTimeout)
	fmt.Fprintf(&out, "BLAKE timeout:  %d\n", record.BlakeTimeout)

	// The two CLTV heights are absolute heights on independent chains and have no
	// ordering relationship; the v1.5 policy records the safety margins as wall-clock
	// seconds, which is the only meaningful comparison. So the windows are printed and
	// a height difference is not.
	if record.TimeoutPolicyVersion != nil && *record.TimeoutPolicyVersion == "1.5" {
		fmt.Fprintf(&out, "BTC window:     %d s\n", secondsOf(record.BTCTimeoutWindowSeconds))
		fmt.Fprintf(&out, "Bob recovery:   %d s\n", secondsOf(record.BobRecoverySeconds))
		fmt.Fprintf(&out, "BLAKE window:   %d s\n", secondsOf(record.BlakeTimeoutWindowSeconds))
	}

	out.WriteString("\n")
	out.WriteString("TRANSACTIONS\n")
	out.WriteString("------------\n")
	fmt.Fprintf(&out, "BLAKE funding:  %s\n", txidOrDash(record.BlakeFundingTxID))
	fmt.Fprintf(&out, "BTC funding:    %s\n", txidOrDash(record.BTCFundingTxID))
	fmt.Fprintf(&out, "BTC claim:      %s\n", txidOrDash(record.BTCCLaimTxID))
	fmt.Fprintf(&out, "BLAKE claim:    %s\n", txidOrDash(record.BlakeClaimTxID))

	return out.String()
}

// secondsOf reads an optional seconds field, treating absent as zero.
func secondsOf(value *int64) int64 {
	if value == nil {
		return 0
	}

	return *value
}

// txidOrDash renders a transaction id the way the reference does: a dash when there is
// none, so the column stays readable.
func txidOrDash(value *string) string {
	if value == nil || *value == "" {
		return "-"
	}

	return *value
}

func chainSuffix(ch chain.Chain) string {
	if ch == chain.Blake {
		return "blake"
	}

	return "btc"
}

func chainLabel(ch chain.Chain) string {
	if ch == chain.Blake {
		return "BLAKE"
	}

	return "Bitcoin"
}

// randReader is the entropy source for secrets and identifiers.
func randReader() io.Reader { return rand.Reader }

// ---------------------------------------------------------------- engine

// buildEngine wires the engine from the environment the reference CLI uses.
func buildEngine() (*swap.Engine, func(), error) {
	cleanup := func() {}

	stateDir := os.Getenv("BLAKE_SWAP_STATE_DIR")

	if stateDir == "" {
		home, err := os.UserHomeDir()

		if err != nil {
			return nil, cleanup, err
		}

		stateDir = filepath.Join(home, ".blake-swaps-v2")
	}

	messageDir := os.Getenv("BLAKE_SWAP_MESSAGE_DIR")

	records := store.NewFile(stateDir)

	if messageDir != "" && filepath.Clean(messageDir) != filepath.Join(stateDir, "messages") {
		records.SetMessageDirectory(messageDir)
	}

	intents := fund.NewFileStore(filepath.Join(stateDir, "funding-intents"))

	blakeNode, err := nodeFromDatadir(chain.Blake)

	if err != nil {
		return nil, cleanup, err
	}

	btcNode, err := nodeFromDatadir(chain.BTC)

	if err != nil {
		return nil, cleanup, err
	}

	identity, err := loadIdentity(stateDir)

	if err != nil {
		return nil, cleanup, err
	}

	mode := nostr.ModeOff

	if raw := os.Getenv("BLAKE_SWAP_NOSTR_ENCRYPTION"); raw != "" {
		mode, err = nostr.ParseMode(raw)

		if err != nil {
			return nil, cleanup, err
		}
	}

	engine := swap.NewEngine(records, intents, blakeNode, btcNode)

	engine.Identity = identity
	engine.Mode = mode
	engine.Role = roleFromEnv()
	engine.BlakeWallet = walletFromEnv("BLAKE_SWAP_BLAKE_WALLET", engine.Role)
	engine.BTCWallet = walletFromEnv("BLAKE_SWAP_BTC_WALLET", engine.Role)
	engine.BlakeWatchWallet = engine.BlakeWallet + "-watch"
	engine.BTCWatchWallet = engine.BTCWallet + "-watch"
	engine.TestMode = os.Getenv("BLAKE_SWAP_TEST_MODE") == "1"
	engine.Funding.AutoMine = autoMine()
	engine.Now = time.Now

	//
	// The file transport is the only one wired here so far, and it is the one the
	// suites and a same-host Python participant use. A relay transport is the
	// browser client's job, not the CLI's.
	//
	engine.Transport = &coord.FileTransport{Spool: records, Sender: identity.PublicKey()}

	return engine, cleanup, nil
}

// nodeFromDatadir builds a node from a regtest datadir.
//
// The reference engine shells out to bitcoin-cli; this one speaks RPC, so it needs
// the port from bitcoin.conf and the cookie the node wrote. Both are what a
// datadir already contains, which keeps the configuration surface identical.
func nodeFromDatadir(ch chain.Chain) (*chain.Node, error) {
	envName := "BLAKE_SWAP_BTC_DATADIR"

	if ch == chain.Blake {
		envName = "BLAKE_SWAP_BLAKE_DATADIR"
	}

	datadir := os.Getenv(envName)

	if datadir == "" {
		home, err := os.UserHomeDir()

		if err != nil {
			return nil, err
		}

		// The test rig publishes its datadirs; a real deployment sets the variable.
		datadir = filepath.Join(home, ".blake-swap-test", string(ch))
	}

	port, err := rpcPort(datadir)

	if err != nil {
		return nil, err
	}

	cookie, err := os.ReadFile(filepath.Join(datadir, "regtest", ".cookie"))

	if err != nil {
		return nil, fmt.Errorf("%s: read RPC cookie: %w", envName, err)
	}

	user, password, ok := strings.Cut(strings.TrimSpace(string(cookie)), ":")

	if !ok {
		return nil, fmt.Errorf("%s: malformed RPC cookie", envName)
	}

	// A node that accepts the connection and then never answers must not hang the
	// engine forever: without a deadline that is an engine with no error and nothing to
	// retry. The default and the override are the reference's.
	timeout, err := chain.TimeoutFromEnv()

	if err != nil {
		return nil, err
	}

	return chain.New(ch, chain.Config{
		URL:                 "http://127.0.0.1:" + strconv.Itoa(port) + "/",
		User:                user,
		Password:            password,
		TimeoutMilliseconds: int(timeout / time.Millisecond),
	})
}

// rpcPort reads rpcport from a datadir's bitcoin.conf.
func rpcPort(datadir string) (int, error) {
	conf, err := os.ReadFile(filepath.Join(datadir, "bitcoin.conf"))

	if err != nil {
		return 0, fmt.Errorf("read bitcoin.conf: %w", err)
	}

	for _, line := range strings.Split(string(conf), "\n") {
		line = strings.TrimSpace(line)

		if strings.HasPrefix(line, "rpcport") {
			_, value, ok := strings.Cut(line, "=")

			if ok {
				return strconv.Atoi(strings.TrimSpace(value))
			}
		}
	}

	return 0, fmt.Errorf("bitcoin.conf has no rpcport")
}

// loadIdentity reads this participant's Nostr key.
//
// The reference engine's order is kept: an explicit key file first, then the
// legacy key file, then a role-derived file in the key directory.
func loadIdentity(stateDir string) (swap.Identity, error) {
	candidates := []string{
		os.Getenv("BLAKE_SWAP_PARTICIPANT_KEY_FILE"),
		os.Getenv("BLAKE_SWAP_NOSTR_KEY_FILE"),
	}

	directory := os.Getenv("BLAKE_SWAP_NOSTR_KEY_DIR")

	if directory != "" {
		candidates = append(candidates, filepath.Join(directory, roleFromEnv()+".key"))
	}

	candidates = append(candidates, filepath.Join(stateDir, roleFromEnv()+".nostr-key"))

	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}

		raw, err := os.ReadFile(candidate)

		if err != nil {
			continue
		}

		key, err := nostr.NewKey(strings.TrimSpace(string(raw)))

		if err != nil {
			return nil, fmt.Errorf("%s: %w", candidate, err)
		}

		return key, nil
	}

	return nil, fmt.Errorf(
		"no participant key: set BLAKE_SWAP_PARTICIPANT_KEY_FILE (a file holding 64 hex characters)",
	)
}

// roleFromEnv is the local protocol role.
func roleFromEnv() string {
	if role := os.Getenv("BLAKE_SWAP_ROLE"); role != "" {
		return role
	}

	return store.Alice
}

// walletFromEnv reads a wallet name, defaulting to the role's wallet.
func walletFromEnv(name, role string) string {
	if wallet := os.Getenv(name); wallet != "" {
		return wallet
	}

	return role
}

// autoMine reads the regtest auto-mine switch, defaulting on like the reference.
func autoMine() int64 {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("BLAKE_SWAP_AUTO_MINE")))

	switch raw {
	case "", "1", "true", "yes", "on":
		return 1
	case "0", "false", "no", "off":
		return 0
	}

	return 0
}
