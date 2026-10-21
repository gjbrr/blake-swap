// Package settle builds, prices and broadcasts the transaction that spends one
// leg's HTLC.
//
// The hard part is not the transaction, it is the fee. A settlement pays one
// input and one output, and the amount left for the fee is whatever the input
// holds minus what the output pays — so the fee is not chosen independently of
// the transaction, it *is* the transaction's shape. Changing the fee can change
// the vsize (a change output crosses the dust threshold and disappears), which
// changes the fee the transaction requires. The builder therefore measures the
// real, finalized transaction and iterates, and it accepts a fee that covers the
// requirement rather than insisting on equality: the two-cycle around a dust
// boundary never reaches equality, and treating that as failure turned a
// perfectly good settlement into a happy-path error.
package settle

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/decimal"
	"github.com/gjbrr/blake-swap/pkg/fees"
	"github.com/gjbrr/blake-swap/pkg/psbt"
)

// MaxAttempts bounds the fee/vsize iteration. It is a bound on pathological
// behaviour, not an expected count: a normal settlement converges in one or two
// attempts.
const MaxAttempts = 8

// Node is the node surface settlement needs.
type Node interface {
	Bind(wallet string) chain.Wallet
	CreateRawTransaction(ctx context.Context, inputs []chain.Outpoint, outputs map[string]decimal.Dec) (string, error)
	CreateRawTransactionAt(ctx context.Context, inputs []chain.Outpoint, outputs map[string]decimal.Dec, lockTime int64) (string, error)
	ConvertToPSBT(ctx context.Context, hexTx string) (string, error)
	UtxoUpdatePSBT(ctx context.Context, psbt string, descriptors []string) (string, error)
	FinalizePSBT(ctx context.Context, psbt string) (*chain.FinalizeResult, error)
	SendRaw(ctx context.Context, hexTx string) (string, error)
	DecodeRaw(ctx context.Context, hexTx string) (*chain.Tx, error)
	EstimateSmartFee(ctx context.Context, confTarget int64) (*chain.FeeEstimate, error)
	MempoolInfo(ctx context.Context) (*chain.MempoolInfo, error)
}

// Options is the fee policy for one settlement.
type Options struct {
	// FeeRate is the explicit per-invocation rate in sat/vB. It outranks every
	// other knob: a parameter is this invocation's instruction, and the
	// environment is the host's standing default.
	FeeRate *decimal.Dec
	// FeeSats is an exact fee, the equivalent of the environment's
	// `*_SPEND_FEE_SATS`. It is ignored when FeeRate is set, and the plan says
	// so rather than silently paying something else.
	FeeSats *int64
	// Multiplier scales the estimator. Below 1.0 is refused by the fee policy.
	Multiplier *decimal.Dec
	// MaxPercent and MaxSats bound what a settlement may pay.
	MaxPercent decimal.Dec
	MaxSats    *int64
	// FeeTargetBlocks is the estimator's confirmation target.
	FeeTargetBlocks int64
	// Wallet is the signing wallet.
	Wallet string
	// Sighash is passed to the wallet; empty means ALL.
	Sighash string
}

// DefaultFeeTargetBlocks is the confirmation target settlement asks for.
const DefaultFeeTargetBlocks int64 = 2

// Plan is a finalized settlement and the fee it pays.
type Plan struct {
	Chain      chain.Chain
	Hex        string
	FeeSats    int64
	Vsize      int64
	Rate       decimal.Dec
	Source     string
	OutputSats int64
	// Note carries a message the operator should see, such as an environment
	// override that a parameter passed over.
	Note string
}

// Finalizer builds a fully finalized transaction paying `feeSats`.
//
// It must not broadcast. It is a function rather than a set of parameters
// because the builder has to be able to rebuild the transaction several times
// with different fees, and every rebuild goes through the same pipeline.
type Finalizer func(feeSats int64) (string, error)

// Build prices a settlement by measuring real finalized transactions.
func Build(
	ctx context.Context, node Node, ch chain.Chain, inputSats int64,
	finalize Finalizer, opts Options,
) (*Plan, error) {
	if inputSats < 1 {
		return nil, fmt.Errorf("input amount must be >= 1 sat-unit")
	}

	cap := fees.Cap{MaxPercent: opts.MaxPercent, AbsoluteSats: opts.MaxSats}

	note := ""

	//
	// An explicit rate outranks the exact-satoshi override, and the override is
	// not consulted at all. Saying so matters: an operator who set it as an
	// emergency needs to know it was passed over, and a silently ignored
	// override is worse than no override at all.
	//
	if opts.FeeRate != nil {
		if opts.FeeSats != nil {
			note = fmt.Sprintf(
				"the exact fee of %d sats is overridden by --fee-rate %s sat/vB",
				*opts.FeeSats, opts.FeeRate,
			)
		}
	} else if opts.FeeSats != nil {
		fee := *opts.FeeSats

		if _, err := fees.EnforceCap(fee, inputSats, cap); err != nil {
			return nil, err
		}

		hexTx, err := finalize(fee)

		if err != nil {
			return nil, err
		}

		vsize, err := Vsize(ctx, node, hexTx)

		if err != nil {
			return nil, err
		}

		rate, err := decimal.FromInt64(fee).Quo(decimal.FromInt64(vsize), 18, decimal.HalfEven)

		if err != nil {
			return nil, err
		}

		return &Plan{
			Chain:      ch,
			Hex:        hexTx,
			FeeSats:    fee,
			Vsize:      vsize,
			Rate:       rate,
			Source:     "override",
			OutputSats: inputSats - fee,
		}, nil
	}

	rate, source, err := settlementRate(ctx, node, ch, opts)

	if err != nil {
		return nil, err
	}

	//
	// Start deliberately small. The provisional transaction is never
	// broadcast; it exists only to measure the real witness.
	//
	feeSats := int64(1)

	for attempt := 0; attempt < MaxAttempts; attempt++ {
		if _, err := fees.EnforceCap(feeSats, inputSats, cap); err != nil {
			return nil, err
		}

		hexTx, err := finalize(feeSats)

		if err != nil {
			return nil, err
		}

		vsize, err := Vsize(ctx, node, hexTx)

		if err != nil {
			return nil, err
		}

		required, err := fees.FeeForVsize(int(vsize), rate)

		if err != nil {
			return nil, err
		}

		measuring := cap
		measuring.Vsize = int(vsize)

		if _, err := fees.EnforceCap(required, inputSats, measuring); err != nil {
			return nil, err
		}

		//
		// Converged, or already over the requirement. The transaction being
		// returned is the one that was measured, so its fee covers its own
		// requirement even when a dust-threshold cycle has just lowered that
		// requirement by a satoshi or two.
		//
		if required <= feeSats {
			return &Plan{
				Chain:      ch,
				Hex:        hexTx,
				FeeSats:    feeSats,
				Vsize:      vsize,
				Rate:       rate,
				Source:     source,
				OutputSats: inputSats - feeSats,
				Note:       note,
			}, nil
		}

		feeSats = required
	}

	return nil, fmt.Errorf(
		"Settlement fee/vsize did not converge after %d signing attempts",
		MaxAttempts,
	)
}

// settlementRate resolves the rate and where it came from.
func settlementRate(
	ctx context.Context, node Node, ch chain.Chain, opts Options,
) (decimal.Dec, string, error) {
	if opts.FeeRate != nil {
		if opts.FeeRate.Sign() <= 0 {
			return decimal.Dec{}, "", fmt.Errorf("fee rate must be positive")
		}

		return *opts.FeeRate, "--fee-rate", nil
	}

	// An environment override is the host's default, and the caller's explicit rate
	// already won above. The node still supplies its floors: paying a rate it will not
	// accept is a settlement that stalls, and a refusal here can name the knob.
	if override, err := fees.OverrideFromEnv(string(ch)); err != nil {
		return decimal.Dec{}, "", err
	} else if override.Rate != nil {
		mempool, err := node.MempoolInfo(ctx)

		if err != nil {
			return decimal.Dec{}, "", err
		}

		if floor, found := fees.NodeFloor(fees.FeeData{
			MempoolMinFee: mempool.MempoolMinFee,
			MinRelayTxFee: mempool.MinRelayTxFee,
		}); found && override.Rate.Cmp(floor.Rate) < 0 {
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

	rate, err := fees.ChooseRate(fees.FeeData{
		Feerate:       estimate.Feerate,
		MempoolMinFee: mempool.MempoolMinFee,
		MinRelayTxFee: mempool.MinRelayTxFee,
	}, opts.Multiplier)

	if err != nil {
		return decimal.Dec{}, "", err
	}

	return rate, "dynamic", nil
}

// Vsize measures a finalized transaction.
func Vsize(ctx context.Context, node Node, hexTx string) (int64, error) {
	decoded, err := node.DecodeRaw(ctx, hexTx)

	if err != nil {
		return 0, err
	}

	if decoded.VSize < 1 {
		return 0, fmt.Errorf("finalized transaction has invalid vsize")
	}

	return decoded.VSize, nil
}

// SpendFee is what a finalized transaction actually pays.
//
// It is measured from the transaction rather than assumed from the request, so a
// builder that rounded an output or dropped a change output cannot hide behind
// its own arithmetic.
func SpendFee(ctx context.Context, node Node, hexTx string, inputSats int64) (int64, error) {
	decoded, err := node.DecodeRaw(ctx, hexTx)

	if err != nil {
		return 0, err
	}

	var outputSats int64

	for _, out := range decoded.Vout {
		value, err := out.Value.Sats()

		if err != nil {
			return 0, fmt.Errorf("output %d has an unrepresentable amount: %w", out.N, err)
		}

		outputSats += int64(value)
	}

	fee := inputSats - outputSats

	if fee < 1 {
		return 0, fmt.Errorf(
			"finalized settlement transaction does not pay a positive fee",
		)
	}

	return fee, nil
}

// Request is one settlement to build.
type Request struct {
	Chain       chain.Chain
	Funding     chain.Outpoint
	Destination string
	Descriptor  string
	InputSats   int64
	// SecretHash and Preimage are required for a claim: the hashlock branch is
	// only spendable with the preimage, and the portfolio of secrets in the
	// durable record is the only place it can come from.
	SecretHash string
	Preimage   string
	// Wallet is the signing wallet.
	Wallet string
	// LockTime is the contract's timelock height, for a refund. A claim needs no
	// locktime; a refund cannot be finalized without one.
	LockTime int64
}

// LocalSpend is a node that can build and finalise a contract spend itself.
//
// A node needs a descriptor to fill in the witness script and amount of a PSBT; a local wallet
// already knows the contract it watched, so it builds the spend from the coin, the script and
// (for a claim) the preimage, and finalises it. When a node offers this, settle uses it instead
// of the CreateRawTransaction / UtxoUpdatePSBT / ProcessPSBT / FinalizePSBT chain.
type LocalSpend interface {
	SpendContract(ctx context.Context, req LocalSpendRequest) (hexTx string, err error)
}

// LocalSpendRequest is one contract spend for a local wallet to build.
type LocalSpendRequest struct {
	Chain       chain.Chain
	Funding     chain.Outpoint
	Destination string
	// Descriptor is the contract the spend is against, as the swap renders it. A local wallet
	// reads the terms out of it rather than asking a node to compile it.
	Descriptor string
	InputSats  int64
	// FeeSats is the fee the plan chose; the wallet subtracts it from InputSats.
	FeeSats int64
	// SecretHash and Preimage are a claim's hashlock terms; a refund carries neither.
	SecretHash string
	Preimage   string
	// LockTime is the refund's absolute locktime.
	LockTime int64
	// Refund selects the timelock branch instead of the hashlock one.
	Refund bool
}

// Claim builds and finalizes a claim of the HTLC.
//
// The preimage is injected between signing and finalizing: the wallet signs the
// hashlock branch knowing only H, and the preimage is the "0x0b" field the
// finalizer needs to build the witness. Using the revealed secret rather than
// any other copy is deliberate — see the caller.
func Claim(ctx context.Context, node Node, req Request, opts Options) (*Plan, error) {
	if req.Preimage == "" {
		return nil, fmt.Errorf("a claim requires the revealed preimage")
	}

	// The contract's claim branch begins with OP_SIZE 32, so a preimage of any other
	// length signs a witness no node will accept: the claim would be built, signed and
	// broadcast, and then rejected. Refusing it here keeps that mistake where it is —
	// the peer's preimage is wrong — instead of turning it into a failed settlement.
	secret, err := hex.DecodeString(req.Preimage)

	if err != nil {
		return nil, fmt.Errorf("the claim preimage is not hex: %w", err)
	}

	if len(secret) != 32 {
		return nil, fmt.Errorf(
			"the claim preimage is %d bytes, and the contract's claim branch checks 32",
			len(secret),
		)
	}

	finalize := func(feeSats int64) (string, error) {
		if feeSats >= req.InputSats {
			return "", fmt.Errorf("%s amount is too small for claim fee", chainName(req.Chain))
		}

		outputSats := req.InputSats - feeSats

		if local, ok := node.(LocalSpend); ok {
			return local.SpendContract(ctx, LocalSpendRequest{
				Chain:       req.Chain,
				Funding:     req.Funding,
				Destination: req.Destination,
				Descriptor:  req.Descriptor,
				InputSats:   req.InputSats,
				FeeSats:     feeSats,
				SecretHash:  req.SecretHash,
				Preimage:    req.Preimage,
			})
		}

		raw, err := node.CreateRawTransaction(
			ctx,
			[]chain.Outpoint{req.Funding},
			map[string]decimal.Dec{
				req.Destination: amount.Sats(outputSats).Coin(),
			},
		)

		if err != nil {
			return "", err
		}

		unsigned, err := node.ConvertToPSBT(ctx, raw)

		if err != nil {
			return "", err
		}

		updated, err := node.UtxoUpdatePSBT(ctx, unsigned, []string{req.Descriptor})

		if err != nil {
			return "", err
		}

		signed, err := node.Bind(req.Wallet).ProcessPSBT(ctx, updated, sighash(opts))

		if err != nil {
			return "", err
		}

		withSecret, err := psbt.AddSHA256Preimage(signed.PSBT, req.SecretHash, req.Preimage, 0)

		if err != nil {
			return "", err
		}

		return finalizeHex(ctx, node, withSecret, "claim")
	}

	return Build(ctx, node, req.Chain, req.InputSats, finalize, opts)
}

// Refund builds and finalizes a refund of the HTLC.
//
// The timelock branch needs no preimage, which is the whole reason it exists:
// the refund is what a participant falls back to when the counterparty never
// reveals the secret.
func Refund(ctx context.Context, node Node, req Request, opts Options) (*Plan, error) {
	finalize := func(feeSats int64) (string, error) {
		if feeSats >= req.InputSats {
			return "", fmt.Errorf("%s amount is too small for refund fee", chainName(req.Chain))
		}

		outputSats := req.InputSats - feeSats

		if local, ok := node.(LocalSpend); ok {
			return local.SpendContract(ctx, LocalSpendRequest{
				Chain:       req.Chain,
				Funding:     req.Funding,
				Destination: req.Destination,
				Descriptor:  req.Descriptor,
				InputSats:   req.InputSats,
				FeeSats:     feeSats,
				LockTime:    req.LockTime,
				Refund:      true,
			})
		}

		raw, err := node.CreateRawTransactionAt(
			ctx,
			[]chain.Outpoint{req.Funding},
			map[string]decimal.Dec{
				req.Destination: amount.Sats(outputSats).Coin(),
			},
			req.LockTime,
		)

		if err != nil {
			return "", err
		}

		unsigned, err := node.ConvertToPSBT(ctx, raw)

		if err != nil {
			return "", err
		}

		updated, err := node.UtxoUpdatePSBT(ctx, unsigned, []string{req.Descriptor})

		if err != nil {
			return "", err
		}

		signed, err := node.Bind(req.Wallet).ProcessPSBT(ctx, updated, sighash(opts))

		if err != nil {
			return "", err
		}

		return finalizeHex(ctx, node, signed.PSBT, "refund")
	}

	return Build(ctx, node, req.Chain, req.InputSats, finalize, opts)
}

// finalizeHex finalizes a PSBT and refuses an incomplete one.
//
// An incomplete PSBT is not a transaction: broadcasting its hex would fail, and
// treating it as success would advance durable state on nothing.
func finalizeHex(ctx context.Context, node Node, signed, branch string) (string, error) {
	finalized, err := node.FinalizePSBT(ctx, signed)

	if err != nil {
		return "", err
	}

	if !finalized.Complete {
		return "", fmt.Errorf("%s PSBT did not finalize", branch)
	}

	if finalized.Hex == "" {
		return "", fmt.Errorf("%s finalization returned no transaction", branch)
	}

	return finalized.Hex, nil
}

// sighash defaults to ALL, which is what both branches sign.
func sighash(opts Options) string {
	if opts.Sighash != "" {
		return opts.Sighash
	}

	return "ALL"
}

// Broadcast sends a finalized settlement and reports the txid and the fee the
// chain will actually charge.
func Broadcast(
	ctx context.Context, node Node, ch chain.Chain, plan *Plan, inputSats int64,
) (string, int64, error) {
	if plan == nil || plan.Hex == "" {
		return "", 0, fmt.Errorf("nothing to broadcast")
	}

	fee, err := SpendFee(ctx, node, plan.Hex, inputSats)

	if err != nil {
		return "", 0, err
	}

	txid, err := node.SendRaw(ctx, plan.Hex)

	if err != nil {
		return "", 0, fmt.Errorf("%s settlement broadcast: %w", chainName(ch), err)
	}

	return txid, fee, nil
}

// chainName is the chain's name as the operator-facing messages spell it.
func chainName(ch chain.Chain) string {
	if ch == chain.Blake {
		return "BLAKE"
	}

	return "Bitcoin"
}
