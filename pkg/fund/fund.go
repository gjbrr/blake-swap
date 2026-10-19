// Package fund moves one leg of a swap onto its chain.
//
// The order of operations is the whole design, and it is not the obvious one.
// Before any money moves, the HTLC is imported into a watch-only wallet and a
// durable intent is written; the intent is what makes a funding resumable after
// a crash between "the node signed a transaction" and "the swap record knows
// about it". Skipping either step produces a failure mode this package exists
// to prevent: a funded contract that nothing on this machine can find, or a
// second funding transaction for a contract that is already paid.
//
// Nothing here mutates the swap. Fund returns what happened; persisting the
// outpoint, advancing the state and clearing the intent is the caller's job,
// in that order.
package fund

import (
	"context"
	"errors"
	"fmt"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/contract"
	"github.com/gjbrr/blake-swap/pkg/decimal"
	"github.com/gjbrr/blake-swap/pkg/fees"
	"github.com/gjbrr/blake-swap/pkg/model"
)

// DefaultFeeTargetBlocks is the confirmation target funding asks for when the
// caller does not choose one.
//
// One block, which is the reference's default for funding: a funding transaction
// is the commitment a swap depends on, so it is priced to confirm in the next block
// rather than to wait. Settlement has its own, slower, target.
const DefaultFeeTargetBlocks int64 = 1

// Node is the chain surface funding needs. It is an interface so the whole
// flow can be exercised against a node whose answers the test controls,
// including the ones a real node only produces during an incident.
type Node interface {
	Bind(wallet string) chain.Wallet
	Height(ctx context.Context) (int64, error)
	ListWallets(ctx context.Context) ([]string, error)
	EnsureWallet(ctx context.Context, wallet string, watchOnly bool) error
	DescriptorInfo(ctx context.Context, body string) (*chain.DescriptorInfo, error)
	DeriveAddresses(ctx context.Context, descriptor string) ([]string, error)
	TxOut(ctx context.Context, txid string, vout int64) (*chain.TxOut, error)
	RawTx(ctx context.Context, txid, blockHash string) (*chain.Tx, error)
	DecodeRaw(ctx context.Context, hexTx string) (*chain.Tx, error)
	EstimateSmartFee(ctx context.Context, confTarget int64) (*chain.FeeEstimate, error)
	MempoolInfo(ctx context.Context) (*chain.MempoolInfo, error)
	// MineToAddress exists for the regtest rig; nothing in a real deployment
	// calls it.
	MineToAddress(ctx context.Context, blocks int64, address string) ([]string, error)
}

// Options is everything funding needs beyond the swap and the node.
type Options struct {
	// Wallet is the signing wallet that pays for this leg.
	Wallet string
	// WatchWallet overrides the watch-only wallet name. Empty means the
	// default for the chain, which the CLI's environment can also override.
	WatchWallet string
	// SkipWatchOnly disables the pre-funding watch-only import. It exists for
	// tests that exercise a node refusing the import; normal operation never
	// wants it.
	SkipWatchOnly bool
	// FeeRate is an explicit feerate in sat/vB. It outranks the estimator,
	// because the client driving this swap knows what it wants to pay and the
	// node's estimate is only a default.
	FeeRate *decimal.Dec
	// FeeTargetBlocks is the confirmation target for the estimator.
	FeeTargetBlocks int64
	// FeeMultiplier scales the estimator's answer. Below 1.0 is refused by the
	// fee policy: paying less than the estimate is a decision for FeeRate.
	FeeMultiplier *decimal.Dec
	// MinFeeRate is a wallet-construction floor. Bitcoin Core's wallet
	// requires 1 sat/vB for its own transactions; BLAKE does not.
	MinFeeRate *decimal.Dec
	// AutoMine mines this many blocks after broadcasting. It exists for the
	// regtest rig, where waiting for a block would wait forever.
	AutoMine int64
	// Intents is the durable funding-intent store. It is required: the intent
	// is the crash window's only cover.
	Intents Store
}

// Receipt is what happened.
type Receipt struct {
	Chain         chain.Chain
	Descriptor    string
	Address       string
	Amount        int64
	FeeRate       decimal.Dec
	FeeSource     string
	Intent        *Intent
	TxID          string
	Vout          int64
	BlockHash     string
	Confirmations int64
	Pending       bool
	Recovered     bool
	Mined         int64
	Watch         WatchStatus
}

// Fund puts one leg's HTLC on the chain.
//
// It is safe to call more than once: a crash before the outpoint was recorded
// leaves a payment this wallet already made to the exact contract address and
// amount, and Fund adopts that payment rather than sending a second one.
func Fund(
	ctx context.Context,
	node Node,
	swap *model.Swap,
	ch chain.Chain,
	opts Options,
) (*Receipt, error) {
	if opts.Intents == nil {
		return nil, fmt.Errorf("funding requires an intent store")
	}

	if swap == nil {
		return nil, fmt.Errorf("swap is nil")
	}

	if opts.Wallet == "" {
		return nil, fmt.Errorf("funding requires a signing wallet")
	}

	timeout := swap.BTCTimeout

	if ch == chain.Blake {
		timeout = swap.BlakeTimeout
	}

	height, err := node.Height(ctx)

	if err != nil {
		return nil, err
	}

	if height >= timeout {
		return nil, fmt.Errorf(
			"%s timeout %d has already been reached; current height=%d",
			chainName(ch), timeout, height,
		)
	}

	derived, err := contract.Derive(ctx, node, swap, ch)

	if err != nil {
		return nil, err
	}

	receipt := &Receipt{
		Chain:      ch,
		Descriptor: derived.Descriptor,
		Address:    derived.Address,
		Amount:     derived.Amount,
	}

	//
	// Recovery first, and deliberately before the balance preflight: an HTLC
	// this wallet already funded must stay recoverable even if the wallet's
	// present balance is now below the amount.
	//
	payment, err := FindPayment(ctx, node.Bind(opts.Wallet), derived.Address, derived.Amount)

	if err != nil {
		return nil, err
	}

	if payment != nil {
		receipt.Recovered = true
		receipt.TxID = payment.TxID
		receipt.Vout = payment.Vout
		receipt.BlockHash = payment.BlockHash
	} else {
		if ch == chain.Blake {
			if err := preflightBalance(ctx, node, opts.Wallet, derived); err != nil {
				return nil, err
			}
		}

		//
		// Record the HTLC as watch-only before any funds move, so a later
		// loss of the signing wallet does not leave a funded swap that cannot
		// even be observed. A failure is reported, not fatal: the keys are
		// present right now, so the swap is spendable, and refusing to fund
		// because a *watch-only* copy could not be written would break the
		// case it exists to protect.
		//
		if !opts.SkipWatchOnly {
			receipt.Watch = ImportWatchOnly(ctx, node, swap, ch, derived, watchWallet(opts, ch))
		}

		//
		// The intent is written once the decision to move funds is final and
		// before the node can move them: a refused funding leaves no marker
		// behind, and a funding that the node rejected leaves one that says
		// exactly which contract was attempted.
		//
		intent, err := WriteIntent(ctx, opts.Intents, swap, ch, derived.Address)

		if err != nil {
			return nil, err
		}

		receipt.Intent = intent

		rate, source, err := feeRate(ctx, node, ch, opts)

		if err != nil {
			return nil, err
		}

		receipt.FeeRate = rate
		receipt.FeeSource = source

		txid, err := node.Bind(opts.Wallet).SendToAddress(
			ctx, derived.Address, amount.Sats(derived.Amount), &rate,
		)

		if err != nil {
			return nil, fmt.Errorf("%s funding broadcast: %w", chainName(ch), err)
		}

		vout, blockHash, err := LocatePaymentOutput(
			ctx, node.Bind(opts.Wallet), txid, derived.Address, derived.Amount,
		)

		if err != nil {
			return nil, err
		}

		receipt.TxID = txid
		receipt.Vout = vout
		receipt.BlockHash = blockHash
	}

	//
	// The broadcast identity is recorded whether the payment is fresh or
	// recovered. A recovered payment with no intent is refused: the intent is
	// the only proof that this participant started the funding, and adopting a
	// payment without it would hide a restored or replaced wallet.
	//
	if err := RecordBroadcast(ctx, opts.Intents, swap.SwapID, ch, receipt.TxID, receipt.Vout); err != nil {
		return nil, err
	}

	if receipt.BlockHash == "" && opts.AutoMine > 0 {
		receipt.Mined, err = mine(ctx, node, opts.AutoMine, opts.Wallet)

		if err != nil {
			return nil, err
		}

		walletTx, err := node.Bind(opts.Wallet).Tx(ctx, receipt.TxID)

		if err != nil {
			return nil, err
		}

		receipt.BlockHash = walletTx.BlockHash
	}

	if receipt.BlockHash == "" {
		receipt.Pending = true

		return receipt, nil
	}

	anchor, err := VerifyAnchor(ctx, node, swap, ch, receipt.TxID, receipt.Vout, receipt.BlockHash)

	if err != nil {
		return nil, err
	}

	receipt.Confirmations = anchor.Confirmations

	return receipt, nil
}

// preflightBalance refuses to start a funding whose wallet cannot pay for it.
//
// The comparison is in base units, exactly: a payment one base unit short is a
// different contract, and the node would either refuse it or fund it with an
// extra input the swap did not anticipate.
func preflightBalance(ctx context.Context, node Node, wallet string, derived *contract.Contract) error {
	balances, err := node.Bind(wallet).Balances(ctx)

	if err != nil {
		return err
	}

	trusted, err := balances.TrustedAmount()

	if err != nil {
		return err
	}

	required := amount.Blake(derived.Amount).Decimal()

	if trusted.Cmp(required) < 0 {
		return fmt.Errorf(
			"Insufficient trusted %s balance for HTLC funding: trusted=%s required=%s",
			chainName(derived.Chain), trusted, required,
		)
	}

	return nil
}

// feeRate chooses the funding feerate and says where it came from.
//
// Funding deliberately does not use the settlement-only overrides: it derives
// a rate from the node's estimator and the current policy floors, then applies
// any wallet-construction floor the chain requires. An explicit rate outranks
// both, because the party driving this swap is the one that knows what it wants
// to pay.
func feeRate(
	ctx context.Context, node Node, ch chain.Chain, opts Options,
) (decimal.Dec, string, error) {
	if opts.FeeRate != nil {
		if opts.FeeRate.Sign() <= 0 {
			return decimal.Dec{}, "", fmt.Errorf("fee rate must be positive")
		}

		return *opts.FeeRate, "explicit", nil
	}

	// An environment override is the host's default; the caller's explicit rate
	// already won above. The node is still asked for its floors, because a rate it
	// will not accept is a funding that stalls and a refusal here can name the knob.
	if override, err := fees.OverrideFromEnv(string(ch)); err != nil {
		return decimal.Dec{}, "", err
	} else if override.Rate != nil {
		mempool, err := node.MempoolInfo(ctx)

		if err != nil {
			return decimal.Dec{}, "", err
		}

		if floor, found := fees.NodeFloor(mempoolFeeData(mempool)); found &&
			override.Rate.Cmp(floor.Rate) < 0 {
			return decimal.Dec{}, "", fmt.Errorf(
				"%s is %s sat/vB, below the node's %s sat/vB %s; a transaction paying it "+
					"would not be accepted. Raise the rate, or unset %s to use the node's estimator",
				override.Source, override.Rate.String(), floor.Rate.String(), floor.Field,
				override.Source,
			)
		}

		return *override.Rate, "rate-override", nil
	}

	target := opts.FeeTargetBlocks

	if target <= 0 {
		target = DefaultFeeTargetBlocks
	}

	estimate, err := node.EstimateSmartFee(ctx, target)

	if err != nil {
		return decimal.Dec{}, "", err
	}

	mempool, err := node.MempoolInfo(ctx)

	if err != nil {
		return decimal.Dec{}, "", err
	}

	data := fees.FeeData{
		Feerate:       estimate.Feerate,
		MempoolMinFee: mempool.MempoolMinFee,
		MinRelayTxFee: mempool.MinRelayTxFee,
	}

	rate, err := fees.ChooseRate(data, opts.FeeMultiplier)

	if err != nil {
		return decimal.Dec{}, "", err
	}

	source := "dynamic"

	if ch == chain.BTC {
		//
		// Bitcoin Core's wallet refuses to build a transaction below its own
		// -mintxfee, which defaults to 1 sat/vB. Leaving that to a broadcast
		// failure would report a fee-policy refusal as a funding failure, so the
		// floor is applied here and named when it is what decided the rate.
		//
		floor := decimal.MustParse("1")

		if opts.MinFeeRate != nil {
			floor = *opts.MinFeeRate
		}

		if rate.Cmp(floor) < 0 {
			rate = floor
			source = "wallet-minimum"
		}
	}

	return rate, source, nil
}

// mempoolFeeData keeps the two node fields the fee floor is derived from.
func mempoolFeeData(mempool *chain.MempoolInfo) fees.FeeData {
	return fees.FeeData{
		MempoolMinFee: mempool.MempoolMinFee,
		MinRelayTxFee: mempool.MinRelayTxFee,
	}
}

// mine mines blocks to the wallet's own address.
//
// The address comes from the wallet because a rig block has to be claimable by
// the participant that will later spend its coinbase.
func mine(ctx context.Context, node Node, blocks int64, wallet string) (int64, error) {
	bound := node.Bind(wallet)

	address, err := bound.NewAddress(ctx)

	if err != nil {
		return 0, err
	}

	if _, err := node.MineToAddress(ctx, blocks, address); err != nil {
		return 0, err
	}

	return blocks, nil
}

// chainName is the chain's name as the operator-facing messages spell it.
func chainName(ch chain.Chain) string { return contract.Name(ch) }

// ErrNoIntent reports that no funding was ever started for a swap and chain.
var ErrNoIntent = errors.New("no funding intent")
