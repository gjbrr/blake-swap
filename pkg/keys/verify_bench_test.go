package keys

import (
	"encoding/hex"
	"testing"
)

// These two benchmarks exist because the choice between them is a performance
// decision, and a decision without a measurement is a preference. Run them with
// `go test ./pkg/keys -bench Verify`. The TinyGo target does not take a -bench
// flag, so the ratio below is the host's; the shape is the same in the browser,
// where the absolute numbers are larger and matter more.
func benchmarkVerify(b *testing.B, verify func(pub, msg, sig []byte) bool) {
	b.Helper()

	vector := bip340Vectors[0]

	pub, err := hex.DecodeString(vector.pubkey)

	if err != nil {
		b.Fatal(err)
	}

	msg, _ := hex.DecodeString(vector.message)

	sig, err := hex.DecodeString(vector.sig)

	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if !verify(pub, msg, sig) {
			b.Fatal("a valid vector was rejected")
		}
	}
}

func BenchmarkVerifyFast(b *testing.B) { benchmarkVerify(b, verifySchnorr) }

func BenchmarkVerifySlow(b *testing.B) { benchmarkVerify(b, verifyBIP340) }
