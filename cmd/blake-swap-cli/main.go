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
	"github.com/gjbrr/blake-swap/pkg/store"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	command := os.Args[1]
	args := os.Args[2:]

	// A market read talks to relays, not to nodes, so it runs before the engine is
	// built: looking for an offer must not require a regtest datadir and two nodes.
	if isMarketCommand(command) {
		if err := run(context.Background(), nil, command, args); err != nil {
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

// fail prints an error the way an operator (or a test) expects to read it.
func fail(err error) {
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
  publish-offer SWAP_ID
  accept-offer SWAP_ID [--expected-alice-pub HEX] [--allow-unpinned-author]
  receive-accept SWAP_ID
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
  resume SWAP_ID
  watch-htlc SWAP_ID
  recovery-plan SWAP_ID
  refresh-timeouts SWAP_ID [--clear-stale-intent]
  sync-funding SWAP_ID
  sync-settlement SWAP_ID
  dashboard
  resume-all
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
		return runDashboard(ctx, engine)
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

	for _, record := range records {
		fmt.Println(record.SwapID, record.State)
	}

	return nil
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

	fmt.Println("OFFER PUBLISHED")
	fmt.Println("===============")
	fmt.Println("swap id:", args[0])
	fmt.Println("message:", result.Location)

	return nil
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

	fmt.Println("OFFER ACCEPTED")
	fmt.Println("==============")
	fmt.Println("swap id:", result.Swap.SwapID)
	fmt.Println("accept message:", result.Location)
	fmt.Println("Bob state:", result.State)

	if result.Timeout != nil {
		fmt.Println("BTC remaining safety (h):",
			strconv.FormatInt(result.Timeout.BTCRemainingSeconds/3600, 10))
		fmt.Println("cross-chain recovery gap (s):", result.Timeout.ActualRecoveryGap)
	}

	return nil
}

func runReceiveAccept(ctx context.Context, engine *swap.Engine, args []string) error {
	if len(args) < 1 {
		return errors.New("receive-accept requires a swap id")
	}

	result, err := engine.ReceiveAccept(ctx, args[0])

	if err != nil {
		return err
	}

	fmt.Println("ACCEPT RECEIVED")
	fmt.Println("===============")
	fmt.Println("swap id:", args[0])
	fmt.Println("message:", result.Location)
	fmt.Println("state:  ", result.State)

	return nil
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
	if len(args) < 1 {
		return fmt.Errorf("%s requires a swap id", label)
	}

	result, err := call(ctx, args[0])

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

// runResume performs the recommended action once.
func runResume(ctx context.Context, engine *swap.Engine, args []string) error {
	if len(args) < 1 {
		return errors.New("resume requires a swap id")
	}

	result, err := engine.Resume(ctx, args[0])

	if err != nil {
		return err
	}

	fmt.Println("swap id:", result.SwapID)
	fmt.Println("next action:", result.Action)
	fmt.Println("class:", result.Class)
	fmt.Println("reason:", result.Reason)

	if result.Performed {
		fmt.Println("performed: yes")
		fmt.Println("txid:", result.TxID)
		fmt.Println("state:", result.State)
	} else {
		fmt.Println("performed: no")
	}

	return nil
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

	return map[string]any{
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
		"heights": map[string]any{
			"blake": report.Heights.Blake,
			"btc":   report.Heights.BTC,
		},
		"timeouts": map[string]any{
			"blake":                  report.Timeouts.BlakeTimeout,
			"btc":                    report.Timeouts.BTCTimeout,
			"blake_blocks_remaining": report.Timeouts.BlakeBlocksRemaining,
			"btc_blocks_remaining":   report.Timeouts.BTCBlocksRemaining,
		},
		"blake":       observation(report.Blake),
		"btc":         observation(report.BTC),
		"next_action": string(report.NextAction),
		"reason":      report.Reason,
		"readiness":   readiness,
	}
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
	fmt.Println("BTC blocks:  ", report.Timeouts.BTCBlocksRemaining)
	fmt.Println("BLAKE blocks:", report.Timeouts.BlakeBlocksRemaining)
	fmt.Println()
	fmt.Println("DECISION")
	fmt.Println("--------")
	fmt.Println("class:      ", report.DecisionClass)
	fmt.Println("next action:", report.NextAction)
	fmt.Println("reason:     ", report.Reason)

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
	}

	fmt.Println()
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
func runDashboard(ctx context.Context, engine *swap.Engine) error {
	entries, err := engine.Dashboard(ctx)

	if err != nil {
		return err
	}

	fmt.Println()
	fmt.Println("SWAP DASHBOARD")
	fmt.Println("==============")

	if len(entries) == 0 {
		fmt.Println("No swaps.")

		return nil
	}

	for _, entry := range entries {
		fmt.Printf("%s  %-14s %-22s %s\n",
			entry.SwapID, entry.State, entry.Outcome, entry.Action)
	}

	return nil
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
func printRecord(record *model.Swap) {
	fmt.Println()
	fmt.Println("ATOMIC SWAP")
	fmt.Println("===========")
	fmt.Println("swap id:      ", record.SwapID)
	fmt.Println("state:        ", record.State)
	fmt.Println("created:      ", record.CreatedAt)
	fmt.Println("BLAKE amount: ", amount.Blake(record.BlakeAmountBaseUnits).Format())
	fmt.Println("BTC sats:     ", record.BTCSats)
	fmt.Println("secret hash:  ", record.SecretHash)

	if record.SecretHex != nil {
		fmt.Println("has secret:    yes")
	}

	if record.RevealedSecretHex != nil {
		fmt.Println("revealed:      yes")
	}

	fmt.Println("BTC timeout:  ", record.BTCTimeout)
	fmt.Println("BLAKE timeout:", record.BlakeTimeout)

	printOutpoint("BLAKE funding", record.BlakeFundingTxID, record.BlakeFundingBlockHash)
	printOutpoint("BTC funding", record.BTCFundingTxID, record.BTCFundingBlockHash)
	printOutpoint("BTC claim", record.BTCCLaimTxID, record.BTCCLaimBlockHash)
	printOutpoint("BLAKE claim", record.BlakeClaimTxID, record.BlakeClaimBlockHash)
	printOutpoint("BTC refund", record.BTCRefundTxID, record.BTCRefundBlockHash)
	printOutpoint("BLAKE refund", record.BlakeRefundTxID, record.BlakeRefundBlockHash)
	fmt.Println()
}

func printOutpoint(label string, txid, blockHash *string) {
	if txid == nil || *txid == "" {
		return
	}

	fmt.Printf("%s: %s", label, *txid)

	if blockHash != nil && *blockHash != "" {
		fmt.Printf(" (block %s)", *blockHash)
	}

	fmt.Println()
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

	return chain.New(ch, chain.Config{
		URL:      "http://127.0.0.1:" + strconv.Itoa(port) + "/",
		User:     user,
		Password: password,
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
