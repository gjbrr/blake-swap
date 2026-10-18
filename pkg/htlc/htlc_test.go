package htlc

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func pk(prefix, fill string) string { return prefix + strings.Repeat(fill, 64) }

func hash32(b byte) string { return strings.Repeat(string([]byte{"0123456789abcdef"[b%16]}), 64) }

func bond() Bond {
	return Bond{
		SecretHash:    hash32(3),
		BlakeBobPub:   pk("02", "b"),
		BlakeAlicePub: pk("03", "c"),
		BTCAlicePub:   pk("02", "d"),
		BTCBobPub:     pk("03", "e"),
		BlakeTimeout:  1_000,
		BTCTimeout:    900,
	}
}

func preimageFor(secretHash string) string {
	// Search would be silly; the tests build the hash from a known preimage.
	return ""
}

func TestDescriptorShape(t *testing.T) {
	b := bond()

	blake, err := b.Descriptor(Blake)

	if err != nil {
		t.Fatal(err)
	}

	want := "wsh(or_i(and_v(v:pk(" + b.BlakeBobPub + "),sha256(" + b.SecretHash +
		")),and_v(v:after(1000),pk(" + b.BlakeAlicePub + "))))"

	if blake != want {
		t.Fatalf("blake descriptor =\n%s\nwant\n%s", blake, want)
	}

	btc, err := b.Descriptor(BTC)

	if err != nil {
		t.Fatal(err)
	}

	want = "wsh(or_i(and_v(v:pk(" + b.BTCAlicePub + "),sha256(" + b.SecretHash +
		")),and_v(v:after(900),pk(" + b.BTCBobPub + "))))"

	if btc != want {
		t.Fatalf("btc descriptor =\n%s\nwant\n%s", btc, want)
	}

	// The claimer/refunder inversion is the protocol: Bob claims BLAKE, Alice
	// claims Bitcoin.
	if !strings.Contains(blake, b.BlakeBobPub) || !strings.Contains(blake, b.BlakeAlicePub) {
		t.Fatal("blake descriptor is missing a key")
	}

	if strings.Contains(btc, b.BlakeBobPub) {
		t.Fatal("btc descriptor used a blake key")
	}
}

func TestDescriptorSubstitution(t *testing.T) {
	b := bond()

	base, err := b.Descriptor(Blake)

	if err != nil {
		t.Fatal(err)
	}

	private := map[string]string{b.BlakeBobPub: "cNOPRIVATEKEYFORTEST"}

	substituted, err := b.DescriptorWithKeys(Blake, private)

	if err != nil {
		t.Fatal(err)
	}

	if substituted == base {
		t.Fatal("substituting a private key did not change the descriptor")
	}

	if !strings.Contains(substituted, "cNOPRIVATEKEYFORTEST") {
		t.Fatal("substituted key is missing")
	}

	// Only the substituted key changes; the rest of the body is identical.
	if strings.Contains(substituted, b.BlakeBobPub) {
		t.Fatal("public key survived substitution")
	}

	if !strings.Contains(substituted, b.SecretHash) {
		t.Fatal("hashlock changed during substitution")
	}
}

func TestDescriptorRefusesMalformedBonds(t *testing.T) {
	cases := map[string]func(*Bond){
		"short hash":        func(b *Bond) { b.SecretHash = "abcd" },
		"uppercase hash":    func(b *Bond) { b.SecretHash = strings.Repeat("Ab", 32) },
		"bad pubkey prefix": func(b *Bond) { b.BlakeBobPub = "04" + strings.Repeat("b", 64) },
		"short pubkey":      func(b *Bond) { b.BTCBobPub = "02ab" },
		"zero blake time":   func(b *Bond) { b.BlakeTimeout = 0 },
		"negative btc time": func(b *Bond) { b.BTCTimeout = -1 },
	}

	for name, mutate := range cases {
		b := bond()
		mutate(&b)

		if _, err := b.Descriptor(Blake); err == nil {
			t.Errorf("%s: descriptor was built", name)
		}
	}

	if _, err := bond().Descriptor("dogecoin"); err == nil {
		t.Error("unknown chain was accepted")
	}
}

// spendTx builds a transaction spending the HTLC with the given witness.
func spendTx(witness []string) Tx {
	return Tx{Vin: []Input{
		{TxID: hash32(1), Vout: 0, Witness: []string{"unrelated"}},
		{TxID: hash32(9), Vout: 2, Witness: witness},
	}}
}

func TestClassifySpend(t *testing.T) {
	secret := make([]byte, 32)

	for i := range secret {
		secret[i] = byte(i)
	}

	secretHex := hex.EncodeToString(secret)
	sum := sha256.Sum256(secret)
	secretHash := hex.EncodeToString(sum[:])

	fundingTxID := hash32(9)
	const fundingVout int64 = 2

	sig := strings.Repeat("30", 71)
	script := "51"

	cases := []struct {
		name    string
		witness []string
		want    Branch
	}{
		{
			name:    "claim carries the preimage and the IF selector",
			witness: []string{secretHex, sig, "01", script},
			want:    Claim,
		},
		{
			name:    "refund has the ELSE selector and no preimage",
			witness: []string{sig, "", script},
			want:    Refund,
		},
		{
			name:    "refund with an explicit zero selector",
			witness: []string{sig, "00", script},
			want:    Refund,
		},
		{
			name:    "claim selector without the preimage is not a claim",
			witness: []string{sig, "01", script},
			want:    Unknown,
		},
		{
			name:    "refund selector with the preimage is not a refund",
			witness: []string{secretHex, sig, "00", script},
			want:    Unknown,
		},
		{
			name:    "an unknown selector is unknown",
			witness: []string{sig, "7f", script},
			want:    Unknown,
		},
		{
			name:    "a witness too short to have a selector is unknown",
			witness: []string{sig, script},
			want:    Unknown,
		},
		{
			name:    "a preimage that hashes to something else is not evidence",
			witness: []string{strings.Repeat("ab", 32), sig, "01", script},
			want:    Unknown, // selector 01 without the right preimage
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ClassifySpend(spendTx(c.witness), fundingTxID, fundingVout, secretHash)

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got != c.want {
				t.Fatalf("branch = %s, want %s", got, c.want)
			}
		})
	}
}

func TestClassifySpendFailClosed(t *testing.T) {
	secret := strings.Repeat("ab", 32)
	sum := sha256.Sum256([]byte{0xab})
	secretHash := hex.EncodeToString(sum[:])

	// The transaction must spend our exact outpoint.
	tx := Tx{Vin: []Input{{TxID: hash32(1), Vout: 0, Witness: []string{secret, "01", "51"}}}}

	if _, err := ClassifySpend(tx, hash32(9), 2, secretHash); err == nil {
		t.Fatal("a transaction spending another outpoint was accepted")
	}

	// A confirmed spend with no witness at all is a security failure, not an
	// unknown branch.
	empty := Tx{Vin: []Input{{TxID: hash32(9), Vout: 2}}}

	if _, err := ClassifySpend(empty, hash32(9), 2, secretHash); err == nil ||
		!strings.Contains(err.Error(), "SECURITY FAILURE") {
		t.Fatalf("witness-less spend: %v", err)
	}
}

func TestClassifyIsStructuralNotAbsenceBased(t *testing.T) {
	// A no-preimage witness with a claim selector must NOT be read as a
	// refund just because no preimage is present; that is the whole reason
	// the classifier looks at the selector.
	secretHash := hash32(3)
	tx := spendTx([]string{"deadbeef", "01", "51"})

	branch, err := ClassifySpend(tx, hash32(9), 2, secretHash)

	if err != nil {
		t.Fatal(err)
	}

	if branch == Refund {
		t.Fatal("a claim-shaped witness without a preimage was classified as a refund")
	}

	if branch != Unknown {
		t.Fatalf("branch = %s, want unknown", branch)
	}
}
