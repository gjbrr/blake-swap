package fees

import (
	"fmt"
	"os"
	"strings"

	"github.com/gjbrr/blake-swap/pkg/decimal"
)

// The environment variables that override the settlement fee.
//
// A client with its own view of the fee market should be able to say what to pay: it
// may read rates from somewhere this engine does not, and it knows how urgently a
// particular swap needs to settle. The engine's own estimator is a default, not a
// policy, so the knob exists — and it is per chain, because steering the BLAKE leg
// from the BTC knob would be a mistake that looks like a working configuration.
const (
	// EnvBTCSpendFeeSats carries an exact fee, in satoshis, for the BTC leg.
	EnvBTCSpendFeeSats = "BLAKE_SWAP_BTC_SPEND_FEE_SATS"
	// EnvBTCSpendFeeRate carries a feerate, in sat/vB, for the BTC leg.
	EnvBTCSpendFeeRate = "BLAKE_SWAP_BTC_SPEND_FEE_RATE"
	// EnvBlakeSpendFeeSats and EnvBlakeSpendFeeRate are the same for the BLAKE leg.
	EnvBlakeSpendFeeSats = "BLAKE_SWAP_BLAKE_SPEND_FEE_SATS"
	EnvBlakeSpendFeeRate = "BLAKE_SWAP_BLAKE_SPEND_FEE_RATE"
)

// Override is an operator's explicit settlement fee.
//
// Exactly one of Rate and Sats is set. Rate is a feerate in sat/vB; Sats is an exact
// fee, which wins over a rate because it is the more specific instruction.
type Override struct {
	// Rate is the feerate in sat/vB, when the override named one.
	Rate *decimal.Dec
	// Sats is the exact fee in satoshis, when the override named one.
	Sats *int64
	// Source is the environment variable that supplied the value, so an error or a
	// report can name the knob rather than a number.
	Source string
}

// Set reports whether an override was supplied.
func (o Override) Set() bool { return o.Sats != nil || o.Rate != nil }

// OverrideFromEnv reads the per-chain settlement-fee override.
//
// Precedence is exact fee, then feerate, then nothing. Both names are read for the
// chain named, so setting the BTC variables cannot change what the BLAKE leg pays.
func OverrideFromEnv(chainName string) (Override, error) {
	satsName, rateName, err := envNames(chainName)

	if err != nil {
		return Override{}, err
	}

	if raw := strings.TrimSpace(os.Getenv(satsName)); raw != "" {
		sats, err := parseSats(raw, satsName)

		if err != nil {
			return Override{}, err
		}

		return Override{Sats: &sats, Source: satsName}, nil
	}

	if raw := strings.TrimSpace(os.Getenv(rateName)); raw != "" {
		rate, err := ParseRate(raw, rateName)

		if err != nil {
			return Override{}, err
		}

		return Override{Rate: &rate, Source: rateName}, nil
	}

	return Override{}, nil
}

// envNames maps a chain to its two override variables.
func envNames(chainName string) (satsName, rateName string, err error) {
	switch chainName {
	case "btc", "bitcoin":
		return EnvBTCSpendFeeSats, EnvBTCSpendFeeRate, nil
	case "blake":
		return EnvBlakeSpendFeeSats, EnvBlakeSpendFeeRate, nil
	default:
		return "", "", fmt.Errorf("unknown settlement-fee chain: %q", chainName)
	}
}

// parseSats validates an exact-fee override.
func parseSats(raw, label string) (int64, error) {
	var value int64

	if _, err := fmt.Sscanf(raw, "%d", &value); err != nil {
		return 0, fmt.Errorf("%s must be a whole number of satoshis, got %q", label, raw)
	}

	if value < 1 {
		return 0, fmt.Errorf("%s must be >= 1", label)
	}

	return value, nil
}

// ParseRate reads a feerate in sat/vB.
//
// The label names where the value arrived, so a refusal tells the operator which knob
// to fix rather than showing a bare number.
func ParseRate(raw, label string) (decimal.Dec, error) {
	rate, err := decimal.Parse(strings.TrimSpace(raw))

	if err != nil {
		return decimal.Dec{}, fmt.Errorf("%s is not a decimal feerate: %q", label, raw)
	}

	if rate.Sign() <= 0 {
		return decimal.Dec{}, fmt.Errorf("%s must be > 0", label)
	}

	return rate, nil
}

// Floor is the highest feerate a node will currently accept.
//
// It exists so an override can be refused where it is written rather than at the
// broadcast that would fail minutes later: both floors are checked, because
// mempoolminfee rises above relay policy when the mempool is congested and a
// transaction below it is rejected just as surely, if only for as long as the
// congestion lasts.
type Floor struct {
	// Rate is the floor in sat/vB.
	Rate decimal.Dec
	// Field names the node field the floor came from.
	Field string
}

// NodeFloor reports the highest enforced floor in a node's mempool data.
func NodeFloor(data FeeData) (Floor, bool) {
	var (
		best  decimal.Dec
		field string
		found bool
	)

	consider := func(value any, name string) {
		rate, err := CoinPerKvbToSatVb(value)

		if err != nil {
			return
		}

		if !found || rate.Cmp(best) > 0 {
			best = rate
			field = name
			found = true
		}
	}

	consider(data.MempoolMinFee, "mempoolminfee")
	consider(data.MinRelayTxFee, "minrelaytxfee")

	return Floor{Rate: best, Field: field}, found
}
