package keys

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"testing"
)

// TestFastAndSlowVerifiersAgree is the whole justification for having two
// verifiers: the fast one is what production uses, and the slow one is the
// independent statement of BIP-340 that the fast one has to match.
//
// A disagreement in either direction is a failure. A false accept is a forged
// event that reaches the engine; a false reject is a valid event nobody sees.
func TestFastAndSlowVerifiersAgree(t *testing.T) {
	t.Run("official vectors", func(t *testing.T) {
		for _, vector := range bip340Vectors {
			pub, err := hex.DecodeString(vector.pubkey)

			if err != nil {
				t.Fatal(err)
			}

			msg, _ := hex.DecodeString(vector.message)
			sig, _ := hex.DecodeString(vector.sig)

			fast := verifySchnorr(pub, msg, sig)
			slow := verifyBIP340(pub, msg, sig)

			if fast != slow {
				t.Fatalf("%s: fast=%v slow=%v", vector.name, fast, slow)
			}

			if !fast {
				t.Fatalf("%s: the vector did not verify", vector.name)
			}
		}
	})

	t.Run("signatures this implementation produced", func(t *testing.T) {
		for round := 0; round < 64; round++ {
			secret := make([]byte, 32)

			if _, err := rand.Read(secret); err != nil {
				t.Fatal(err)
			}

			key, err := FromSecret(secret)

			if err != nil {
				continue // an out-of-range secret is not a key
			}

			message := make([]byte, 32)

			if _, err := rand.Read(message); err != nil {
				t.Fatal(err)
			}

			signature, err := key.Sign(message)

			if err != nil {
				t.Fatal(err)
			}

			valid, err := VerifySchnorr(key.PublicHex(), message, signature)

			if err != nil {
				t.Fatal(err)
			}

			if !valid {
				t.Fatalf("round %d: a signature this implementation made was rejected", round)
			}

			if !verifyBIP340(key.Public(), message, signature) {
				t.Fatalf("round %d: the two verifiers disagree on a valid signature", round)
			}

			// A flipped bit in the message, the signature or the key must be
			// rejected by both, and they must agree about it.
			for name, mutate := range map[string]func(*[3][]byte){
				"message": func(parts *[3][]byte) { parts[1][0] ^= 0x01 },
				"signature": func(parts *[3][]byte) {
					parts[2][7] ^= 0x80
				},
				"public key": func(parts *[3][]byte) { parts[0][31] ^= 0x01 },
			} {
				pub := append([]byte{}, key.Public()...)
				msg := append([]byte{}, message...)
				sig := append([]byte{}, signature...)

				parts := [3][]byte{pub, msg, sig}
				mutate(&parts)

				fast := verifySchnorr(parts[0], parts[1], parts[2])
				slow := verifyBIP340(parts[0], parts[1], parts[2])

				if fast != slow {
					t.Fatalf("round %d %s: fast=%v slow=%v", round, name, fast, slow)
				}

				if fast {
					t.Fatalf("round %d: a corrupted %s still verified", round, name)
				}
			}
		}
	})

	t.Run("encodings the rules reject", func(t *testing.T) {
		pub, msg, sig := sampleVector(t)

		cases := map[string]struct {
			pub, msg, sig []byte
		}{
			"short key":     {pub[:31], msg, sig},
			"short sig":     {pub, msg, sig[:63]},
			"short message": {pub, msg[:31], sig},
			"empty":         {nil, nil, nil},
			// r >= p is not a signature, and s >= n is not either. Both are
			// rejected by the encoding rules rather than by the curve arithmetic.
			"r at the field prime": {pub, msg, append(bytes.Repeat([]byte{0xff}, 32),
				sig[32:]...)},
			"s at the group order": {pub, msg, append(append([]byte{}, sig[:32]...),
				bytes.Repeat([]byte{0xff}, 32)...)},
		}

		for name, c := range cases {
			if verifySchnorr(c.pub, c.msg, c.sig) {
				t.Fatalf("%s: an unencoded input verified", name)
			}

			if verifyBIP340(c.pub, c.msg, c.sig) {
				t.Fatalf("%s: the slow verifier accepted an unencoded input", name)
			}
		}
	})
}

// sampleVector is one official vector, for tests that need a valid triple.
func sampleVector(t *testing.T) (pub, msg, sig []byte) {
	t.Helper()

	if len(bip340Vectors) == 0 {
		t.Fatal("no vectors")
	}

	vector := bip340Vectors[0]

	pub, err := hex.DecodeString(vector.pubkey)

	if err != nil {
		t.Fatal(err)
	}

	msg, _ = hex.DecodeString(vector.message)

	decoded, err := hex.DecodeString(vector.sig)

	if err != nil {
		t.Fatal(err)
	}

	return pub, msg, decoded
}
