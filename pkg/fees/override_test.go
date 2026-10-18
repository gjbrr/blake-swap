package fees

import (
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/decimal"
)

// TestOverrideFromEnvReadsTheExactFeeFirst: both knobs set means the exact fee wins,
// because it is the more specific instruction.
func TestOverrideFromEnvReadsTheExactFeeFirst(t *testing.T) {
	t.Setenv(EnvBTCSpendFeeSats, "1234")
	t.Setenv(EnvBTCSpendFeeRate, "9.5")

	override, err := OverrideFromEnv("btc")

	if err != nil {
		t.Fatal(err)
	}

	if !override.Set() || override.Sats == nil || *override.Sats != 1234 {
		t.Fatalf("override = %+v, want an exact fee", override)
	}

	if override.Source != EnvBTCSpendFeeSats {
		t.Fatalf("source = %q", override.Source)
	}
}

// TestOverrideFromEnvReadsTheRate: with only a rate set, the rate is the override.
func TestOverrideFromEnvReadsTheRate(t *testing.T) {
	t.Setenv(EnvBTCSpendFeeRate, "12.5")

	override, err := OverrideFromEnv("btc")

	if err != nil {
		t.Fatal(err)
	}

	if override.Rate == nil || override.Rate.String() != "12.5" {
		t.Fatalf("override = %+v, want rate 12.5", override)
	}

	if override.Source != EnvBTCSpendFeeRate {
		t.Fatalf("source = %q", override.Source)
	}
}

// TestOverrideFromEnvIsPerChain: the BTC knobs must not steer the BLAKE leg.
func TestOverrideFromEnvIsPerChain(t *testing.T) {
	t.Setenv(EnvBTCSpendFeeRate, "50")

	blake, err := OverrideFromEnv("blake")

	if err != nil {
		t.Fatal(err)
	}

	if blake.Set() {
		t.Fatalf("the BLAKE leg read a BTC override: %+v", blake)
	}

	t.Setenv(EnvBlakeSpendFeeRate, "3")

	blake, err = OverrideFromEnv("blake")

	if err != nil {
		t.Fatal(err)
	}

	if blake.Rate == nil || blake.Rate.String() != "3" {
		t.Fatalf("BLAKE override = %+v", blake)
	}
}

// TestOverrideFromEnvRefusesBadValues: a knob that cannot be used is an error naming
// the knob, not a silent fallback to the estimator.
func TestOverrideFromEnvRefusesBadValues(t *testing.T) {
	cases := map[string]string{
		EnvBTCSpendFeeSats: "0",
		EnvBTCSpendFeeRate: "0",
	}

	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, value)

			override, err := OverrideFromEnv("btc")

			if err == nil {
				t.Fatalf("%s=%q was accepted: %+v", name, value, override)
			}

			if !strings.Contains(err.Error(), name) {
				t.Fatalf("error does not name the knob: %v", err)
			}
		})
	}

	t.Run("not a number", func(t *testing.T) {
		t.Setenv(EnvBTCSpendFeeSats, "plenty")

		if _, err := OverrideFromEnv("btc"); err == nil {
			t.Fatal("a non-numeric exact fee was accepted")
		}
	})

	t.Run("not a feerate", func(t *testing.T) {
		t.Setenv(EnvBTCSpendFeeRate, "fast")

		if _, err := OverrideFromEnv("btc"); err == nil {
			t.Fatal("a non-numeric feerate was accepted")
		}
	})
}

// TestOverrideFromEnvRefusesAnUnknownChain: a typo in the chain name must not read
// somebody else's knob.
func TestOverrideFromEnvRefusesAnUnknownChain(t *testing.T) {
	if _, err := OverrideFromEnv("litecoin"); err == nil {
		t.Fatal("an unknown chain was accepted")
	}
}

// TestOverrideUnsetIsEmpty: no variables means no override, which is the default.
func TestOverrideUnsetIsEmpty(t *testing.T) {
	override, err := OverrideFromEnv("btc")

	if err != nil {
		t.Fatal(err)
	}

	if override.Set() {
		t.Fatalf("override = %+v, want none", override)
	}
}

// TestNodeFloorPicksTheHighest: mempool congestion raises mempoolminfee above relay
// policy, and the binding floor is the higher one.
func TestNodeFloorPicksTheHighest(t *testing.T) {
	floor, ok := NodeFloor(FeeData{
		MempoolMinFee: decimal.MustParse("0.00005000"),
		MinRelayTxFee: decimal.MustParse("0.00001000"),
	})

	if !ok {
		t.Fatal("no floor found")
	}

	if floor.Rate.String() != "5" || floor.Field != "mempoolminfee" {
		t.Fatalf("floor = %+v, want 5 sat/vB from mempoolminfee", floor)
	}

	// A missing mempool floor falls back to relay policy.
	floor, ok = NodeFloor(FeeData{MinRelayTxFee: decimal.MustParse("0.00002000")})

	if !ok || floor.Rate.String() != "2" || floor.Field != "minrelaytxfee" {
		t.Fatalf("floor = %+v, want 2 sat/vB from minrelaytxfee", floor)
	}

	if _, ok := NodeFloor(FeeData{}); ok {
		t.Fatal("an empty mempool report produced a floor")
	}
}
