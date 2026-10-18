package decimal

import (
	"testing"
)

func TestParseExact(t *testing.T) {
	cases := map[string]string{
		"0":            "0",
		"0.0":          "0",
		"1":            "1",
		"1.0":          "1",
		"1.19":         "1.19",
		"0.00001000":   "0.00001",
		"1e-8":         "0.00000001",
		"1E2":          "100",
		"-0.5":         "-0.5",
		" 1.5 ":        "1.5",
		"+2.25":        "2.25",
		"100000000":    "100000000",
		"0.1e1":        "1",
		"123.456e-2":   "1.23456",
		"0.3000000000": "0.3",
	}

	for in, want := range cases {
		got, err := Parse(in)

		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}

		if got.String() != want {
			t.Errorf("Parse(%q).String() = %q, want %q", in, got.String(), want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, in := range []string{
		"", " ", ".", "1..2", "1.2.3", "abc", "1e", "e1", "NaN", "Inf",
		"-Inf", "1 2", "0x10", "1,5",
	} {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) was accepted", in)
		}
	}
}

func TestExactArithmetic(t *testing.T) {
	// The canonical float failure: 0.1+0.2 must be exactly 0.3.
	sum := MustParse("0.1").Add(MustParse("0.2"))

	if sum.String() != "0.3" {
		t.Fatalf("0.1+0.2 = %s, want 0.3", sum.String())
	}

	if MustParse("1").Sub(MustParse("0.9")).String() != "0.1" {
		t.Fatalf("1-0.9 != 0.1")
	}

	// 1.19 sat/vB x 140 vB, which is the measured claim size.
	product := MustParse("1.19").Mul(MustParse("140"))

	if product.String() != "166.6" {
		t.Fatalf("1.19*140 = %s, want 166.6", product.String())
	}

	if product.CeilInt().String() != "167" {
		t.Fatalf("ceil(166.6) = %s, want 167", product.CeilInt())
	}

	// Multiplying by a negative scale (exponent notation) must be exact.
	thousand := MustParse("1e3")

	if thousand.Mul(MustParse("0.5")).String() != "500" {
		t.Fatalf("1e3*0.5 != 500")
	}
}

func TestCmpAndSign(t *testing.T) {
	if MustParse("1.10").Cmp(MustParse("1.1")) != 0 {
		t.Fatal("1.10 != 1.1")
	}

	if MustParse("2").Cmp(MustParse("10")) != -1 {
		t.Fatal("2 should be less than 10")
	}

	if MustParse("-0").Sign() != 0 {
		t.Fatal("-0 is not zero")
	}

	if !MustParse("-1.5").IsNegative() {
		t.Fatal("-1.5 should be negative")
	}
}

func TestQuoRounding(t *testing.T) {
	// 100/131 is the BLAKE fee share in a worked example: 0.763358...
	rate100, err := MustParse("100").Quo(MustParse("131"), 3, HalfEven)

	if err != nil {
		t.Fatal(err)
	}

	if rate100.String() != "0.763" {
		t.Fatalf("100/131 at 3dp = %s, want 0.763", rate100.String())
	}

	// ceil(-1.5) = -1, floor(-1.5) = -2: the sign must be respected.
	neg := MustParse("-1.5")

	if neg.CeilInt().String() != "-1" {
		t.Fatalf("ceil(-1.5) = %s", neg.CeilInt())
	}

	if neg.FloorInt().String() != "-2" {
		t.Fatalf("floor(-1.5) = %s", neg.FloorInt())
	}

	// Half-even on a tie: 2.5 -> 2, 3.5 -> 4.
	for in, want := range map[string]string{"2.5": "2", "3.5": "4", "-2.5": "-2"} {
		q, err := MustParse(in).Quo(FromInt64(1), 0, HalfEven)

		if err != nil {
			t.Fatal(err)
		}

		if q.String() != want {
			t.Errorf("round-half-even %s = %s, want %s", in, q, want)
		}
	}

	if _, err := MustParse("1").Quo(Zero(), 0, Floor); err == nil {
		t.Fatal("division by zero was accepted")
	}
}

func TestFixedMatchesPythonFormat(t *testing.T) {
	cases := map[string]string{
		"1":      "1.000",
		"1.5":    "1.500",
		"0.0001": "0.000",
		// Python's Decimal formats half-even: 0.0005 -> 0.000.
		"0.0005":  "0.000",
		"0.0015":  "0.002",
		"2.71828": "2.718",
		"166.6":   "166.600",
	}

	for in, want := range cases {
		got, err := MustParse(in).Fixed(3)

		if err != nil {
			t.Fatal(err)
		}

		if got != want {
			t.Errorf("Fixed(%s, 3) = %s, want %s", in, got, want)
		}
	}

	if _, err := MustParse("1").Fixed(-1); err == nil {
		t.Fatal("negative precision was accepted")
	}
}

func TestInt64(t *testing.T) {
	if n, err := MustParse("42.9").Int64(); err != nil || n != 42 {
		t.Fatalf("42.9 -> %d, %v", n, err)
	}

	if n, err := MustParse("-42.9").Int64(); err != nil || n != -42 {
		t.Fatalf("-42.9 -> %d, %v", n, err)
	}

	if _, err := MustParse("1e30").Int64(); err == nil {
		t.Fatal("overflow was not reported")
	}
}

func TestFromFloat64UsesShortestRepr(t *testing.T) {
	// 0.1 as a float must parse as exactly 0.1, not 0.1000000000000000055.
	d, err := FromFloat64(0.1)

	if err != nil {
		t.Fatal(err)
	}

	if d.String() != "0.1" {
		t.Fatalf("FromFloat64(0.1) = %s", d.String())
	}
}
