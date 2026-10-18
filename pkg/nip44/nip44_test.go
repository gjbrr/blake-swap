package nip44

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/keys"
)

// conversationVectors are the official NIP-44 v2 vectors (paulmillr/nip44),
// including the two edge cases: sec1 = n-2 against an x-only peer key whose x is
// 2, and sec1 = the generator against the generator's x.
var conversationVectors = []struct {
	secret  string
	peer    string
	wantKey string
}{
	{
		"315e59ff51cb9209768cf7da80791ddcaae56ac9775eb25b6dee1234bc5d2268",
		"c2f9d9948dc8c7c38321e4b85c8558872eafa0641cd269db76848a6073e69133",
		"3dfef0ce2a4d80a25e7a328accf73448ef67096f65f79588e358d9a0eb9013f1",
	},
	{
		"0e44e2d1db3c1717b05ffa0f08d102a09c554a1cbbf678ab158b259a44e682f1",
		"1ffa76c5cc7a836af6914b840483726207cb750889753d7499fb8b76aa8fe0de",
		"a39970a667b7f861f100e3827f4adbf6f464e2697686fe1a81aeda817d6b8bdf",
	},
	{
		"a28eed0fe977893856ab9667e06ace39f03abbcdb845c329a1981be438ba565d",
		"b0f38b950a5013eba5ab4237f9ed29204a59f3625c71b7e210fec565edfa288c",
		"9d3a802b45bc5aeeb3b303e8e18a92ddd353375710a31600d7f5fff8f3a7285b",
	},
	{
		"fffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364139",
		"0000000000000000000000000000000000000000000000000000000000000002",
		"8b6392dbf2ec6a2b2d5b1477fc2be84d63ef254b667cadd31bd3f444c44ae6ba",
	},
	{
		"0000000000000000000000000000000000000000000000000000000000000001",
		"79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798",
		"3b4610cb7189beb9cc29eb3716ecc6102f1247e8f3101a03a1787d8908aeb54e",
	},
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()

	raw, err := hex.DecodeString(s)

	if err != nil {
		t.Fatal(err)
	}

	return raw
}

func TestConversationKeyVectors(t *testing.T) {
	for _, vector := range conversationVectors {
		got, err := ConversationKey(unhex(t, vector.secret), unhex(t, vector.peer))

		if err != nil {
			t.Fatalf("%s: %v", vector.secret[:8], err)
		}

		if hex.EncodeToString(got) != vector.wantKey {
			t.Errorf("conversation key for %s:\n got %x\nwant %s",
				vector.secret[:8], got, vector.wantKey)
		}
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := unhex(t, conversationVectors[0].wantKey)
	nonce := unhex(t, strings.Repeat("ab", 32))

	payload, err := Encrypt(key, []byte("hello world"), nonce)

	if err != nil {
		t.Fatal(err)
	}

	// The payload is v2 base64: it starts with A (0x02) and is long enough.
	if payload[0] != 'A' {
		t.Fatalf("payload starts with %q, want 'A'", payload[0])
	}

	if len(payload) < MinBase64Chars {
		t.Fatalf("payload is %d characters", len(payload))
	}

	raw, err := base64.StdEncoding.DecodeString(payload)

	if err != nil {
		t.Fatal(err)
	}

	if raw[0] != Version {
		t.Fatalf("version byte = %d", raw[0])
	}

	plaintext, err := Decrypt(key, payload)

	if err != nil {
		t.Fatal(err)
	}

	if plaintext != "hello world" {
		t.Fatalf("plaintext = %q", plaintext)
	}

	// The same nonce and plaintext must reproduce the same payload, which is
	// what makes the construction testable at all.
	again, err := Encrypt(key, []byte("hello world"), nonce)

	if err != nil {
		t.Fatal(err)
	}

	if again != payload {
		t.Fatal("the same inputs produced a different payload")
	}

	// A different nonce produces a different payload.
	otherNonce := unhex(t, strings.Repeat("cd", 32))

	different, err := Encrypt(key, []byte("hello world"), otherNonce)

	if err != nil {
		t.Fatal(err)
	}

	if different == payload {
		t.Fatal("a different nonce produced the same payload")
	}
}

func TestDecryptRefusals(t *testing.T) {
	key := unhex(t, conversationVectors[0].wantKey)
	nonce := unhex(t, strings.Repeat("ab", 32))

	payload, err := Encrypt(key, []byte("secret"), nonce)

	if err != nil {
		t.Fatal(err)
	}

	// A different key must fail the MAC.
	wrongKey := unhex(t, conversationVectors[1].wantKey)

	if _, err := Decrypt(wrongKey, payload); err == nil {
		t.Fatal("the wrong key decrypted a payload")
	}

	// A tampered ciphertext must fail the MAC.
	raw, _ := base64.StdEncoding.DecodeString(payload)
	raw[len(raw)/2] ^= 0xff

	tampered := base64.StdEncoding.EncodeToString(raw)

	if _, err := Decrypt(key, tampered); err == nil {
		t.Fatal("a tampered payload decrypted")
	}

	// A payload whose version byte is not 2 is refused by name.
	raw, _ = base64.StdEncoding.DecodeString(payload)
	raw[0] = 3

	if _, err := Decrypt(key, base64.StdEncoding.EncodeToString(raw)); err == nil ||
		!strings.Contains(err.Error(), "unsupported NIP-44 version") {
		t.Fatalf("future version: %v", err)
	}

	// Structural refusals.
	for _, bad := range []string{
		"",
		"#future",
		"AAAA",
		"not base64!!",
	} {
		if _, err := Decrypt(key, bad); err == nil {
			t.Errorf("payload %q decrypted", bad)
		}
	}

	if _, err := Decrypt([]byte{1, 2, 3}, payload); err == nil {
		t.Error("a short conversation key was accepted")
	}
}

func TestLooksEncrypted(t *testing.T) {
	key := unhex(t, conversationVectors[0].wantKey)
	nonce := unhex(t, strings.Repeat("ab", 32))

	payload, err := Encrypt(key, []byte("hello"), nonce)

	if err != nil {
		t.Fatal(err)
	}

	if !LooksEncrypted(payload) {
		t.Fatal("a real payload does not look encrypted")
	}

	// A JSON message is never mistaken for a payload: `{` is not base64.
	if LooksEncrypted(`{"type":"offer"}`) {
		t.Fatal("JSON looked encrypted")
	}

	for _, bad := range []string{
		"",
		"#future",
		" " + payload,
		strings.Repeat("A", MinBase64Chars-1),
		"!!!! not base64 !!!!",
	} {
		if LooksEncrypted(bad) {
			t.Errorf("%q looked encrypted", bad)
		}
	}
}

func TestDescribePayload(t *testing.T) {
	key := unhex(t, conversationVectors[0].wantKey)
	nonce := unhex(t, strings.Repeat("ab", 32))

	payload, err := Encrypt(key, []byte("hello"), nonce)

	if err != nil {
		t.Fatal(err)
	}

	description := DescribePayload(payload)

	if !strings.Contains(description, "NIP-44 v2") {
		t.Fatalf("description = %q", description)
	}

	if strings.Contains(description, "hello") {
		t.Fatal("description leaked plaintext")
	}

	// It must never panic, even on rubbish.
	if DescribePayload("###") == "" {
		t.Fatal("description of rubbish is empty")
	}
}

func TestConversationKeyIsSymmetric(t *testing.T) {
	// The vectors publish one secret and an unrelated peer key, so the public
	// halves are derived here to test the symmetry that actually matters: the
	// two sides of one pair must compute the same conversation key.
	a := unhex(t, conversationVectors[0].secret)
	b := unhex(t, conversationVectors[1].secret)

	aKey, err := keys.FromSecret(a)

	if err != nil {
		t.Fatal(err)
	}

	bKey, err := keys.FromSecret(b)

	if err != nil {
		t.Fatal(err)
	}

	ab, err := ConversationKey(a, bKey.Public())

	if err != nil {
		t.Fatal(err)
	}

	ba, err := ConversationKey(b, aKey.Public())

	if err != nil {
		t.Fatal(err)
	}

	// The two sides of one pair agree, which is what makes a conversation
	// possible at all.
	if hex.EncodeToString(ab) != hex.EncodeToString(ba) {
		t.Fatalf("the two sides disagree:\n%x\n%x", ab, ba)
	}
}
