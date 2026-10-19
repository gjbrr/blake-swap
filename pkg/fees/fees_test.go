package fees

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/decimal"
)

// Rates are compared as exact strings so a rounding change cannot hide inside
// a numeric tolerance.
func rateString(t *testing.T, r decimal.Dec) string {
	t.Helper()
	return r.String()
}

// Parity: tests/fee_policy_tests.py
func TestCoinPerKvbConversion(t *testing.T) {
	cases := map[string]string{
		"0.00001000": "1",
		"0.00001190": "1.19",
		"0.00003000": "3",
		"0.00000100": "0.1",
		"0.00000001": "0.001",
	}

	for in, want := range cases {
		got, err := CoinPerKvbToSatVb(in)

		if err != nil {
			t.Fatalf("CoinPerKvbToSatVb(%q): %v", in, err)
		}

		if rateString(t, got) != want {
			t.Errorf("CoinPerKvbToSatVb(%q) = %s, want %s", in, got, want)
		}
	}

	for _, bad := range []any{0, "0", -1, "-0.00001", nil, "nonsense"} {
		if _, err := CoinPerKvbToSatVb(bad); err == nil {
			t.Errorf("CoinPerKvbToSatVb(%v) was accepted", bad)
		}
	}
}

func TestChooseRate(t *testing.T) {
	// A malformed estimator feerate is ignored, not fatal.
	rate, err := ChooseRate(FeeData{
		Feerate:       "unavailable",
		MempoolMinFee: 0.00001000,
	}, nil)

	if err != nil || rate.String() != "1" {
		t.Fatalf("malformed feerate: %s %v", rate, err)
	}

	// A malformed floor is ignored, not fatal.
	rate, err = ChooseRate(FeeData{
		Feerate:       0.00001190,
		MempoolMinFee: "nonsense",
		MinRelayTxFee: nil,
	}, nil)

	if err != nil || rate.String() != "1.19" {
		t.Fatalf("malformed floor: %s %v", rate, err)
	}

	// An unavailable or zero estimator falls back to the best floor.
	for _, data := range []FeeData{
		{MempoolMinFee: 0.00001000, MinRelayTxFee: 0.00000500},
		{Feerate: 0, MempoolMinFee: 0.00001000, MinRelayTxFee: 0.00000500},
		{Feerate: "nonsense", MempoolMinFee: 0.00001000, MinRelayTxFee: 0.00000500},
	} {
		rate, err := ChooseRate(data, nil)

		if err != nil || rate.String() != "1" {
			t.Fatalf("%+v: %s %v", data, rate, err)
		}
	}

	// The floor wins when the estimator is unusable.
	rate, err = ChooseRate(FeeData{MinRelayTxFee: 0.00003000}, nil)

	if err != nil || rate.String() != "3" {
		t.Fatalf("floor: %s %v", rate, err)
	}

	// No usable data at all fails closed.
	for _, data := range []FeeData{{}, {Feerate: 0}} {
		if _, err := ChooseRate(data, nil); err == nil ||
			!strings.Contains(err.Error(), "no usable fee estimate") {
			t.Fatalf("%+v: %v", data, err)
		}
	}

	// A multiplier below 1.0 is refused; nil means 1.0.
	below := decimal.MustParse("0.99")

	if _, err := ChooseRate(FeeData{Feerate: 0.00001000}, &below); err == nil ||
		!strings.Contains(err.Error(), "multiplier") {
		t.Fatalf("multiplier 0.99: %v", err)
	}

	boost := decimal.MustParse("1.10")

	rate, err = ChooseRate(FeeData{Feerate: 0.00001190}, &boost)

	if err != nil || rate.String() != "1.309" {
		t.Fatalf("multiplier 1.10: %s %v", rate, err)
	}
}

func TestFeeForVsize(t *testing.T) {
	// The measured regtest shapes: a claim is 140 vB, a refund 131 vB.
	cases := []struct {
		vsize int
		rate  string
		want  int64
	}{
		{131, "1.19", 156},
		{131, "1", 131},
		{140, "1.19", 167},
		{140, "2", 280},
		{131, "1.309", 172},
	}

	for _, c := range cases {
		got, err := FeeForVsize(c.vsize, decimal.MustParse(c.rate))

		if err != nil {
			t.Fatalf("FeeForVsize(%d, %s): %v", c.vsize, c.rate, err)
		}

		if got != c.want {
			t.Errorf("FeeForVsize(%d, %s) = %d, want %d", c.vsize, c.rate, got, c.want)
		}
	}

	for _, badVsize := range []int{0, -1} {
		if _, err := FeeForVsize(badVsize, decimal.MustParse("1")); err == nil ||
			!strings.Contains(err.Error(), "vsize") {
			t.Errorf("vsize %d: %v", badVsize, err)
		}
	}

	if _, err := FeeForVsize(140, decimal.Zero()); err == nil ||
		!strings.Contains(err.Error(), "feerate") {
		t.Errorf("zero feerate: %v", err)
	}
}

func TestMinimumInputForRate(t *testing.T) {
	// The documented boundary: exact arithmetic, not the rough formula.
	cases := []struct {
		rate  string
		vsize int
		want  int64
	}{
		{"2", 140, 2_791},
		{"33.8", 140, 47_311},
		{"171.08", 140, 239_511},
		{"889", 140, 1_244_591},
	}

	for _, c := range cases {
		got, err := MinimumInputForRate(decimal.MustParse(c.rate), c.vsize, DefaultMaxPercent)

		if err != nil {
			t.Fatal(err)
		}

		if got != c.want {
			t.Errorf("MinimumInputForRate(%s, %d) = %d, want %d", c.rate, c.vsize, got, c.want)
		}
	}

	// The rough formula would say 2800 for the first case; being exact is
	// what lets the documented table and the enforced cap agree.
	if _, err := MinimumInputForRate(decimal.MustParse("2"), 0, DefaultMaxPercent); err == nil {
		t.Error("vsize 0 was accepted")
	}

	if _, err := MinimumInputForRate(decimal.MustParse("2"), 140, decimal.MustParse("100")); err == nil {
		t.Error("percent 100 was accepted")
	}
}

func TestEnforceCap(t *testing.T) {
	// Under the cap: returned unchanged.
	if got, err := EnforceCap(50, 100_000, Cap{}); err != nil || got != 50 {
		t.Fatalf("fee 50: %d %v", got, err)
	}

	// The percentage cap binds: 140 vB at 2 sat/vB costs 280 on a 2,600
	// satoshi input, which is 10.8% and above the 10% cap.
	_, err := EnforceCap(280, 2_600, Cap{Vsize: 140})

	if err == nil || !strings.Contains(err.Error(), "safety cap") {
		t.Fatalf("percentage cap: %v", err)
	}

	// The refusal must be actionable: it names the rate the cap permits and
	// the input the current rate needs.
	msg := err.Error()

	for _, want := range []string{
		"fee=280", "cap=260", "input=2600",
		"vsize=140 vB", "permits 1.857 sat/vB",
		"below the 2791 satoshi minimum",
		PercentCapEnv, AbsoluteCapEnv,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal is missing %q:\n%s", want, msg)
		}
	}

	// The absolute cap is the control a percentage cannot express: 50 sats
	// is under 10% of a large input, so only the flat ceiling can refuse it.
	hundred := int64(100)

	if got, err := EnforceCap(50, 100_000, Cap{AbsoluteSats: &hundred}); err != nil || got != 50 {
		t.Fatalf("absolute cap permitting case: %d %v", got, err)
	}

	ninety := int64(40)

	if _, err := EnforceCap(50, 100_000, Cap{AbsoluteSats: &ninety}); err == nil ||
		!strings.Contains(err.Error(), "safety cap") {
		t.Fatalf("absolute cap binding case: %v", err)
	}

	//
	// The "would consume the whole input" branch is defensive and, exactly as
	// in Python, unreachable: fee >= input implies fee > the percentage cap
	// for every permitted percentage, so the cap refuses first. It is kept as
	// a restatement of the invariant rather than deleted.
	//
	// A fee at or above the input is still refused, just by the cap.
	if _, err := EnforceCap(100, 100, Cap{}); err == nil ||
		!strings.Contains(err.Error(), "safety cap") {
		t.Fatalf("whole-input fee: %v", err)
	}

	// Input validation.
	for _, bad := range []int64{0, -1} {
		if _, err := EnforceCap(bad, 100_000, Cap{}); err == nil {
			t.Errorf("fee %d was accepted", bad)
		}

		if _, err := EnforceCap(50, bad, Cap{}); err == nil {
			t.Errorf("input %d was accepted", bad)
		}
	}
}

// TestHistoricalEnvironments pins the policy to real fee-market history. The
// percentage cap, not the estimator, decides whether a small swap can settle.
func TestHistoricalEnvironments(t *testing.T) {
	environments := []struct {
		name string
		rate float64
	}{
		{"3y p50", 2.0},
		{"3y p90", 33.8},
		{"3y p99", 171.08},
		{"3y max", 889.0},
	}

	// A 2,600 satoshi HTLC can afford 1.857 sat/vB for a 140 vB claim.
	affordable, err := decimal.FromInt64(260).
		Quo(decimal.FromInt64(140), 3, decimal.HalfEven)

	if err != nil {
		t.Fatal(err)
	}

	if affordable.String() != "1.857" {
		t.Fatalf("260/140 = %s, want 1.857", affordable)
	}

	for _, environment := range environments {
		rate, err := decimal.FromFloat64(environment.rate)

		if err != nil {
			t.Fatalf("%s: %v", environment.name, err)
		}

		needed, err := MinimumInputForRate(rate, 140, DefaultMaxPercent)

		if err != nil {
			t.Fatalf("%s: %v", environment.name, err)
		}

		// The documented table says a 2,600 satoshi HTLC is not settleable
		// in any of these environments.
		if needed <= 2_600 {
			t.Errorf("%s: minimum %d <= 2600, but the table says it is not settleable",
				environment.name, needed)
		}
	}
}

// TestChooseRateAcceptsDecodedJSONNumbers: the node adapter keeps a feerate as
// json.Number so no float ever touches it, and that is the shape the policy
// actually receives from a real RPC reply. A string-only parser would reject
// every live estimate.
func TestChooseRateAcceptsDecodedJSONNumbers(t *testing.T) {
	rate, err := ChooseRate(FeeData{
		Feerate:       json.Number("0.00010000"),
		MempoolMinFee: json.Number("0.00001000"),
		MinRelayTxFee: json.Number("0.00001000"),
	}, nil)

	if err != nil {
		t.Fatalf("decoded numbers were rejected: %v", err)
	}

	if got, want := rate.String(), "10"; got != want {
		t.Fatalf("rate = %s, want %s", got, want)
	}
}
