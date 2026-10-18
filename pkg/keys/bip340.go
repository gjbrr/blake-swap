// BIP-340 Schnorr verification, implemented here rather than delegated.
//
// This exists because the library verifier the client would otherwise use
// returns false for valid signatures when compiled by TinyGo, while its signing
// path produces byte-identical signatures to libsecp256k1. That asymmetry was
// found by running the official BIP-340 vector under `tinygo test -target=wasm`:
// signing matched, verification rejected. A verifier that rejects everything is
// a denial of service, so this file implements verification directly on
// math/big, which is the one arithmetic layer the port has already proved under
// TinyGo (the exact-decimal package is built on it).
//
// It is deliberately slow (Jacobian double-and-add with big.Int) and used only a
// few times per swap: verifying coordination events and settlement
// announcements. Correctness is worth more than speed here.
package keys

import (
	"crypto/sha256"
	"math/big"
)

var (
	// p is the secp256k1 field prime, 2^256 - 2^32 - 977.
	curveP = mustHexInt("fffffffffffffffffffffffffffffffffffffffffffffffffffffffefffffc2f")

	// n is the curve order.
	curveN = mustHexInt("fffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141")

	// curveB is the curve constant 7 (a = 0).
	curveB = big.NewInt(7)

	// seven is reused by the point arithmetic.
	two   = big.NewInt(2)
	three = big.NewInt(3)

	// sqrtExponent is (p+1)/4, the exponent that extracts a square root
	// because p = 3 mod 4.
	sqrtExponent = new(big.Int).Div(
		new(big.Int).Add(curveP, big.NewInt(1)),
		big.NewInt(4),
	)

	// gx, gy is the generator.
	gx = mustHexInt("79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798")
	gy = mustHexInt("483ada7726a3c4655da4fbfc0e1108a8fd17b448a68554199c47d08ffb10d4b8")
)

func mustHexInt(s string) *big.Int {
	value, ok := new(big.Int).SetString(s, 16)

	if !ok {
		panic("keys: bad curve constant")
	}

	return value
}

// point is a Jacobian point. Z == 0 means the point at infinity.
type point struct {
	x, y, z *big.Int
}

func infinity() point {
	return point{big.NewInt(0), big.NewInt(0), big.NewInt(0)}
}

func generator() point {
	return point{new(big.Int).Set(gx), new(big.Int).Set(gy), big.NewInt(1)}
}

func (p point) isInfinity() bool { return p.z.Sign() == 0 }

// mod reduces v modulo curveP into a fresh integer.
func mod(v *big.Int) *big.Int {
	out := new(big.Int).Mod(v, curveP)

	if out.Sign() < 0 {
		out.Add(out, curveP)
	}

	return out
}

func mulMod(a, b *big.Int) *big.Int {
	return mod(new(big.Int).Mul(a, b))
}

func addMod(a, b *big.Int) *big.Int {
	return mod(new(big.Int).Add(a, b))
}

func subMod(a, b *big.Int) *big.Int {
	return mod(new(big.Int).Sub(a, b))
}

// double returns 2*p.
func (p point) double() point {
	if p.isInfinity() || p.y.Sign() == 0 {
		return infinity()
	}

	// lambda = 3x^2 / 2y
	numerator := mulMod(three, mulMod(p.x, p.x))
	denominator := mulMod(two, p.y)

	lambda := mulMod(numerator, new(big.Int).ModInverse(denominator, curveP))

	x3 := subMod(mulMod(lambda, lambda), mulMod(two, p.x))
	y3 := subMod(mulMod(lambda, subMod(p.x, x3)), p.y)

	return point{x3, y3, big.NewInt(1)}
}

// add returns p+q.
//
// The affine formulas are used with an explicit inverse; this is called from
// double-and-add, so the cost is acceptable for the handful of verifications a
// swap performs.
func (p point) add(q point) point {
	if p.isInfinity() {
		return q
	}

	if q.isInfinity() {
		return p
	}

	if p.x.Cmp(q.x) == 0 {
		if p.y.Cmp(q.y) == 0 {
			return p.double()
		}

		// Same x, opposite y: the sum is infinity.
		return infinity()
	}

	// lambda = (qy - py) / (qx - px)
	numerator := subMod(q.y, p.y)
	denominator := subMod(q.x, p.x)

	lambda := mulMod(numerator, new(big.Int).ModInverse(denominator, curveP))

	x3 := subMod(subMod(mulMod(lambda, lambda), p.x), q.x)
	y3 := subMod(mulMod(lambda, subMod(p.x, x3)), p.y)

	return point{x3, y3, big.NewInt(1)}
}

// negate returns -p.
func (p point) negate() point {
	if p.isInfinity() {
		return p
	}

	return point{new(big.Int).Set(p.x), subMod(big.NewInt(0), p.y), new(big.Int).Set(p.z)}
}

// scalarMult returns k*p for a non-negative k.
func scalarMult(k *big.Int, p point) point {
	result := infinity()
	addend := p

	for i := 0; i < k.BitLen(); i++ {
		if k.Bit(i) == 1 {
			result = result.add(addend)
		}

		addend = addend.double()
	}

	return result
}

// taggedHash is BIP-340's domain-separated hash.
func taggedHash(tag string, parts ...[]byte) *big.Int {
	tagHash := sha256.Sum256([]byte(tag))

	hasher := sha256.New()
	hasher.Write(tagHash[:])
	hasher.Write(tagHash[:])

	for _, part := range parts {
		hasher.Write(part)
	}

	return new(big.Int).SetBytes(hasher.Sum(nil))
}

// liftX returns the point with the given x-coordinate and even y, or nil when no
// such point exists.
func liftX(xBytes []byte) *point {
	if len(xBytes) != 32 {
		return nil
	}

	x := new(big.Int).SetBytes(xBytes)

	if x.Cmp(curveP) >= 0 {
		return nil
	}

	// c = x^3 + 7
	c := addMod(mulMod(mulMod(x, x), x), curveB)

	// y = c^((p+1)/4); valid only when y^2 == c.
	y := new(big.Int).Exp(c, sqrtExponent, curveP)

	if mulMod(y, y).Cmp(c) != 0 {
		return nil
	}

	// Take the even y, as BIP-340 requires.
	if y.Bit(0) == 1 {
		y = subMod(big.NewInt(0), y)
	}

	return &point{x, y, big.NewInt(1)}
}

// verifyBIP340 checks a 64-byte BIP-340 signature over a 32-byte message for a
// 32-byte x-only public key.
func verifyBIP340(publicX, message, signature []byte) bool {
	if len(publicX) != 32 || len(message) != 32 || len(signature) != 64 {
		return false
	}

	r := new(big.Int).SetBytes(signature[:32])
	s := new(big.Int).SetBytes(signature[32:])

	if r.Cmp(curveP) >= 0 || s.Cmp(curveN) >= 0 {
		return false
	}

	publicPoint := liftX(publicX)

	if publicPoint == nil {
		return false
	}

	// e = int(hash("BIP0340/challenge", r || pubkey || message)) mod n
	e := taggedHash("BIP0340/challenge", signature[:32], publicX, message)
	e.Mod(e, curveN)

	// R = s*G - e*P
	negE := new(big.Int).Sub(curveN, e)
	negE.Mod(negE, curveN)

	rPoint := scalarMult(s, generator()).add(scalarMult(negE, *publicPoint))

	if rPoint.isInfinity() {
		return false
	}

	// R must have even y, and R.x must equal r.
	if rPoint.y.Bit(0) == 1 {
		return false
	}

	return rPoint.x.Cmp(r) == 0
}
