package vault

import (
	"bytes"
	"strings"
	"testing"
)

// reader returns a deterministic byte stream for the nonces and salts.
type reader struct{ b []byte }

func (r *reader) Read(p []byte) (int, error) {
	n := copy(p, r.b)
	r.b = r.b[n:]

	return n, nil
}

func stream(fill byte, n int) *reader {
	return &reader{b: bytes.Repeat([]byte{fill}, n)}
}

// fastParams keeps the tests quick while still above the accepted minimum.
func fastParams(t *testing.T) Params {
	t.Helper()

	salt := make([]byte, SaltSize)

	for i := range salt {
		salt[i] = byte(i)
	}

	return Params{Iterations: MinIterations, Salt: salt}
}

func TestDeriveKeyIsDeterministic(t *testing.T) {
	p := fastParams(t)

	first, err := DeriveKey("correct horse battery staple", p)

	if err != nil {
		t.Fatal(err)
	}

	second, err := DeriveKey("correct horse battery staple", p)

	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(first, second) {
		t.Fatal("the same passphrase produced two keys")
	}

	if len(first) != KeySize {
		t.Fatalf("key is %d bytes", len(first))
	}

	// A different passphrase, salt or iteration count produces a different key.
	other, err := DeriveKey("wrong horse battery staple", p)

	if err != nil {
		t.Fatal(err)
	}

	if bytes.Equal(first, other) {
		t.Fatal("a different passphrase produced the same key")
	}

	p2 := p
	p2.Salt = append([]byte{}, p.Salt...)
	p2.Salt[0] ^= 0xff

	other, err = DeriveKey("correct horse battery staple", p2)

	if err != nil {
		t.Fatal(err)
	}

	if bytes.Equal(first, other) {
		t.Fatal("a different salt produced the same key")
	}
}

func TestDeriveKeyRefusals(t *testing.T) {
	p := fastParams(t)

	if _, err := DeriveKey("", p); err == nil {
		t.Fatal("an empty passphrase was accepted")
	}

	short := p
	short.Salt = short.Salt[:16]

	if _, err := DeriveKey("pass", short); err == nil {
		t.Fatal("a short salt was accepted")
	}

	weak := p
	weak.Iterations = MinIterations - 1

	if _, err := DeriveKey("pass", weak); err == nil {
		t.Fatal("a weak iteration count was accepted")
	}

	huge := p
	huge.Iterations = MaxIterations + 1

	if _, err := DeriveKey("pass", huge); err == nil {
		t.Fatal("an implausible iteration count was accepted")
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	key, err := DeriveKey("passphrase", fastParams(t))

	if err != nil {
		t.Fatal(err)
	}

	plaintext := []byte(`{"swap_id":"abcdef123456","secret_hex":"aa"}`)

	payload, err := Seal(key, "alice/abcdef123456", plaintext, stream(7, 64))

	if err != nil {
		t.Fatal(err)
	}

	// The payload looks like a NIP-44 payload, which is the point.
	if payload[0] != 'A' {
		t.Fatalf("payload starts with %q", payload[0])
	}

	if strings.Contains(payload, "abcdef123456") {
		t.Fatal("payload leaked the record key")
	}

	opened, err := Open(key, "alice/abcdef123456", payload)

	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(opened, plaintext) {
		t.Fatalf("opened %q, want %q", opened, plaintext)
	}
}

func TestOpenRefusals(t *testing.T) {
	key, _ := DeriveKey("passphrase", fastParams(t))
	otherKey, _ := DeriveKey("other passphrase", fastParams(t))

	payload, err := Seal(key, "alice/abcdef123456", []byte("secret state"), stream(9, 64))

	if err != nil {
		t.Fatal(err)
	}

	// The wrong passphrase fails the MAC.
	if _, err := Open(otherKey, "alice/abcdef123456", payload); err == nil {
		t.Fatal("the wrong passphrase opened the record")
	}

	// The right key but the wrong record binding is refused by name: this is
	// the property that stops a ciphertext being moved between records.
	if _, err := Open(key, "bob/abcdef123456", payload); err == nil ||
		!strings.Contains(err.Error(), "different record") {
		t.Fatalf("wrong record key: %v", err)
	}

	// A tampered payload fails the MAC.
	raw := []byte(payload)
	raw[len(raw)/2] ^= 0x01

	if _, err := Open(key, "alice/abcdef123456", string(raw)); err == nil {
		t.Fatal("a tampered payload opened")
	}

	// An empty record key is refused on both sides.
	if _, err := Open(key, "", payload); err == nil {
		t.Fatal("an empty record key opened")
	}

	if _, err := Seal(key, "", []byte("x"), stream(1, 64)); err == nil {
		t.Fatal("sealing with an empty record key was accepted")
	}
}

func TestSealUsesAFreshNonce(t *testing.T) {
	key, _ := DeriveKey("passphrase", fastParams(t))

	first, err := Seal(key, "k", []byte("same plaintext"), stream(1, 64))

	if err != nil {
		t.Fatal(err)
	}

	second, err := Seal(key, "k", []byte("same plaintext"), stream(2, 64))

	if err != nil {
		t.Fatal(err)
	}

	if first == second {
		t.Fatal("two seals produced the same payload, so the nonce was reused")
	}
}

func TestHeaderRoundTrip(t *testing.T) {
	params, err := NewParams(stream(3, 64))

	if err != nil {
		t.Fatal(err)
	}

	if params.Iterations != DefaultIterations {
		t.Fatalf("iterations = %d", params.Iterations)
	}

	encoded, err := params.MarshalHeader()

	if err != nil {
		t.Fatal(err)
	}

	// The header is not secret: it is a KDF descriptor and a salt.
	if strings.Contains(string(encoded), "passphrase") {
		t.Fatal("header leaked the passphrase")
	}

	decoded, err := UnmarshalHeader(encoded)

	if err != nil {
		t.Fatal(err)
	}

	if decoded.Iterations != params.Iterations || !bytes.Equal(decoded.Salt, params.Salt) {
		t.Fatalf("header round trip changed the parameters: %+v", decoded)
	}
}

func TestHeaderRefusals(t *testing.T) {
	cases := map[string]Header{
		"future version":     {Version: 2, KDF: "pbkdf2-hmac-sha256", Iterations: DefaultIterations, Salt: strings.Repeat("aa", 32)},
		"unknown kdf":        {Version: 1, KDF: "argon2", Iterations: DefaultIterations, Salt: strings.Repeat("aa", 32)},
		"weak iterations":    {Version: 1, KDF: "pbkdf2-hmac-sha256", Iterations: 1, Salt: strings.Repeat("aa", 32)},
		"huge iterations":    {Version: 1, KDF: "pbkdf2-hmac-sha256", Iterations: MaxIterations + 1, Salt: strings.Repeat("aa", 32)},
		"short salt":         {Version: 1, KDF: "pbkdf2-hmac-sha256", Iterations: DefaultIterations, Salt: "aabb"},
		"non-hex salt":       {Version: 1, KDF: "pbkdf2-hmac-sha256", Iterations: DefaultIterations, Salt: strings.Repeat("zz", 32)},
		"missing everything": {},
	}

	for name, header := range cases {
		if _, err := ParamsFromHeader(header); err == nil {
			t.Errorf("%s: header was accepted", name)
		}
	}

	if _, err := UnmarshalHeader([]byte("not json")); err == nil {
		t.Fatal("a non-JSON header was accepted")
	}
}
