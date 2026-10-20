package vaultbackup

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The envelope is the one file the client parses that it did not write, so its format and
// its bounds are tested on their own: what a backup promises is independent of the two
// commands that move one in and out of the vault.

// TestVaultEnvelopeDigest: the checksum is over a canonical string, so order does not
// matter and one changed byte does.
func TestVaultEnvelopeDigest(t *testing.T) {
	created := "2026-10-20T12:00:00Z"

	first := []Entry{
		{Key: "a", Value: base64.StdEncoding.EncodeToString([]byte("one"))},
		{Key: "b", Value: base64.StdEncoding.EncodeToString([]byte("two"))},
	}

	reordered := []Entry{first[1], first[0]}
	SortEntries(reordered)

	if Digest(Kind, 1, created, first) !=
		Digest(Kind, 1, created, reordered) {
		t.Fatal("the digest depends on the order the entries are collected in")
	}

	changed := []Entry{
		{Key: "a", Value: base64.StdEncoding.EncodeToString([]byte("one"))},
		{Key: "b", Value: base64.StdEncoding.EncodeToString([]byte("tw0"))},
	}

	if Digest(Kind, 1, created, first) ==
		Digest(Kind, 1, created, changed) {
		t.Fatal("one changed byte did not change the digest")
	}

	if Digest(Kind, 2, created, first) ==
		Digest(Kind, 1, created, first) {
		t.Fatal("the version is not covered by the digest")
	}

	// And it is the documented string, not merely some string.
	var builder strings.Builder

	builder.WriteString(Kind + "\n1\n" + created + "\n")
	builder.WriteString("a\n" + first[0].Value + "\n")
	builder.WriteString("b\n" + first[1].Value + "\n")

	sum := sha256.Sum256([]byte(builder.String()))

	if want := hex.EncodeToString(sum[:]); Digest(Kind, 1, created, first) != want {
		t.Fatalf("the digest does not match the documented input")
	}
}

// TestVaultEnvelopeRefusesBadDocuments: every bound and every structural rule, with the
// size that was measured.
func TestVaultEnvelopeRefusesBadDocuments(t *testing.T) {
	valid := func() map[string]any {
		return map[string]any{
			"kind":    Kind,
			"version": Version,
			"created": "2026-10-20T12:00:00Z",
			"entries": []any{
				map[string]any{"key": HeaderKey, "value": "aGVsbG8="},
				map[string]any{"key": VerifierKey, "value": "d29ybGQ="},
			},
			"sha256": Digest(Kind, Version,
				"2026-10-20T12:00:00Z", []Entry{
					{Key: HeaderKey, Value: "aGVsbG8="},
					{Key: VerifierKey, Value: "d29ybGQ="},
				}),
		}
	}

	marshal := func(document map[string]any) string {
		encoded, err := json.Marshal(document)

		if err != nil {
			t.Fatal(err)
		}

		return string(encoded)
	}

	if _, _, err := Parse(marshal(valid())); err != nil {
		t.Fatalf("a valid document was refused: %v", err)
	}

	cases := map[string]struct {
		document map[string]any
		want     string
	}{
		"wrong kind": {func() map[string]any {
			document := valid()
			document["kind"] = "something-else"

			return document
		}(), "not a blake-swap vault backup"},

		"wrong version": {func() map[string]any {
			document := valid()
			document["version"] = 99

			return document
		}(), "version 99"},

		"no entries": {func() map[string]any {
			document := valid()
			document["entries"] = []any{}

			return document
		}(), "holds no entries"},

		"bad base64": {func() map[string]any {
			document := valid()
			document["entries"] = []any{
				map[string]any{"key": HeaderKey, "value": "not base64!"},
				map[string]any{"key": VerifierKey, "value": "d29ybGQ="},
			}

			return document
		}(), "not valid base64"},

		"duplicate key": {func() map[string]any {
			document := valid()
			document["entries"] = []any{
				map[string]any{"key": HeaderKey, "value": "aGVsbG8="},
				map[string]any{"key": HeaderKey, "value": "d29ybGQ="},
			}

			return document
		}(), "twice"},

		"empty key": {func() map[string]any {
			document := valid()
			document["entries"] = append(document["entries"].([]any),
				map[string]any{"key": "", "value": "aGVsbG8="})

			return document
		}(), "empty key"},

		"control character": {func() map[string]any {
			document := valid()
			document["entries"] = append(document["entries"].([]any),
				map[string]any{"key": "bad\x01key", "value": "aGVsbG8="})

			return document
		}(), "control character"},

		"long key": {func() map[string]any {
			document := valid()
			document["entries"] = append(document["entries"].([]any),
				map[string]any{"key": strings.Repeat("k", MaxKeyBytes+1), "value": "aGVsbG8="})

			return document
		}(), "more than the 256 a backup holds"},

		"edited": {func() map[string]any {
			document := valid()
			document["sha256"] = strings.Repeat("0", 64)

			return document
		}(), "does not match its own checksum"},

		"no header": {func() map[string]any {
			document := valid()
			document["entries"] = []any{
				map[string]any{"key": VerifierKey, "value": "d29ybGQ="},
			}
			document["sha256"] = Digest(Kind, 1, "2026-10-20T12:00:00Z",
				[]Entry{{Key: VerifierKey, Value: "d29ybGQ="}})

			return document
		}(), "missing \"vault/header\""},

		"no verifier": {func() map[string]any {
			document := valid()
			document["entries"] = []any{
				map[string]any{"key": HeaderKey, "value": "aGVsbG8="},
			}
			document["sha256"] = Digest(Kind, 1, "2026-10-20T12:00:00Z",
				[]Entry{{Key: HeaderKey, Value: "aGVsbG8="}})

			return document
		}(), "missing \"vault/verifier\""},
	}

	for name, test := range cases {
		if _, _, err := Parse(marshal(test.document)); err == nil {
			t.Errorf("%s was accepted", name)
		} else if !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: refusal = %v, want it to mention %q", name, err, test.want)
		}
	}

	// Too many entries, and an oversized file, are refused with the measured size.
	many := map[string]any{
		"kind":    Kind,
		"version": Version,
		"created": "2026-10-20T12:00:00Z",
		"entries": make([]any, MaxEntries+1),
		"sha256":  strings.Repeat("0", 64),
	}

	entries := many["entries"].([]any)

	for i := range entries {
		entries[i] = map[string]any{"key": "k" + strings.Repeat("0", 6), "value": ""}
	}

	if _, _, err := Parse(marshal(many)); err == nil ||
		!strings.Contains(err.Error(), "more than the 10000 a backup holds") {
		t.Fatalf("too many entries: %v", err)
	}

	oversized := strings.Repeat("x", MaxTextBytes+1)

	if _, _, err := Parse(oversized); err == nil ||
		!strings.Contains(err.Error(), "more than the 25165824 a backup holds") {
		t.Fatalf("oversized file: %v", err)
	}
}

// tamperFirstValue changes one stored value and leaves the document's checksum alone, which
// is what an edited file looks like.
func tamperFirstValue(t *testing.T, text string) string {
	t.Helper()

	var document map[string]any

	if err := json.Unmarshal([]byte(text), &document); err != nil {
		t.Fatal(err)
	}

	entries, ok := document["entries"].([]any)

	if !ok || len(entries) == 0 {
		t.Fatal("the document has no entries")
	}

	first, ok := entries[0].(map[string]any)

	if !ok {
		t.Fatal("the first entry is not an object")
	}

	value, _ := first["value"].(string)

	first["value"] = base64.StdEncoding.EncodeToString(
		append([]byte{0x00}, value...),
	)

	encoded, err := json.Marshal(document)

	if err != nil {
		t.Fatal(err)
	}

	return string(encoded)
}

// TestVaultExportNameIsUTC: two backups of one vault must be tellable apart, and the name
// must not depend on the machine's timezone.
func TestVaultExportNameIsUTC(t *testing.T) {
	when := time.Date(2026, 10, 20, 12, 34, 56, 0, time.FixedZone("ahead", 4*3600))

	if got := FileName(when); got != "blake-swap-vault-20261020-083456Z.json" {
		t.Fatalf("name = %q", got)
	}
}

// TestVaultExportNameIsUTC: two backups of one vault must be tellable apart, and the name
// must not depend on the machine's timezone.
