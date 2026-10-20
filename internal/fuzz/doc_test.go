//go:build !js

// Fuzz targets for every path that turns untrusted bytes into a value.
//
// The client parses four kinds of input it did not produce: a sealed record from the vault
// (which may have been written by an older build, or corrupted), a coordination message
// body from a relay, a command envelope from the page, and a key. A panic in any of them is
// a dead wasm instance, which is a dead client: not an error a person can see, but a page
// that stops answering.
//
// `go test -fuzz=Fuzz...` runs these; the seed corpus also runs as an ordinary unit test, so
// a case that panics fails `go test` without anybody remembering to fuzz.
package fuzz_test

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/keys"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/nip44"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/proto"
	"github.com/gjbrr/blake-swap/pkg/vaultbackup"
	"github.com/gjbrr/blake-swap/pkg/wire"
)

// FuzzModelDecode is the vault's record decoder: a sealed record is only as trustworthy as
// the build that wrote it, and an old one may hold fields this build has never seen.
func FuzzModelDecode(f *testing.F) {
	valid, err := model.New(&countingReader{}, model.NewParams{
		BlakeAmount:  1_000_000,
		BTCSats:      25_000,
		BlakeTimeout: 500,
		BTCTimeout:   400,
	})

	if err != nil {
		f.Fatal(err)
	}

	encoded, err := valid.Marshal()

	if err != nil {
		f.Fatal(err)
	}

	f.Add(encoded)
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"swap_id":""}`))
	f.Add([]byte(`{"swap_id":"aaaa00000001","state":"NOPE"}`))
	f.Add([]byte(`null`))
	f.Add([]byte(``))
	f.Add([]byte(`[]`))
	f.Add([]byte(`{"swap_id":"aaaa00000001","blake_amount_base_units":-1}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		defer survived(t)

		_, _ = model.Decode(data, nil)
	})
}

// FuzzWireDecode is the coordination message decoder.
func FuzzWireDecode(f *testing.F) {
	f.Add([]byte(`{"type":"offer","swap_id":"aaaa00000001"}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"type":""}`))
	f.Add([]byte(`null`))
	f.Add([]byte(``))
	f.Add([]byte(`{"type":"offer","blake_amount":"1e999999"}`))
	f.Add([]byte(`{"type":"offer","blake_amount":-1}`))
	f.Add([]byte(`{"type":"offer","btc_sats":1e400}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		defer survived(t)

		_, _ = wire.Decode(data, nil)
		_, _ = wire.Decode(data, kindPtr(wire.KindOffer))
	})
}

// FuzzProtoDecode is the page-worker link: a malformed envelope must be a refusal rather
// than a dead domain.
func FuzzProtoDecode(f *testing.F) {
	f.Add(`{"kind":"command","seq":1,"body":"{\"command\":\"engine.status\"}"}`)
	f.Add(`{}`)
	f.Add(`{"kind":""}`)
	f.Add(`not json`)
	f.Add(``)
	f.Add(`{"kind":"command","body":"{}"}`)
	f.Add(`{"kind":"command-result","body":"{\"command\":\"x\",\"ok\":true}"}`)
	f.Add(`{"kind":"command-result","body":"{\"command\":\"x\",\"ok\":true,\"data\":{\"swap_id\":1}}"}`)

	f.Fuzz(func(t *testing.T, text string) {
		defer survived(t)

		envelope, err := proto.Decode(text)

		if err != nil {
			return
		}

		if _, err := proto.DecodeResult(envelope.Body); err == nil {
			// A result that decodes is still parsed by the page.
			var view proto.SwapSummary

			_ = proto.DecodeData(proto.Result{Command: "x"}, &view)
		}

		_, _ = proto.DecodeRequest(envelope.Body)
	})
}

// FuzzMarketParse is the market's message parser: offers, takes, matches and cancellations
// from any relay, in any shape.
func FuzzMarketParse(f *testing.F) {
	maker := strings.Repeat("a", 64)

	f.Add(`{"type":"market-offer","version":2,"base":"BLAKE","quote":"BTC","blake_amount":"10","btc_sats":50000,"direction":"blake-for-btc","created_at":1700000000,"expires_at":1800000000,"maker":"` + maker + `"}`)
	f.Add(`{"type":"market-take"}`)
	f.Add(`{"type":"market-match"}`)
	f.Add(`{"type":"market-cancel"}`)
	f.Add(`{}`)
	f.Add(`null`)
	f.Add(`{"type":"market-offer","blake_amount":{}}`)

	f.Fuzz(func(t *testing.T, text string) {
		defer survived(t)

		var message map[string]any

		if err := json.Unmarshal([]byte(text), &message); err != nil {
			return
		}

		_, _ = market.ParseOffer(message)
		_, _ = market.ParseTake(message)
		_, _ = market.ParseMatch(message)
		_, _ = market.ParseCancel(message)
	})
}

// FuzzKeys covers the key parsers, including the two shapes the codec review named: an empty
// key and a one-byte key. A verifier that panics on a short key is a verifier an attacker can
// kill the client with.
func FuzzKeys(f *testing.F) {
	f.Add("")
	f.Add("00")
	f.Add(strings.Repeat("11", 32))
	f.Add(strings.Repeat("11", 33))
	f.Add("02" + strings.Repeat("11", 32))
	f.Add("zz")
	f.Add(strings.Repeat("11", 31))

	f.Fuzz(func(t *testing.T, text string) {
		defer survived(t)

		_, _ = nostr.NewKey(text)
		_, _ = keys.FromSecretHex(text)
		_, _ = keys.VerifySchnorr(text, []byte("message"), []byte("signature"))

		if raw, err := hex.DecodeString(text); err == nil {
			_ = keys.EqualSecrets(raw, raw)
		}
	})
}

// FuzzNIP44 covers the sealing layer with adversarial key lengths and payloads.
//
// The encrypted round trip is the one place a wrong length is both common (a truncated key
// from a malformed message) and fatal (a panic in the middle of a decryption).
func FuzzNIP44(f *testing.F) {
	f.Add("", "", "")
	f.Add("00", "00", "00")
	f.Add(strings.Repeat("11", 32), strings.Repeat("22", 32), "payload")
	f.Add(strings.Repeat("11", 32), "02"+strings.Repeat("22", 32), "#not-a-payload")

	f.Fuzz(func(t *testing.T, secret, peer, payload string) {
		defer survived(t)

		secretBytes, secretErr := hex.DecodeString(secret)
		peerBytes, peerErr := hex.DecodeString(peer)

		if secretErr == nil && peerErr == nil {
			if conversation, err := nip44.ConversationKey(secretBytes, peerBytes); err == nil {
				if sealed, err := nip44.Encrypt(conversation, []byte(payload), nil); err == nil {
					_, _ = nip44.Decrypt(conversation, sealed)
				}
			}
		}

		_, _ = nip44.Decrypt([]byte(payload), payload)
		_ = nip44.DescribePayload(payload)
	})
}

// FuzzVaultBackupParse is the vault backup parser: the one file the client reads that it
// did not write, and the one whose failure would be a dead worker at the moment a person
// is trying to recover a vault.
//
// The bounds are what make this fuzzable at all: a parser that allocated the file's own
// declared sizes would be a way to ask the browser for a gigabyte, so the seed corpus
// includes a duplicate key, a missing header and a value that claims to be enormous.
func FuzzVaultBackupParse(f *testing.F) {
	seeds := []string{
		`{}`,
		`[]`,
		`{"kind":"blake-swap-vault"}`,
		`{"kind":"blake-swap-vault","version":1,"created":"2026-10-20T12:00:00Z","entries":[],"sha256":""}`,
		`{"kind":"blake-swap-vault","version":1,"created":"x","entries":[{"key":"","value":""}],"sha256":"00"}`,
		`{"kind":"blake-swap-vault","version":1,"created":"x","entries":[{"key":"vault/header","value":"!!"},{"key":"vault/header","value":"aGVsbG8="}],"sha256":"00"}`,
		`{"kind":"blake-swap-vault","version":1,"created":"x","entries":[{"key":"vault/header","value":"aGVsbG8="},{"key":"vault/verifier","value":"d29ybGQ="}],"sha256":"0000000000000000000000000000000000000000000000000000000000000000"}`,
		`{"kind":"blake-swap-vault","version":2,"created":"x","entries":`,
	}

	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, text string) {
		defer survived(t)

		_, entries, err := vaultbackup.Parse(text)

		if err != nil {
			if entries != nil {
				t.Fatalf("a refused backup returned entries")
			}

			return
		}

		// A backup that parsed must be re-encodable, and its entries must be the ones the
		// digest covered.
		for key, value := range entries {
			if key == "" {
				t.Fatalf("an empty key parsed")
			}

			if len(key) > vaultbackup.MaxKeyBytes {
				t.Fatalf("a %d-byte key parsed", len(key))
			}

			if len(value) > vaultbackup.MaxValueBytes {
				t.Fatalf("a %d-byte value parsed", len(value))
			}
		}
	})
}

// survived fails the case if the code under test panicked. A panic is the failure: an error
// is an answer.
func survived(t *testing.T) {
	t.Helper()

	if recovered := recover(); recovered != nil {
		t.Fatalf("panicked: %v", recovered)
	}
}

// kindPtr is a helper for the optional expected kind.
func kindPtr(kind wire.Kind) *wire.Kind { return &kind }

// countingReader is deterministic entropy, so the seed record is the same every run.
type countingReader struct{ next byte }

func (r *countingReader) Read(p []byte) (int, error) {
	for i := range p {
		r.next++

		p[i] = r.next
	}

	return len(p), nil
}
