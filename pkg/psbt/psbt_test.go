package psbt

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

// emptyPSBT is a valid PSBT with one empty global map, one empty input map and
// one empty output map.
func emptyPSBT() []byte {
	raw := append([]byte{}, Magic...)
	raw = append(raw, 0x00) // end of global map
	raw = append(raw, 0x00) // end of input 0 map
	raw = append(raw, 0x00) // end of output 0 map

	return raw
}

func b64(raw []byte) string { return base64.StdEncoding.EncodeToString(raw) }

func preimage(t *testing.T, fill byte) (secretHex, hashHex string) {
	t.Helper()

	secret := bytes.Repeat([]byte{fill}, 32)
	sum := sha256.Sum256(secret)

	return hex.EncodeToString(secret), hex.EncodeToString(sum[:])
}

func TestAddPreimageInsertsIntoInputZero(t *testing.T) {
	secretHex, hashHex := preimage(t, 0x11)

	base := emptyPSBT()

	got, err := AddSHA256Preimage(b64(base), hashHex, secretHex, 0)

	if err != nil {
		t.Fatal(err)
	}

	raw, err := base64.StdEncoding.DecodeString(got)

	if err != nil {
		t.Fatal(err)
	}

	// The magic, global map and its terminator are byte-identical.
	prefix := append([]byte{}, Magic...)
	prefix = append(prefix, 0x00)

	if !bytes.HasPrefix(raw, prefix) {
		t.Fatalf("header changed: %x", raw)
	}

	// The record is the only thing between the input terminator position and
	// the remaining maps.
	recordLen := 1 + 33 + 1 + 32

	if len(raw) != len(base)+recordLen {
		t.Fatalf("length %d, want %d", len(raw), len(base)+recordLen)
	}

	// Parse it back: exactly one record, and it matches.
	records, err := SHA256Records(got)

	if err != nil {
		t.Fatal(err)
	}

	if len(records) != 1 {
		t.Fatalf("got %d records", len(records))
	}

	if hex.EncodeToString(records[0].Hash) != hashHex {
		t.Error("hash does not match")
	}

	if hex.EncodeToString(records[0].Preimage) != secretHex {
		t.Error("preimage does not match")
	}

	// The output map is still intact and still empty: the input map's
	// terminator, then the output map's terminator.
	tail := raw[len(raw)-2:]

	if tail[0] != 0x00 || tail[1] != 0x00 {
		t.Fatalf("map terminators were disturbed: %x", raw)
	}
}

func TestAddPreimageWithExistingInputRecords(t *testing.T) {
	// A PSBT whose input map already holds a witness UTXO record must keep it.
	secretHex, hashHex := preimage(t, 0x22)

	raw := append([]byte{}, Magic...)
	raw = append(raw, 0x00) // global map end

	// input map: key 0x01 (witness UTXO) length 4, value "abcd"
	raw = append(raw, 0x01, 0x01, 0x04, 'a', 'b', 'c', 'd')
	raw = append(raw, 0x00) // input map end
	raw = append(raw, 0x00) // output map end

	got, err := AddSHA256Preimage(b64(raw), hashHex, secretHex, 0)

	if err != nil {
		t.Fatal(err)
	}

	decoded, _ := base64.StdEncoding.DecodeString(got)

	if !bytes.Contains(decoded, []byte("abcd")) {
		t.Fatal("an existing input record was lost")
	}

	records, err := SHA256Records(got)

	if err != nil {
		t.Fatal(err)
	}

	if len(records) != 1 {
		t.Fatalf("got %d records", len(records))
	}
}

func TestAddPreimageRefusals(t *testing.T) {
	secretHex, hashHex := preimage(t, 0x33)
	_, otherHash := preimage(t, 0x44)

	cases := []struct {
		name   string
		psbt   string
		hash   string
		secret string
		index  int
		want   string
	}{
		{"not base64", "!!!!", hashHex, secretHex, 0, "base64"},
		{"not a psbt", b64([]byte("hello")), hashHex, secretHex, 0, "not a PSBT"},
		{"short hash", b64(emptyPSBT()), "abcd", secretHex, 0, "hash must be 32 bytes"},
		{"short secret", b64(emptyPSBT()), hashHex, "abcd", 0, "secret must be 32 bytes"},
		{"non-hex hash", b64(emptyPSBT()), strings.Repeat("zz", 32), secretHex, 0, "hash must be hex"},
		{"secret does not match", b64(emptyPSBT()), otherHash, secretHex, 0, "does not hash"},
		{"wrong input index", b64(emptyPSBT()), hashHex, secretHex, 1, "only supports input 0"},
		{"unterminated global map", b64(append(append([]byte{}, Magic...), 0x01)), hashHex, secretHex, 0, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := AddSHA256Preimage(c.psbt, c.hash, c.secret, c.index)

			if err == nil {
				t.Fatal("accepted")
			}

			if c.want != "" && !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

func TestCompactSize(t *testing.T) {
	// Minimal encodings round-trip.
	cases := []struct {
		raw   []byte
		value uint64
		next  int
	}{
		{[]byte{0x00}, 0, 1},
		{[]byte{0xfc}, 252, 1},
		{[]byte{0xfd, 0xfd, 0x00}, 253, 3},
		{[]byte{0xfd, 0xff, 0xff}, 65535, 3},
		{[]byte{0xfe, 0x00, 0x00, 0x01, 0x00}, 65536, 5},
		{[]byte{0xff, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00}, 1 << 32, 9},
	}

	for _, c := range cases {
		value, next, err := ReadCompactSize(c.raw, 0)

		if err != nil {
			t.Fatalf("%x: %v", c.raw, err)
		}

		if value != c.value || next != c.next {
			t.Fatalf("%x -> %d,%d want %d,%d", c.raw, value, next, c.value, c.next)
		}
	}

	// Truncated, non-minimal and out-of-range reads are refused.
	for _, raw := range [][]byte{
		{},
		{0xfd},
		{0xfd, 0x01},
		{0xfe, 0x00},
		{0xff, 0x01, 0x02, 0x03},
		{0xfd, 0x01, 0x00}, // 1 encoded in two bytes: non-minimal
		{0xfe, 0xfd, 0x00, 0x00, 0x00},
		{0xff, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
	} {
		if _, _, err := ReadCompactSize(raw, 0); err == nil {
			t.Errorf("%x was accepted", raw)
		}
	}

	if _, _, err := ReadCompactSize([]byte{0x00}, -1); err == nil {
		t.Error("negative offset was accepted")
	}
}

func TestFindMapEnd(t *testing.T) {
	// An empty map terminates immediately.
	end, err := FindMapEnd([]byte{0x00}, 0)

	if err != nil || end != 0 {
		t.Fatalf("empty map: %d %v", end, err)
	}

	// One key/value pair then the terminator.
	raw := []byte{0x01, 'k', 0x02, 'v', 'v', 0x00}

	end, err = FindMapEnd(raw, 0)

	if err != nil || end != 5 {
		t.Fatalf("one pair: %d %v", end, err)
	}

	// Unterminated, and a key that runs past the end.
	for _, bad := range [][]byte{
		{0x01, 'k'},
		{0x01, 'k', 0x02, 'v'},
		{0x02, 'k'},
		{},
	} {
		if _, err := FindMapEnd(bad, 0); err == nil {
			t.Errorf("%x was accepted", bad)
		}
	}
}

func TestSHA256RecordsOnPlainPSBT(t *testing.T) {
	records, err := SHA256Records(b64(emptyPSBT()))

	if err != nil {
		t.Fatal(err)
	}

	if len(records) != 0 {
		t.Fatalf("empty PSBT has %d records", len(records))
	}

	if _, err := SHA256Records("not base64"); err == nil {
		t.Error("invalid base64 was accepted by the reader")
	}
}
