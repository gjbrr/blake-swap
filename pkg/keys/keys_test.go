package keys

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func secret(fill byte) []byte { return bytes.Repeat([]byte{fill}, SecretSize) }

func TestDeterministicPublicKey(t *testing.T) {
	a, err := FromSecret(secret(1))

	if err != nil {
		t.Fatal(err)
	}

	b, err := FromSecret(secret(1))

	if err != nil {
		t.Fatal(err)
	}

	if a.PublicHex() != b.PublicHex() {
		t.Fatal("the same secret produced two public keys")
	}

	if len(a.Public()) != PublicSize {
		t.Fatalf("public key is %d bytes", len(a.Public()))
	}

	if len(a.PublicHex()) != 64 {
		t.Fatalf("public hex is %d characters", len(a.PublicHex()))
	}

	// A different secret must produce a different key.
	c, _ := FromSecret(secret(2))

	if c.PublicHex() == a.PublicHex() {
		t.Fatal("two secrets produced the same public key")
	}

	if _, err := FromSecret(secret(1)[:31]); err == nil {
		t.Fatal("a short secret was accepted")
	}

	if _, err := FromSecretHex("zz"); err == nil {
		t.Fatal("a non-hex secret was accepted")
	}
}

func TestSignVerify(t *testing.T) {
	key, err := FromSecret(secret(7))

	if err != nil {
		t.Fatal(err)
	}

	message := sha256.Sum256([]byte("hello world"))

	signature, err := key.Sign(message[:])

	if err != nil {
		t.Fatal(err)
	}

	if len(signature) != SignatureSize {
		t.Fatalf("signature is %d bytes", len(signature))
	}

	valid, err := VerifySchnorr(key.PublicHex(), message[:], signature)

	if err != nil || !valid {
		t.Fatalf("signature did not verify: %v %v", valid, err)
	}

	// A different message must not verify.
	other := sha256.Sum256([]byte("hello world!"))

	if valid, _ := VerifySchnorr(key.PublicHex(), other[:], signature); valid {
		t.Fatal("signature verified for another message")
	}

	// A tampered signature must not verify.
	tampered := append([]byte{}, signature...)
	tampered[0] ^= 0xff

	if valid, _ := VerifySchnorr(key.PublicHex(), message[:], tampered); valid {
		t.Fatal("tampered signature verified")
	}

	// A signature from one key must not verify under another.
	otherKey, _ := FromSecret(secret(8))

	if valid, _ := VerifySchnorr(otherKey.PublicHex(), message[:], signature); valid {
		t.Fatal("signature verified under the wrong key")
	}
}

func TestSignatureCopiesAreIndependent(t *testing.T) {
	// The underlying signer reuses its buffer; two retained signatures must
	// not alias.
	key, _ := FromSecret(secret(3))
	message := sha256.Sum256([]byte("one"))

	first, err := key.Sign(message[:])

	if err != nil {
		t.Fatal(err)
	}

	second, err := key.Sign(message[:])

	if err != nil {
		t.Fatal(err)
	}

	first[0] ^= 0xff

	if bytes.Equal(first, second) {
		t.Fatal("the two signatures share a buffer")
	}

	// The unmodified second signature still verifies.
	valid, err := VerifySchnorr(key.PublicHex(), message[:], second)

	if err != nil || !valid {
		t.Fatalf("second signature did not verify: %v %v", valid, err)
	}
}

func TestSharedX(t *testing.T) {
	alice, _ := FromSecret(secret(1))
	bob, _ := FromSecret(secret(2))

	aliceShared, err := alice.SharedX(bob.PublicHex())

	if err != nil {
		t.Fatal(err)
	}

	bobShared, err := bob.SharedX(alice.PublicHex())

	if err != nil {
		t.Fatal(err)
	}

	if len(aliceShared) != 32 {
		t.Fatalf("shared secret is %d bytes", len(aliceShared))
	}

	// ECDH is symmetric.
	if !bytes.Equal(aliceShared, bobShared) {
		t.Fatal("the shared x-coordinate differs between the two sides")
	}

	if _, err := alice.SharedX("00"); err == nil {
		t.Fatal("a malformed peer key was accepted")
	}

	if _, err := alice.SharedX(strings.Repeat("00", 32)); err == nil {
		t.Fatal("an invalid curve point was accepted")
	}
}

func TestSecretRoundTrip(t *testing.T) {
	raw := secret(9)

	key, err := FromSecret(raw)

	if err != nil {
		t.Fatal(err)
	}

	// The normalised secret is what the signer holds; for an even-Y key it is
	// the same bytes.
	roundTripped, err := FromSecretHex(key.SecretHex())

	if err != nil {
		t.Fatal(err)
	}

	if roundTripped.PublicHex() != key.PublicHex() {
		t.Fatal("re-importing the secret changed the public key")
	}
}

func TestGenerateSecret(t *testing.T) {
	source := bytes.NewReader(secret(5))

	generated, err := GenerateSecret(source)

	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(generated, secret(5)) {
		t.Fatal("GenerateSecret changed the bytes")
	}

	if _, err := GenerateSecret(bytes.NewReader([]byte{1, 2, 3})); err == nil {
		t.Fatal("a short read was accepted")
	}
}

func TestEqualSecrets(t *testing.T) {
	if !EqualSecrets(secret(1), secret(1)) {
		t.Fatal("equal secrets compared unequal")
	}

	if EqualSecrets(secret(1), secret(2)) {
		t.Fatal("different secrets compared equal")
	}

	if EqualSecrets(secret(1), secret(1)[:31]) {
		t.Fatal("different lengths compared equal")
	}
}

// bip340Vectors were produced by libsecp256k1 (coincurve 21.0.0) with
// deterministic aux randomness, so they are an external authority rather than
// this implementation's own output. The third case's full public key has an odd
// y, which is what exercises the x-only even-y lift.
var bip340Vectors = []struct {
	name    string
	secret  string
	pubkey  string
	message string
	sig     string
}{
	{
		name:    "bip340 vector 0 (all zeros)",
		secret:  "0000000000000000000000000000000000000000000000000000000000000003",
		pubkey:  "f9308a019258c31049344f85f89d5229b531c845836f99b08601f113bce036f9",
		message: "0000000000000000000000000000000000000000000000000000000000000000",
		sig:     "e907831f80848d1069a5371b402410364bdf1c5f8307b0084c55f1ce2dca821525f66a4a85ea8b71e482a74f382d2ce5ebeee8fdb2172f477df4900d310536c0",
	},
	{
		name:    "secret 1, sha256(hello)",
		secret:  "0000000000000000000000000000000000000000000000000000000000000001",
		pubkey:  "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798",
		message: "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
		sig:     "fd7ebe40e8b284f1cb604240c9ea503a7ba5c57c86f0db4a14fb13f4743394d96625d34a90a000ade4327cfc49f74809d0a1f06b3e233ce1cd7043fce86da4ec",
	},
	{
		name:    "odd-y public key (x-only lift is exercised)",
		secret:  "1111111111111111111111111111111111111111111111111111111111111111",
		pubkey:  "4f355bdcb7cc0af728ef3cceb9615d90684bb5b2ca5f859ab0f0b704075871aa",
		message: "010247220adc988ecc1c58392e4222c0bd257e64170adc8acd234d733eff7258",
		sig:     "c9265cfb528798a43dc4b39e42450ba6dadfec5b140177bce3d947bc6bfadf09a58f7c844e9cf8348d8f6b038d3783a7f57683b102042c33ac00a342344b4543",
	},
}

func TestBIP340Vectors(t *testing.T) {
	for _, vector := range bip340Vectors {
		t.Run(vector.name, func(t *testing.T) {
			// The signature must verify.
			sig, _ := hex.DecodeString(vector.sig)
			msg, _ := hex.DecodeString(vector.message)

			valid, err := VerifySchnorr(vector.pubkey, msg, sig)

			if err != nil {
				t.Fatal(err)
			}

			if !valid {
				t.Fatal("a valid signature was rejected")
			}

			// Signing the same message with the same secret must reproduce
			// the vector: deterministic signing is what makes the two halves
			// of this file cross-check each other.
			key, err := FromSecretHex(vector.secret)

			if err != nil {
				t.Fatal(err)
			}

			if key.PublicHex() != vector.pubkey {
				t.Fatalf("public key = %s, want %s", key.PublicHex(), vector.pubkey)
			}

			own, err := key.Sign(msg)

			if err != nil {
				t.Fatal(err)
			}

			if hex.EncodeToString(own) != vector.sig {
				t.Fatalf("signature = %x, want %s", own, vector.sig)
			}

			if valid, err := key.Verify(msg, own); err != nil || !valid {
				t.Fatalf("own signature did not verify: %v %v", valid, err)
			}
		})
	}
}

func TestBIP340Refusals(t *testing.T) {
	vector := bip340Vectors[0]

	sig, _ := hex.DecodeString(vector.sig)
	msg, _ := hex.DecodeString(vector.message)

	// Wrong message.
	other := make([]byte, 32)
	other[0] = 1

	if ok, _ := VerifySchnorr(vector.pubkey, other, sig); ok {
		t.Fatal("a signature verified for the wrong message")
	}

	// Wrong public key.
	key2, _ := FromSecret(secret(2))

	if ok, _ := VerifySchnorr(key2.PublicHex(), msg, sig); ok {
		t.Fatal("a signature verified for the wrong key")
	}

	// Tampered signature.
	tampered := append([]byte{}, sig...)
	tampered[63] ^= 1

	if ok, _ := VerifySchnorr(vector.pubkey, msg, tampered); ok {
		t.Fatal("a tampered signature verified")
	}

	// r >= p and s >= n are structurally invalid.
	badR := append([]byte{}, sig...)
	copy(badR[:32], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})

	if ok, _ := VerifySchnorr(vector.pubkey, msg, badR); ok {
		t.Fatal("r >= p verified")
	}

	badS := append([]byte{}, sig...)
	copy(badS[32:], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xfe,
		0xba, 0xae, 0xdc, 0xe6, 0xaf, 0x48, 0xa0, 0x3b,
		0xbf, 0xd2, 0x5e, 0x8c, 0xd0, 0x36, 0x41, 0x41})

	if ok, _ := VerifySchnorr(vector.pubkey, msg, badS); ok {
		t.Fatal("s >= n verified")
	}

	// Wrong lengths.
	if ok, _ := VerifySchnorr(vector.pubkey, msg[:31], sig); ok {
		t.Fatal("a short message verified")
	}

	if ok, _ := VerifySchnorr(vector.pubkey, msg, sig[:63]); ok {
		t.Fatal("a short signature verified")
	}
}
