package swap

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/gjbrr/blake-swap/pkg/decimal"
	"github.com/gjbrr/blake-swap/pkg/fees"
)

// The defaults and limits the reference uses when sizing a market trade.
const (
	// DefaultSettlementVsize is the conservative estimate of a settlement spend's
	// size, in vbytes. A market offer's BTC leg has to be large enough that
	// settling it is economically possible at the current feerate, and the
	// reference has always estimated that at 200 vB unless told otherwise.
	DefaultSettlementVsize = 200
	// DefaultFeeRateMultiplier is the default margin over the node's estimate.
	DefaultFeeRateMultiplier = "1.0"
)

// BTCTradeMinimumSats is the smallest BTC leg that is worth advertising.
//
// A market offer is a promise that somebody can settle it. Below the cost of that
// settlement the BTC leg is dust in practice: it can be claimed, but the claim costs
// more than it moves, so the advertisement would be one nobody could profitably take.
// The reference derives the floor from the node's current conditions rather than a
// constant, and this follows it: an exact-fee override wins, then a feerate override,
// then the node's estimator and policy floors.
func (e *Engine) BTCTradeMinimumSats(ctx context.Context) (int64, error) {
	override, err := fees.OverrideFromEnv("btc")

	if err != nil {
		return 0, err
	}

	if override.Sats != nil {
		// An exact fee is a statement about the settlement, and the minimum is one
		// satoshi above it so a settlement can always pay.
		return *override.Sats + 1, nil
	}

	vsize, err := settlementVsize()

	if err != nil {
		return 0, err
	}

	rate := decimal.MustParse("1")

	if override.Rate != nil {
		rate = *override.Rate
	} else {
		rate, err = e.settlementRate(ctx)

		if err != nil {
			return 0, err
		}
	}

	fee, err := fees.FeeForVsize(vsize, rate)

	if err != nil {
		return 0, err
	}

	return fee + 1, nil
}

// settlementRate is the node's current settlement feerate, with the configured margin.
func (e *Engine) settlementRate(ctx context.Context) (decimal.Dec, error) {
	if e.BTC == nil {
		return decimal.Dec{}, fmt.Errorf("no Bitcoin node configured")
	}

	estimate, err := e.BTC.EstimateSmartFee(ctx, 1)

	if err != nil {
		return decimal.Dec{}, err
	}

	mempool, err := e.BTC.MempoolInfo(ctx)

	if err != nil {
		return decimal.Dec{}, err
	}

	multiplier, err := feeRateMultiplier()

	if err != nil {
		return decimal.Dec{}, err
	}

	return fees.ChooseRate(fees.FeeData{
		Feerate:       estimate.Feerate,
		MempoolMinFee: mempool.MempoolMinFee,
		MinRelayTxFee: mempool.MinRelayTxFee,
	}, multiplier)
}

// settlementVsize is the size a settlement spend is assumed to have.
func settlementVsize() (int, error) {
	raw := strings.TrimSpace(os.Getenv("BLAKE_SWAP_BTC_SETTLEMENT_VSIZE_ESTIMATE"))

	if raw == "" {
		return DefaultSettlementVsize, nil
	}

	vsize, err := strconv.Atoi(raw)

	if err != nil {
		return 0, fmt.Errorf(
			"BLAKE_SWAP_BTC_SETTLEMENT_VSIZE_ESTIMATE must be a whole number, got %q", raw,
		)
	}

	if vsize < 1 {
		return 0, fmt.Errorf("BLAKE_SWAP_BTC_SETTLEMENT_VSIZE_ESTIMATE must be >= 1")
	}

	return vsize, nil
}

// feeRateMultiplier is the configured margin over the node's estimate.
func feeRateMultiplier() (*decimal.Dec, error) {
	raw := strings.TrimSpace(os.Getenv("BLAKE_SWAP_FEE_RATE_MULTIPLIER"))

	if raw == "" {
		return nil, nil
	}

	multiplier, err := decimal.Parse(raw)

	if err != nil {
		return nil, fmt.Errorf("BLAKE_SWAP_FEE_RATE_MULTIPLIER is not a number: %q", raw)
	}

	if multiplier.Cmp(fees.One) < 0 {
		return nil, fmt.Errorf("BLAKE_SWAP_FEE_RATE_MULTIPLIER must be >= 1.0")
	}

	return &multiplier, nil
}
