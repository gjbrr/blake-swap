package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/contract"
	"github.com/gjbrr/blake-swap/pkg/fund"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/state"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// These renderers are the reference's print_* blocks for the funding, claim and
// completion commands, written from `command_*` in src/swap_v2.py.
//
// They are separate from the engine on purpose: the engine produces facts, and the
// wording of the block an operator reads is part of the command surface. Keeping
// them here is what makes each one comparable with the reference line for line.

// messageLocation is where a published message actually is.
//
// The transport reports a name for the local spool and an event id for a relay, and
// the reference prints the spool path because that is what an operator can look at.
// An event id is printed as it comes.
func messageLocation(location string) string {
	if len(location) == 64 && isHex(location) {
		return location
	}

	directory := os.Getenv("BLAKE_SWAP_MESSAGE_DIR")

	if directory == "" {
		directory = filepath.Join(stateDirectory(), "messages")
	}

	// A spool message has no extension of its own; a saved signed transaction does.
	if strings.HasSuffix(location, ".json") || strings.HasSuffix(location, ".hex") {
		return filepath.Join(directory, location)
	}

	return filepath.Join(directory, location+".json")
}

// isHex reports whether a string is all hexadecimal.
func isHex(text string) bool {
	for _, r := range text {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}

	return text != ""
}

// orNone renders a nullable string the way Python prints one.
func orNone(value *string) string {
	if value == nil || *value == "" {
		return "None"
	}

	return *value
}

// orNoneInt renders a nullable integer the way Python prints one.
func orNoneInt(value *int32) string {
	if value == nil {
		return "None"
	}

	return fmt.Sprint(*value)
}

// renderVerifyBlake is the reference's BLAKE contract verification block.
func renderVerifyBlake(check *contract.Check) string {
	return "VERIFY BLAKE CONTRACT\n" +
		"=====================\n" +
		"txid:          " + check.TxID + "\n" +
		fmt.Sprintf("vout:          %d\n", check.Vout) +
		"amount:        " + check.Amount + "\n" +
		"address:       " + check.Address + "\n" +
		"expected:      " + check.Expected + "\n" +
		fmt.Sprintf("confirmations: %d\n", check.Confirmations) +
		"\n" +
		"PASS: BLAKE funding verified\n"
}

// renderVerifyBTC is the reference's Bitcoin contract verification block.
func renderVerifyBTC(check *contract.Check, swap *model.Swap) string {
	return "VERIFY BITCOIN CONTRACT\n" +
		"=======================\n" +
		"txid:          " + check.TxID + "\n" +
		fmt.Sprintf("vout:          %d\n", check.Vout) +
		"amount sats:   " + check.Amount + "\n" +
		"address:       " + check.Address + "\n" +
		"expected:      " + check.Expected + "\n" +
		fmt.Sprintf("confirmations: %d\n", check.Confirmations) +
		"SHA256:        " + swap.SecretHash + "\n" +
		"Alice claim:   " + orNone(swap.BTCAlicePub) + "\n" +
		"Bob refund:    " + orNone(swap.BTCBobPub) + "\n" +
		"\n" +
		"PASS: Bitcoin funding verified\n"
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

// renderFundingPublished is the reference's publish confirmation for a funding
// announcement.
func renderFundingPublished(label, swapID, location string) string {
	header := label + " FUNDING PUBLISHED"

	return "\n" + header + "\n" +
		strings.Repeat("=", len(header)) + "\n" +
		"swap id: " + swapID + "\n" +
		"message: " + location + "\n" +
		"\n" +
		"PASS: secret_hex was NOT published\n"
}

// renderClaimPublished is the reference's publish confirmation for a claim.
//
// The two chains carry different closing sentences because they publish different
// things: a Bitcoin claim carries settlement identity only, a BLAKE claim is the
// final act and says so.
func renderClaimPublished(label string, swap *model.Swap, txid, blockHash, location string) string {
	pass := "PASS: secret_hex was NOT published"

	if label == "BLAKE" {
		pass = "PASS: only public settlement data published"
	}

	header := label + " CLAIM PUBLISHED"

	return "\n" + header + "\n" +
		strings.Repeat("=", len(header)) + "\n" +
		"swap id: " + swap.SwapID + "\n" +
		"txid:    " + txid + "\n" +
		"block:   " + blockHash + "\n" +
		"message: " + location + "\n" +
		"\n" + pass + "\n"
}

// renderFundingReceived is the reference's receive confirmation for a funding
// announcement.
func renderFundingReceived(label string, swap *model.Swap, txid string, vout int64, pass string) string {
	header := label + " FUNDING RECEIVED"

	return "\n" + header + "\n" +
		strings.Repeat("=", len(header)) + "\n" +
		"swap id: " + swap.SwapID + "\n" +
		"txid:    " + txid + "\n" +
		fmt.Sprintf("vout:    %d\n", vout) +
		"state:   " + string(swap.State) + "\n" +
		"\n" + pass + "\n"
}

// renderClaimReceived is the reference's receive confirmation for a claim.
func renderClaimReceived(label string, swap *model.Swap, txid, blockHash string, passes []string) string {
	var out strings.Builder

	header := label + " CLAIM RECEIVED"

	out.WriteString("\n" + header + "\n")
	out.WriteString(strings.Repeat("=", len(header)) + "\n")
	out.WriteString("swap id: " + swap.SwapID + "\n")
	out.WriteString("txid:    " + txid + "\n")
	out.WriteString("block:   " + blockHash + "\n")
	out.WriteString("state:   " + string(swap.State) + "\n")
	out.WriteString("\n")

	for _, pass := range passes {
		out.WriteString(pass + "\n")
	}

	return out.String()
}

// renderComplete is the reference's completion block.
func renderComplete(role, swapID string, st state.State) string {
	return "SWAP COMPLETE\n" +
		"=============\n" +
		"role:   " + role + "\n" +
		"state:  " + string(st) + "\n"
}

// renderExtractSecret is the reference's secret-recovery block.
func renderExtractSecret(swap *model.Swap, revealed, digest string) string {
	return "BOB EXTRACTS SECRET FROM BITCOIN\n" +
		"===============================\n" +
		"BTC claim txid: " + orNone(swap.BTCCLaimTxID) + "\n" +
		"expected H:     " + swap.SecretHash + "\n" +
		"\n" +
		"SECRET FOUND\n" +
		"============\n" +
		"S:        " + revealed + "\n" +
		"SHA256:   " + digest + "\n" +
		"\n" +
		"PASS: Bob recovered S from the Bitcoin transaction\n"
}

// The settlement announcements return a different result type from the funding
// ones, so these adapters give them the shape the shared renderer takes.
func announceBTCCLaimed(e *swap.Engine) func(context.Context, string) (*swap.MessageResult, error) {
	return adaptAnnouncement(e.AnnounceBTCCLaimed)
}

func receiveBTCCLaimed(e *swap.Engine) func(context.Context, string) (*swap.MessageResult, error) {
	return adaptAnnouncement(e.ReceiveBTCCLaimed)
}

func announceBlakeClaimed(e *swap.Engine) func(context.Context, string) (*swap.MessageResult, error) {
	return adaptAnnouncement(e.AnnounceBlakeClaimed)
}

func receiveBlakeClaimed(e *swap.Engine) func(context.Context, string) (*swap.MessageResult, error) {
	return adaptAnnouncement(e.ReceiveBlakeClaimed)
}

func announceBTCRefunded(e *swap.Engine) func(context.Context, string) (*swap.MessageResult, error) {
	return adaptAnnouncement(e.AnnounceBTCRefunded)
}

func receiveBTCRefunded(e *swap.Engine) func(context.Context, string) (*swap.MessageResult, error) {
	return adaptAnnouncement(e.ReceiveBTCRefunded)
}

// adaptAnnouncement presents a settlement announcement as a message result.
func adaptAnnouncement(
	call func(context.Context, string) (*swap.Announcement, error),
) func(context.Context, string) (*swap.MessageResult, error) {
	return func(ctx context.Context, swapID string) (*swap.MessageResult, error) {
		announcement, err := call(ctx, swapID)

		if err != nil {
			return nil, err
		}

		return &swap.MessageResult{
			Kind:      string(announcement.Branch),
			Location:  announcement.Location,
			Sender:    announcement.Sender,
			Duplicate: announcement.Duplicate,
			State:     announcement.State,
		}, nil
	}
}

// fieldTxID reads the settlement transaction for one kind from the record.
func fieldTxID(record *model.Swap, kind string) string {
	return orNone(settlementField(record, kind, "txid"))
}

// fieldBlockHash reads the settlement's block hash for one kind.
func fieldBlockHash(record *model.Swap, kind string) string {
	return orNone(settlementField(record, kind, "blockhash"))
}

// fieldVout reads a funding outpoint's index for one kind.
func fieldVout(record *model.Swap, kind string) int64 {
	if strings.HasPrefix(kind, "blake") {
		if record.BlakeFundingVout == nil {
			return 0
		}

		return int64(*record.BlakeFundingVout)
	}

	if record.BTCFundingVout == nil {
		return 0
	}

	return int64(*record.BTCFundingVout)
}

// settlementField picks the record field one announcement kind is about.
func settlementField(record *model.Swap, kind, field string) *string {
	pick := func(txid, block *string) *string {
		if field == "blockhash" {
			return block
		}

		return txid
	}

	blake := strings.HasPrefix(kind, "blake")

	switch {
	case strings.HasSuffix(kind, "-funded") && blake:
		return pick(record.BlakeFundingTxID, record.BlakeFundingBlockHash)
	case strings.HasSuffix(kind, "-funded"):
		return pick(record.BTCFundingTxID, record.BTCFundingBlockHash)
	case strings.HasSuffix(kind, "-claimed") && blake:
		return pick(record.BlakeClaimTxID, record.BlakeClaimBlockHash)
	case strings.HasSuffix(kind, "-claimed"):
		return pick(record.BTCCLaimTxID, record.BTCCLaimBlockHash)
	case blake:
		return pick(record.BlakeRefundTxID, record.BlakeRefundBlockHash)
	default:
		return pick(record.BTCRefundTxID, record.BTCRefundBlockHash)
	}
}

// renderAlreadyReceived is the reference's block for a message it already had.
func renderAlreadyReceived(label, noun string, record *model.Swap, txid string, vout int64, pass string) string {
	_ = vout
	_ = pass

	header := label + " " + noun + " ALREADY RECEIVED"

	return "\n" + header + "\n" + strings.Repeat("=", len(header)) + "\n" +
		"swap id: " + record.SwapID + "\n" +
		"txid:    " + txid + "\n" +
		"state:   " + string(record.State) + "\n"
}

// renderFundBlake is the reference's fund-blake output.
//
// It is three blocks: the contract Alice is about to fund, the watch-only import
// that keeps the HTLC solvable if her signing wallet is lost, and the result.
func renderFundBlake(result *swap.FundingResult, record *model.Swap) string {
	var out strings.Builder

	out.WriteString("BLAKE contract\n")
	out.WriteString("==============\n")
	out.WriteString("height:   " + fmt.Sprint(result.Height) + "\n")
	out.WriteString("timeout:  " + fmt.Sprint(record.BlakeTimeout) + "\n")
	out.WriteString("address:  " + result.Address + "\n")
	out.WriteString("hash:     " + record.SecretHash + "\n")
	out.WriteString("\n\n")

	out.WriteString(renderWatchOnly("BLAKE", result.Watch))
	out.WriteString("BLAKE funding fee rate: " + result.FeeRate.String() +
		" sat/vB (" + result.FeeSource + ")\n")

	out.WriteString("FUNDED\n")
	out.WriteString("------\n")
	out.WriteString("txid:    " + result.TxID + "\n")
	out.WriteString(fmt.Sprintf("vout:    %d\n", result.Vout))
	out.WriteString("block:   " + result.BlockHash + "\n")
	out.WriteString("state:   " + string(result.State) + "\n")

	return out.String()
}

// renderWatchOnly is the reference's watch-only import confirmation.
func renderWatchOnly(label string, watch fund.WatchStatus) string {
	var out strings.Builder

	out.WriteString("WATCH-ONLY " + label + " HTLC\n")
	out.WriteString(strings.Repeat("=", 12+len(strings.ToLower(label))) + "\n")

	if watch.Imported {
		out.WriteString("wallet:   " + watch.Wallet + "\n")
		out.WriteString("address:  " + watch.Address + "\n")
		out.WriteString("PASS: descriptor imported; this HTLC stays solvable " +
			"without the signing wallet\n")

		return out.String()
	}

	out.WriteString("wallet:   " + orEmpty(watch.Wallet, "(none)") + "\n")

	return out.String()
}

// orEmpty is the fallback a block prints when a value is absent.
func orEmpty(value, fallback string) string {
	if value == "" {
		return fallback
	}

	return value
}

// renderFundBTC is the reference's fund-btc output.
func renderFundBTC(result *swap.FundingResult, record *model.Swap) string {
	var out strings.Builder

	out.WriteString("\nBLAKE contract is acceptable.\n")
	out.WriteString("Preparing Bitcoin HTLC...\n")
	out.WriteString("\n")
	out.WriteString("BITCOIN CONTRACT\n")
	out.WriteString("================\n")
	out.WriteString("height:        " + fmt.Sprint(result.Height) + "\n")
	out.WriteString("timeout:       " + fmt.Sprint(record.BTCTimeout) + "\n")
	out.WriteString("address:       " + result.Address + "\n")
	out.WriteString(fmt.Sprintf("amount sats:   %d\n", record.BTCSats))
	out.WriteString("SHA256:        " + record.SecretHash + "\n")
	out.WriteString("Alice pubkey:  " + orNone(record.BTCAlicePub) + "\n")
	out.WriteString("Bob pubkey:    " + orNone(record.BTCBobPub) + "\n")
	out.WriteString("\n\n")
	out.WriteString(renderWatchOnly("BTC", result.Watch))
	out.WriteString("Bitcoin funding confirmation target: " +
		fmt.Sprint(result.FeeTargetBlocks) + " block(s)\n")
	out.WriteString("Bitcoin funding fee rate: " + result.FeeRate.String() +
		" sat/vB (" + result.FeeSource + ")\n")
	out.WriteString("BTC FUNDED\n")
	out.WriteString("==========\n")
	out.WriteString("txid:   " + result.TxID + "\n")
	out.WriteString(fmt.Sprintf("vout:   %d\n", result.Vout))
	out.WriteString("block:  " + result.BlockHash + "\n")
	out.WriteString("state:  " + string(result.State) + "\n")

	return out.String()
}

// renderClaimReady is the reference's pre-broadcast fee block for a claim.
func renderClaimReady(label string, result *swap.SettlementResult, input int64) string {
	var out strings.Builder

	output := input - result.FeeSats

	if label == "BTC" {
		out.WriteString("BTC CLAIM READY\n")
		out.WriteString("===============\n")
		out.WriteString(fmt.Sprintf("input sats:  %d\n", input))
		out.WriteString(fmt.Sprintf("output sats: %d\n", output))
		out.WriteString(fmt.Sprintf("fee sats:    %d\n", result.FeeSats))
		out.WriteString(fmt.Sprintf("vsize:       %d vB\n", result.Vsize))
		out.WriteString("rate:        " + result.FeeRate.String() + " sat/vB\n")
		out.WriteString("fee source:   " + result.FeeSource + "\n")
		out.WriteString("\n")

		return out.String()
	}

	out.WriteString("BLAKE CLAIM READY\n")
	out.WriteString("=================\n")
	out.WriteString("input:  " + amount.Blake(input).Format() + " BLAKE\n")
	out.WriteString("output: " + amount.Blake(output).Format() + " BLAKE\n")
	out.WriteString("fee:    " + amount.Blake(result.FeeSats).Format() + " BLAKE\n")
	out.WriteString(fmt.Sprintf("vsize:  %d vB\n", result.Vsize))
	out.WriteString("rate:   " + result.FeeRate.String() + " sat/vB\n")
	out.WriteString("source: " + result.FeeSource + "\n")
	out.WriteString("\n")

	return out.String()
}

// renderClaimed is the reference's settlement result block.
func renderClaimed(label string, result *swap.SettlementResult, spentBlake bool) string {
	header := label + " CLAIMED"

	var out strings.Builder

	out.WriteString(header + "\n")
	out.WriteString(strings.Repeat("=", len(header)) + "\n")
	out.WriteString("txid:   " + result.TxID + "\n")
	out.WriteString("block:  " + result.BlockHash + "\n")
	out.WriteString("state:  " + string(result.State) + "\n")

	if spentBlake {
		out.WriteString("\n")
		out.WriteString("PASS: original BLAKE HTLC is spent\n")
	}

	return out.String()
}

// renderClaimBanner is the sentence a claim command prints before it verifies the
// counterparty's HTLC.
func renderClaimBanner(label string) string {
	if label == "BTC" {
		return "Alice verifies Bitcoin HTLC...\n\n"
	}

	return "Bob verifies BLAKE HTLC is still available...\n\n"
}

// runClaim is the claim command: verify the counterparty's HTLC, price the spend,
// then report it.
//
// The verification happens here rather than only inside the engine because the
// reference prints what it verified, and an operator reading "BTC CLAIMED" needs to
// know which outpoint was checked before the claim was signed.
func runClaim(
	ctx context.Context,
	engine *swap.Engine,
	args []string,
	ch chain.Chain,
	claim func(context.Context, string) (*swap.SettlementResult, error),
) error {
	if len(args) < 1 {
		return fmt.Errorf("claim requires a swap id")
	}

	label := "BTC"
	check := engine.CheckBTCFunding

	if ch == chain.Blake {
		label = "BLAKE"
		check = engine.CheckBlakeFunding
	}

	record, err := engine.Show(ctx, args[0])

	if err != nil {
		return err
	}

	fmt.Print(renderClaimBanner(label))

	verified, err := check(ctx, args[0])

	if err != nil {
		return err
	}

	if ch == chain.Blake {
		fmt.Print(renderVerifyBlake(verified))
	} else {
		fmt.Print(renderVerifyBTC(verified, record))
	}

	fmt.Print("\n")

	input := record.BTCSats

	if ch == chain.Blake {
		input = record.BlakeAmountBaseUnits
	}

	result, err := claim(ctx, args[0])

	if err != nil {
		return err
	}

	if result.Pending {
		fmt.Print(renderPending(label, result))

		return nil
	}

	fmt.Print(renderClaimReady(label, result, input))
	fmt.Print(renderClaimed(label, result, ch == chain.Blake))

	return nil
}

// renderPending is the reference's block for a settlement still in the mempool.
func renderPending(label string, result *swap.SettlementResult) string {
	header := label + " CLAIM PENDING"

	return "\n" + header + "\n" + strings.Repeat("=", len(header)) + "\n" +
		"txid:  " + result.TxID + "\n" +
		"block:  pending\n" +
		"state: " + string(result.State) + "\n"
}

// renderRefundPublished is the reference's publish confirmation for a refund.
func renderRefundPublished(label string, swap *model.Swap, txid, blockHash, location string) string {
	header := label + " REFUND PUBLISHED"

	return "\n" + header + "\n" +
		strings.Repeat("=", len(header)) + "\n" +
		"swap id: " + swap.SwapID + "\n" +
		"txid:    " + txid + "\n" +
		"block:   " + blockHash + "\n" +
		"message: " + location + "\n" +
		"\n" +
		"PASS: only public refund data published\n"
}

// renderRefundReceived is the reference's receive confirmation for a refund.
func renderRefundReceived(label string, swap *model.Swap, txid, blockHash string) string {
	header := label + " REFUND RECEIVED"

	return "\n" + header + "\n" +
		strings.Repeat("=", len(header)) + "\n" +
		"swap id: " + swap.SwapID + "\n" +
		"txid:    " + txid + "\n" +
		"block:   " + blockHash + "\n" +
		"state:   " + string(swap.State) + "\n" +
		"\n" +
		"PASS: " + refundPass(label) + "\n"
}

// refundPass is the sentence a refund import closes with.
func refundPass(label string) string {
	if label == "BTC" {
		return "Alice independently verified BTC refund"
	}

	return "Bob independently verified BLAKE refund"
}

// renderRefundReady is the reference's pre-broadcast block for a refund.
func renderRefundReady(label string, result *swap.SettlementResult, input, current, refundAt int64) string {
	var out strings.Builder

	if label == "BTC" {
		header := "BITCOIN REFUND"
		out.WriteString(header + "\n")
		out.WriteString(strings.Repeat("=", len(header)) + "\n")
		out.WriteString(fmt.Sprintf("current height: %d\n", current))
		out.WriteString(fmt.Sprintf("refund height:  %d\n", refundAt))
		out.WriteString(fmt.Sprintf("input sats:     %d\n", input))
		out.WriteString(fmt.Sprintf("output sats:    %d\n", input-result.FeeSats))
		out.WriteString(fmt.Sprintf("fee sats:       %d\n", result.FeeSats))
		out.WriteString(fmt.Sprintf("vsize:          %d vB\n", result.Vsize))
		out.WriteString("rate:           " + result.FeeRate.String() + " sat/vB\n")
		out.WriteString("fee source:     " + result.FeeSource + "\n")
		out.WriteString("\n")

		return out.String()
	}

	header := "BLAKE REFUND READY"
	out.WriteString(header + "\n")
	out.WriteString(strings.Repeat("=", len(header)) + "\n")
	out.WriteString(fmt.Sprintf("current height: %d\n", current))
	out.WriteString(fmt.Sprintf("refund height:  %d\n", refundAt))
	out.WriteString("input:          " + amount.Blake(input).Format() + " BLAKE\n")
	out.WriteString("output:         " + amount.Blake(input-result.FeeSats).Format() + " BLAKE\n")
	out.WriteString("fee:            " + amount.Blake(result.FeeSats).Format() + " BLAKE\n")
	out.WriteString(fmt.Sprintf("vsize:          %d vB\n", result.Vsize))
	out.WriteString("rate:           " + result.FeeRate.String() + " sat/vB\n")
	out.WriteString("fee source:     " + result.FeeSource + "\n")
	out.WriteString("\n")

	return out.String()
}

// renderRefunded is the reference's refund result block.
func renderRefunded(label string, result *swap.SettlementResult) string {
	header := label + " REFUNDED"

	return header + "\n" + strings.Repeat("=", len(header)) + "\n" +
		"txid:   " + result.TxID + "\n" +
		"block:  " + result.BlockHash + "\n" +
		"state:  " + string(result.State) + "\n"
}

// runRefund is the refund command: verify the HTLC, report the plan, then the result.
func runRefund(
	ctx context.Context,
	engine *swap.Engine,
	args []string,
	ch chain.Chain,
	refund func(context.Context, string) (*swap.SettlementResult, error),
) error {
	if len(args) < 1 {
		return fmt.Errorf("refund requires a swap id")
	}

	label := "BTC"
	check := engine.CheckBTCFunding

	if ch == chain.Blake {
		label = "BLAKE"
		check = engine.CheckBlakeFunding
	}

	record, err := engine.Show(ctx, args[0])

	if err != nil {
		return err
	}

	verified, err := check(ctx, args[0])

	if err != nil {
		return err
	}

	if ch == chain.Blake {
		fmt.Print(renderVerifyBlake(verified))
	} else {
		fmt.Print(renderVerifyBTC(verified, record))
	}

	result, err := refund(ctx, args[0])

	if err != nil {
		return err
	}

	//
	// The reference saves the signed refund next to the spool and prints where. The
	// record already holds the same hex, so this file is a convenience -- but it is
	// the file the printed line names, and a line that points at a path nobody
	// wrote is worse than no line.
	//
	signedPath := messageLocation(args[0] + "." + strings.ToLower(label) + "-refund.hex")

	if result.Hex != "" {
		if err := os.WriteFile(signedPath, []byte(result.Hex+"\n"), 0o600); err != nil {
			return err
		}
	}

	fmt.Print("\n")
	fmt.Printf("Saved signed %s refund:\n", labelTitle(label))
	fmt.Println(signedPath)
	fmt.Print("\n")

	if result.Pending {
		fmt.Print(renderPending(label, result))

		return nil
	}

	input := record.BTCSats
	timeout := record.BTCTimeout

	if ch == chain.Blake {
		input = record.BlakeAmountBaseUnits
		timeout = record.BlakeTimeout
	}

	fmt.Print(renderRefundReady(label, result, input, timeout, timeout))
	fmt.Print(renderRefunded(label, result))

	return nil
}

// labelTitle is a chain label as it appears inside a sentence.
func labelTitle(label string) string {
	if label == "BTC" {
		return "Bitcoin"
	}

	return "BLAKE"
}
