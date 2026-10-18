package model

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/state"
)

// fixedReader returns a deterministic byte stream, so swap IDs and secrets are
// reproducible in tests.
type fixedReader struct{ b []byte }

func (r *fixedReader) Read(p []byte) (int, error) {
	n := copy(p, r.b)
	r.b = r.b[n:]

	return n, nil
}

func seq(n byte) *fixedReader {
	b := make([]byte, 128)

	for i := range b {
		b[i] = n + byte(i)
	}

	return &fixedReader{b: b}
}

func TestNewRecord(t *testing.T) {
	swap, err := New(seq(1), NewParams{
		BlakeAmount:    amount.MustParseBlake("0.5"),
		BTCSats:        25_000,
		BlakeTimeout:   1_000,
		BTCTimeout:     900,
		BlakeScanStart: 500,
		BTCScanStart:   100,
	})

	if err != nil {
		t.Fatal(err)
	}

	if swap.State != state.Offered {
		t.Errorf("state = %s, want OFFERED", swap.State)
	}

	if swap.BlakeAmountBaseUnits != 50_000_000 {
		t.Errorf("base units = %d", swap.BlakeAmountBaseUnits)
	}

	if swap.CreatedAt <= 0 {
		t.Error("created_at was not set")
	}

	if !IsSwapID(swap.SwapID) {
		t.Errorf("swap id %q is not 12 hex characters", swap.SwapID)
	}

	if swap.SecretHex == nil || !IsHex32(*swap.SecretHex) {
		t.Error("secret_hex is not 32-byte hex")
	}

	if !IsHex32(swap.SecretHash) {
		t.Error("secret_hash is not 32-byte hex")
	}

	// The hash must be the hash of the secret.
	raw, _ := hex.DecodeString(*swap.SecretHex)
	sum := sha256.Sum256(raw)

	if hex.EncodeToString(sum[:]) != swap.SecretHash {
		t.Error("secret_hash is not sha256(secret_hex)")
	}

	if swap.BlakeScanStartHeight == nil || *swap.BlakeScanStartHeight != 500 {
		t.Error("blake scan anchor was not recorded")
	}

	if swap.BTCScanStartHeight == nil || *swap.BTCScanStartHeight != 100 {
		t.Error("btc scan anchor was not recorded")
	}
}

func TestNewRejectsBadInput(t *testing.T) {
	base := NewParams{
		BlakeAmount: amount.MustParseBlake("1"),
		BTCSats:     10_000,
	}

	bad := []NewParams{
		{SwapID: "nothexnothex", BlakeAmount: amount.MustParseBlake("1"), BTCSats: 1},
		{SwapID: "abc", BlakeAmount: amount.MustParseBlake("1"), BTCSats: 1},
		{BlakeAmount: 0, BTCSats: 1},
		{BlakeAmount: amount.MustParseBlake("1"), BTCSats: 0},
		{BlakeAmount: amount.MustParseBlake("1"), BTCSats: -5},
	}

	for _, p := range bad {
		if _, err := New(seq(1), p); err == nil {
			t.Errorf("New(%+v) was accepted", p)
		}
	}

	// And the good baseline still works, so the table above is not passing for
	// an unrelated reason.
	if _, err := New(seq(1), base); err != nil {
		t.Fatalf("baseline rejected: %v", err)
	}
}

func TestSecretMismatchIsRefused(t *testing.T) {
	swap, err := New(seq(7), NewParams{
		BlakeAmount: amount.MustParseBlake("1"),
		BTCSats:     10_000,
	})

	if err != nil {
		t.Fatal(err)
	}

	// Flip one nibble of the stored secret: the record must not validate.
	corrupted := *swap.SecretHex
	first := corrupted[0]

	if first == 'a' {
		corrupted = "b" + corrupted[1:]
	} else {
		corrupted = "a" + corrupted[1:]
	}

	swap.SecretHex = &corrupted

	if err := swap.Validate(); err == nil ||
		!strings.Contains(err.Error(), "does not match secret_hash") {
		t.Fatalf("corrupted secret: %v", err)
	}
}

func TestValidateRefusals(t *testing.T) {
	newSwap := func(t *testing.T) *Swap {
		t.Helper()

		swap, err := New(seq(3), NewParams{
			BlakeAmount: amount.MustParseBlake("1"),
			BTCSats:     10_000,
		})

		if err != nil {
			t.Fatal(err)
		}

		return swap
	}

	t.Run("bad state", func(t *testing.T) {
		swap := newSwap(t)
		swap.State = "NOT_A_STATE"

		if err := swap.Validate(); err == nil {
			t.Error("unknown state was accepted")
		}
	})

	t.Run("non-positive amounts", func(t *testing.T) {
		swap := newSwap(t)
		swap.BlakeAmountBaseUnits = 0

		if err := swap.Validate(); err == nil {
			t.Error("zero BLAKE amount was accepted")
		}

		swap = newSwap(t)
		swap.BTCSats = 0

		if err := swap.Validate(); err == nil {
			t.Error("zero BTC amount was accepted")
		}
	})

	t.Run("malformed txid", func(t *testing.T) {
		swap := newSwap(t)
		bad := "00"
		swap.BTCFundingTxID = &bad

		if err := swap.Validate(); err == nil {
			t.Error("short txid was accepted")
		}
	})

	t.Run("settlement hex without an anchor", func(t *testing.T) {
		swap := newSwap(t)
		hexed := "0100"
		swap.BTCCLaimHex = &hexed

		if err := swap.Validate(); err == nil ||
			!strings.Contains(err.Error(), "identity anchor") {
			t.Errorf("unanchored settlement hex: %v", err)
		}
	})

	t.Run("settlement hex with an anchor", func(t *testing.T) {
		swap := newSwap(t)
		hexed := "0100"
		txid := strings.Repeat("ab", 32)
		swap.BTCCLaimHex = &hexed
		swap.BTCCLaimTxID = &txid

		if err := swap.Validate(); err != nil {
			t.Errorf("anchored settlement hex was refused: %v", err)
		}
	})

	t.Run("blake settlement anchored by the reorg txid", func(t *testing.T) {
		swap := newSwap(t)
		hexed := "0200"
		txid := strings.Repeat("cd", 32)
		swap.BlakeClaimHex = &hexed
		swap.BlakeReorgClaimTxID = &txid

		if err := swap.Validate(); err != nil {
			t.Errorf("reorg-anchored settlement hex was refused: %v", err)
		}
	})
}

func TestTransitionAndReorg(t *testing.T) {
	swap, err := New(seq(5), NewParams{
		BlakeAmount: amount.MustParseBlake("1"),
		BTCSats:     10_000,
	})

	if err != nil {
		t.Fatal(err)
	}

	if err := swap.Transition(state.BTCFunded); err == nil {
		t.Fatal("OFFERED -> BTC_FUNDED was accepted")
	}

	if err := swap.Transition(state.BlakeFunded); err != nil {
		t.Fatal(err)
	}

	if swap.State != state.BlakeFunded {
		t.Fatalf("state = %s", swap.State)
	}

	// A reorg may not move forward, and forward progress may not move backward.
	if err := swap.ApplyReorg(state.BTCClaimed); err == nil {
		t.Fatal("BLAKE_FUNDED reorg -> BTC_CLAIMED was accepted")
	}
}

func TestMarshalDecodeRoundTrip(t *testing.T) {
	swap, err := New(seq(9), NewParams{
		BlakeAmount:    amount.MustParseBlake("2.5"),
		BTCSats:        40_000,
		BlakeTimeout:   2_000,
		BTCTimeout:     1_800,
		BlakeScanStart: 700,
		BTCScanStart:   650,
	})

	if err != nil {
		t.Fatal(err)
	}

	encoded, err := swap.Marshal()

	if err != nil {
		t.Fatal(err)
	}

	decoded, err := Decode(encoded, func(string) {})

	if err != nil {
		t.Fatal(err)
	}

	if *decoded.SecretHex != *swap.SecretHex ||
		decoded.SecretHash != swap.SecretHash ||
		decoded.BlakeAmountBaseUnits != swap.BlakeAmountBaseUnits ||
		decoded.BTCSats != swap.BTCSats ||
		decoded.State != swap.State ||
		decoded.SwapID != swap.SwapID {
		t.Fatalf("round trip changed the record:\n%+v\n%+v", decoded, swap)
	}

	// Optional fields must stay absent rather than becoming empty strings.
	if decoded.BTCFundingTxID != nil {
		t.Error("absent field came back present")
	}
}

func TestJSONFieldNames(t *testing.T) {
	swap, err := New(seq(11), NewParams{
		BlakeAmount: amount.MustParseBlake("1"),
		BTCSats:     10_000,
	})

	if err != nil {
		t.Fatal(err)
	}

	encoded, _ := swap.Marshal()

	// The durable field names are a compatibility surface; a rename is a
	// migration. Spot-check the ones other components read.
	for _, key := range []string{
		`"swap_id"`, `"state"`, `"created_at"`, `"btc_sats"`,
		`"blake_amount_base_units"`, `"secret_hex"`, `"secret_hash"`,
		`"blake_timeout"`, `"btc_timeout"`,
		`"blake_scan_start_height"`, `"btc_scan_start_height"`,
		`"blake_funding_txid"`, `"blake_funding_vout"`,
	} {
		if !bytes.Contains(encoded, []byte(key)) {
			t.Errorf("record is missing %s: %s", key, encoded)
		}
	}
}

func TestMigration(t *testing.T) {
	base := func(extra map[string]any) map[string]any {
		record := map[string]any{
			"swap_id":                 "abcdef123456",
			"state":                   "OFFERED",
			"created_at":              float64(1_700_000_000),
			"btc_sats":                float64(25_000),
			"secret_hash":             strings.Repeat("cc", 32),
			"blake_timeout":           float64(1_000),
			"btc_timeout":             float64(900),
			"blake_scan_start_height": float64(10),
			"btc_scan_start_height":   float64(20),
		}

		for k, v := range extra {
			record[k] = v
		}

		return record
	}

	encode := func(record map[string]any) []byte {
		data, err := json.Marshal(record)

		if err != nil {
			t.Fatal(err)
		}

		return data
	}

	t.Run("legacy exact value migrates silently", func(t *testing.T) {
		var warnings []string

		swap, err := Decode(encode(base(map[string]any{"blake_amount": 0.3})), func(w string) {
			warnings = append(warnings, w)
		})

		if err != nil {
			t.Fatal(err)
		}

		if swap.BlakeAmountBaseUnits != 30_000_000 {
			t.Fatalf("base units = %d", swap.BlakeAmountBaseUnits)
		}

		if len(warnings) != 0 {
			t.Fatalf("unexpected warnings: %v", warnings)
		}
	})

	t.Run("legacy float artefact migrates with a warning", func(t *testing.T) {
		var warnings []string

		swap, err := Decode(encode(base(map[string]any{"blake_amount": 0.30000000000000004})), func(w string) {
			warnings = append(warnings, w)
		})

		if err != nil {
			t.Fatal(err)
		}

		if swap.BlakeAmountBaseUnits != 30_000_000 {
			t.Fatalf("base units = %d", swap.BlakeAmountBaseUnits)
		}

		if len(warnings) != 1 || !strings.Contains(warnings[0], "migrated") {
			t.Fatalf("warnings = %v", warnings)
		}
	})

	t.Run("both fields agreeing", func(t *testing.T) {
		var warnings []string

		_, err := Decode(encode(base(map[string]any{
			"blake_amount":            0.3,
			"blake_amount_base_units": 30_000_000,
		})), func(w string) { warnings = append(warnings, w) })

		if err != nil {
			t.Fatal(err)
		}

		if len(warnings) != 0 {
			t.Fatalf("unexpected warnings: %v", warnings)
		}
	})

	t.Run("both fields disagreeing: the integer wins and says so", func(t *testing.T) {
		var warnings []string

		swap, err := Decode(encode(base(map[string]any{
			"blake_amount":            0.3,
			"blake_amount_base_units": 31_000_000,
		})), func(w string) { warnings = append(warnings, w) })

		if err != nil {
			t.Fatal(err)
		}

		if swap.BlakeAmountBaseUnits != 31_000_000 {
			t.Fatalf("base units = %d, want the integer", swap.BlakeAmountBaseUnits)
		}

		if len(warnings) != 1 || !strings.Contains(warnings[0], "disagrees") {
			t.Fatalf("warnings = %v", warnings)
		}
	})

	t.Run("current shape needs no migration", func(t *testing.T) {
		var warnings []string

		_, err := Decode(encode(base(map[string]any{
			"blake_amount_base_units": 5_000_000,
		})), func(w string) { warnings = append(warnings, w) })

		if err != nil {
			t.Fatal(err)
		}

		if len(warnings) != 0 {
			t.Fatalf("unexpected warnings: %v", warnings)
		}
	})

	t.Run("a record with no amount at all is refused", func(t *testing.T) {
		if _, err := Decode(encode(base(nil)), nil); err == nil {
			t.Fatal("a record with no amount was accepted")
		}
	})
}

func TestSwapIDShape(t *testing.T) {
	seen := map[string]bool{}

	for i := 0; i < 64; i++ {
		id, err := NewSwapID(seq(byte(i)))

		if err != nil {
			t.Fatal(err)
		}

		if !IsSwapID(id) {
			t.Fatalf("%q is not a swap id", id)
		}

		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}

		seen[id] = true
	}

	for _, bad := range []string{"", "abc", "abcdef12345g", "ABCDEF1234567", "abcdef123456 "} {
		if IsSwapID(bad) {
			t.Errorf("IsSwapID(%q) = true", bad)
		}
	}
}
