package keys

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

// Verification has two implementations and they are both kept.
//
// btcec carries the same pure-Go 5x52/10x26 field arithmetic that orly's portable
// code was adapted from, and it is substantially faster than math/big under
// TinyGo — the target that matters here, because every market and coordination
// event the browser accepts is verified there. `verifyBIP340` in bip340.go stays
// for two jobs: it is the independent cross-check the tests run against this path,
// and it is what makes the port's verifier auditable against the BIP-340 text
// rather than against a dependency.
//
// The two are proved to agree on the official vectors and on random keys, valid
// and invalid signatures, in TestFastAndSlowVerifiersAgree.

// verifySchnorr verifies a BIP-340 signature over a 32-byte message.
//
// A public key or signature that does not parse is not a valid one: BIP-340's
// encodings are part of its verification rules, so a parse failure is `false`
// rather than an error.
func verifySchnorr(publicX, message, signature []byte) bool {
	if len(publicX) != PublicSize || len(message) != 32 || len(signature) != SignatureSize {
		return false
	}

	pub, err := schnorr.ParsePubKey(publicX)

	if err != nil {
		return false
	}

	parsed, err := schnorr.ParseSignature(signature)

	if err != nil {
		return false
	}

	return parsed.Verify(message, pub)
}

// signSchnorrWithAux signs a 32-byte message with an explicit BIP-340
// auxiliary randomness, which is how the official vectors pin a signature.
//
// It exists for the tests: production signing goes through orly's signer, whose
// output is byte-identical to libsecp256k1 and is what the rest of the protocol
// already agrees on.
func signSchnorrWithAux(secret, message, aux []byte) ([]byte, bool) {
	priv, _ := btcec.PrivKeyFromBytes(secret)

	if priv == nil {
		return nil, false
	}

	var nonce [32]byte
	copy(nonce[:], aux)

	signature, err := schnorr.Sign(priv, message, schnorr.CustomNonce(nonce))

	if err != nil {
		return nil, false
	}

	return signature.Serialize(), true
}
