package market

import (
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/amount"
)

func TestDirectionRoles(t *testing.T) {
	// The inversion is the point: a btc-for-blake maker is protocol Bob.
	cases := map[Direction][2]string{
		BlakeForBTC: {"alice", "bob"},
		BTCForBlake: {"bob", "alice"},
	}

	for direction, want := range cases {
		maker, taker, err := direction.ProtocolRoles()

		if err != nil {
			t.Fatal(err)
		}

		if maker != want[0] || taker != want[1] {
			t.Errorf("%s: maker=%s taker=%s, want %v", direction, maker, taker, want)
		}
	}

	if _, _, err := Direction("sideways").ProtocolRoles(); err == nil {
		t.Error("unknown direction was accepted")
	}

	if _, err := ParseDirection("blake-for-btc"); err != nil {
		t.Error("valid direction was refused")
	}

	if _, err := ParseDirection("BLAKE-FOR-BTC"); err == nil {
		t.Error("case-variant direction was accepted")
	}
}

func TestGiveWant(t *testing.T) {
	give, want, err := BlakeForBTC.GiveWant(amount.MustParseBlake("0.5"), amount.Sats(25_000))

	if err != nil {
		t.Fatal(err)
	}

	if give != "0.5 BLAKE" || want != "25,000 sats" {
		t.Fatalf("blake-for-btc gives %q wants %q", give, want)
	}

	give, want, err = BTCForBlake.GiveWant(amount.MustParseBlake("0.5"), amount.Sats(25_000))

	if err != nil {
		t.Fatal(err)
	}

	if give != "25,000 sats" || want != "0.5 BLAKE" {
		t.Fatalf("btc-for-blake gives %q wants %q", give, want)
	}

	// Large amounts keep their separators and small ones do not gain any.
	give, _, _ = BlakeForBTC.GiveWant(amount.MustParseBlake("1"), amount.Sats(1_234_567))

	if give != "1 BLAKE" {
		t.Fatalf("give = %q", give)
	}

	_, want, _ = BlakeForBTC.GiveWant(amount.MustParseBlake("1"), amount.Sats(1_234_567))

	if want != "1,234,567 sats" {
		t.Fatalf("want = %q", want)
	}
}

func offerV1() map[string]any {
	return map[string]any{
		"type":         "market-offer",
		"version":      float64(1),
		"side":         "sell",
		"base":         "BLAKE",
		"quote":        "BTC",
		"blake_amount": "0.5",
		"btc_sats":     float64(25000),
		"created_at":   float64(1_700_000_000),
		"expires_at":   float64(1_700_001_800),
		"nonce":        "0011223344556677",
	}
}

func offerV2() map[string]any {
	return map[string]any{
		"type":         "market-offer",
		"version":      float64(2),
		"direction":    "btc-for-blake",
		"base":         "BLAKE",
		"quote":        "BTC",
		"blake_amount": "0.5",
		"btc_sats":     float64(25000),
		"created_at":   float64(1_700_000_000),
		"expires_at":   float64(1_700_001_800),
		"nonce":        "0011223344556677",
	}
}

func TestParseOfferV1(t *testing.T) {
	offer, err := ParseOffer(offerV1())

	if err != nil {
		t.Fatal(err)
	}

	// Version 1 has no direction field; it is implied by `side: sell`.
	if offer.Direction != BlakeForBTC {
		t.Fatalf("v1 direction = %s", offer.Direction)
	}

	if offer.Version != 1 || offer.BTCSats != 25_000 {
		t.Fatalf("offer = %+v", offer)
	}

	if int64(offer.BlakeAmount) != 50_000_000 {
		t.Fatalf("blake amount = %d", offer.BlakeAmount)
	}

	if offer.Expired(1_700_000_000) {
		t.Error("a fresh offer is already expired")
	}

	if !offer.Expired(1_700_001_800) {
		t.Error("an offer expiring exactly now should be expired")
	}
}

func TestParseOfferV2(t *testing.T) {
	offer, err := ParseOffer(offerV2())

	if err != nil {
		t.Fatal(err)
	}

	if offer.Direction != BTCForBlake {
		t.Fatalf("v2 direction = %s", offer.Direction)
	}

	maker, _, _ := offer.Direction.ProtocolRoles()

	if maker != "bob" {
		t.Fatalf("btc-for-blake maker role = %s", maker)
	}
}

func TestParseOfferRefusals(t *testing.T) {
	cases := map[string]func(map[string]any){
		"wrong type":          func(m map[string]any) { m["type"] = "market-take" },
		"unsupported version": func(m map[string]any) { m["version"] = float64(3) },

		"v2 bad direction": func(m map[string]any) { m["direction"] = "sideways" },
		"wrong base":       func(m map[string]any) { m["base"] = "BTC" },
		"wrong quote":      func(m map[string]any) { m["quote"] = "BLAKE" },
		"zero amount":      func(m map[string]any) { m["blake_amount"] = "0" },
		"zero sats":        func(m map[string]any) { m["btc_sats"] = float64(0) },
		"fractional sats":  func(m map[string]any) { m["btc_sats"] = 1.5 },
		"expiry before creation": func(m map[string]any) {
			m["expires_at"] = float64(1_600_000_000)
		},
		"expiry equal to creation": func(m map[string]any) {
			m["expires_at"] = m["created_at"]
		},
		"non-string amount": func(m map[string]any) { m["blake_amount"] = 0.5 },
		"settlement data":   func(m map[string]any) { m["secret_hash"] = strings.Repeat("aa", 32) },
		"funding data":      func(m map[string]any) { m["btc_funding_txid"] = "aa" },
	}

	for name, mutate := range cases {
		message := offerV2()
		mutate(message)

		if _, err := ParseOffer(message); err == nil {
			t.Errorf("%s: offer was accepted", name)
		}
	}

	// Version 1 has its own rules, so its refusals are checked on a v1 offer.
	for name, mutate := range map[string]func(map[string]any){
		"v1 buy side":     func(m map[string]any) { m["side"] = "buy" },
		"v1 missing side": func(m map[string]any) { delete(m, "side") },
	} {
		message := offerV1()
		mutate(message)

		if _, err := ParseOffer(message); err == nil {
			t.Errorf("%s: offer was accepted", name)
		}
	}
}

func takeV1() map[string]any {
	return map[string]any{
		"type":         "market-take",
		"version":      float64(1),
		"offer_id":     strings.Repeat("11", 32),
		"maker":        strings.Repeat("22", 32),
		"base":         "BLAKE",
		"quote":        "BTC",
		"blake_amount": "0.5",
		"btc_sats":     float64(25000),
		"created_at":   float64(1_700_000_100),
		"nonce":        "8899aabbccddeeff",
	}
}

func takeV2() map[string]any {
	message := takeV1()
	message["version"] = float64(2)
	message["direction"] = "btc-for-blake"
	message["taker"] = strings.Repeat("33", 32)
	message["maker_role"] = "bob"
	message["taker_role"] = "alice"

	return message
}

func TestTakeValidation(t *testing.T) {
	offer, err := ParseOffer(offerV2())

	if err != nil {
		t.Fatal(err)
	}

	take, err := ParseTake(takeV2())

	if err != nil {
		t.Fatal(err)
	}

	if err := take.ValidateAgainstOffer(offer, strings.Repeat("11", 32), strings.Repeat("22", 32)); err != nil {
		t.Fatalf("valid take was refused: %v", err)
	}
}

func TestTakeRefusals(t *testing.T) {
	offer, _ := ParseOffer(offerV2())

	offerID := strings.Repeat("11", 32)
	maker := strings.Repeat("22", 32)

	cases := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{"version downgrade", func(m map[string]any) { m["version"] = float64(1) }, "version"},
		{"other offer", func(m map[string]any) { m["offer_id"] = strings.Repeat("44", 32) }, "another offer"},
		{"other maker", func(m map[string]any) { m["maker"] = strings.Repeat("55", 32) }, "maker mismatch"},
		{"changed blake terms", func(m map[string]any) { m["blake_amount"] = "0.6" }, "BLAKE terms"},
		{"changed btc terms", func(m map[string]any) { m["btc_sats"] = float64(30000) }, "BTC terms"},
		{"changed direction", func(m map[string]any) { m["direction"] = "blake-for-btc" }, "direction"},
		{"maker role", func(m map[string]any) { m["maker_role"] = "alice" }, "maker role"},
		{"taker role", func(m map[string]any) { m["taker_role"] = "bob" }, "taker role"},
	}

	for _, c := range cases {
		message := takeV2()
		c.mutate(message)

		take, err := ParseTake(message)

		if err != nil {
			// Some mutations are refused at parse time; that still counts,
			// but the case is meant to reach validation, so report it.
			t.Fatalf("%s: refused at parse: %v", c.name, err)
		}

		err = take.ValidateAgainstOffer(offer, offerID, maker)

		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error = %v, want %q", c.name, err, c.want)
		}
	}
}

func TestTakeV1BindsImplicitDirection(t *testing.T) {
	offer, _ := ParseOffer(offerV1())

	take, err := ParseTake(takeV1())

	if err != nil {
		t.Fatal(err)
	}

	if err := take.ValidateAgainstOffer(offer, strings.Repeat("11", 32), strings.Repeat("22", 32)); err != nil {
		t.Fatalf("v1 take was refused: %v", err)
	}
}

func matchV2() map[string]any {
	return map[string]any{
		"type":         "market-match",
		"version":      float64(2),
		"offer_id":     strings.Repeat("11", 32),
		"take_id":      strings.Repeat("66", 32),
		"swap_id":      "abcdef123456",
		"maker":        strings.Repeat("22", 32),
		"taker":        strings.Repeat("33", 32),
		"direction":    "btc-for-blake",
		"maker_role":   "bob",
		"taker_role":   "alice",
		"base":         "BLAKE",
		"quote":        "BTC",
		"blake_amount": "0.5",
		"btc_sats":     float64(25000),
		"created_at":   float64(1_700_000_200),
	}
}

func TestParseMatch(t *testing.T) {
	match, err := ParseMatch(matchV2())

	if err != nil {
		t.Fatal(err)
	}

	if match.SwapID != "abcdef123456" || match.Direction != BTCForBlake {
		t.Fatalf("match = %+v", match)
	}

	for _, field := range []string{"offer_id", "take_id", "swap_id", "maker", "taker"} {
		message := matchV2()
		delete(message, field)

		if _, err := ParseMatch(message); err == nil {
			t.Errorf("a match without %s was accepted", field)
		}
	}

	bad := matchV2()
	bad["direction"] = "sideways"

	if _, err := ParseMatch(bad); err == nil {
		t.Error("an invalid direction was accepted")
	}
}

func TestParseCancel(t *testing.T) {
	cancel, err := ParseCancel(map[string]any{
		"type":       "market-cancel",
		"version":    float64(2),
		"offer_id":   strings.Repeat("11", 32),
		"created_at": float64(1_700_000_300),
	})

	if err != nil {
		t.Fatal(err)
	}

	if cancel.OfferID != strings.Repeat("11", 32) || cancel.Version != 2 {
		t.Fatalf("cancel = %+v", cancel)
	}

	if _, err := ParseCancel(map[string]any{
		"type":       "market-cancel",
		"version":    float64(2),
		"created_at": float64(1),
	}); err == nil {
		t.Error("a cancel with no offer was accepted")
	}
}
