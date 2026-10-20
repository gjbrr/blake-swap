// Package vaultbackup is the sealed vault's backup format.
//
// It is a separate package because it is the one place in the client that parses a file it
// did not produce: a backup may have been edited, truncated by a full disk, or written by
// a build that meant something else. Keeping the format next to its parser makes the
// bounds and the checksum testable on their own, and keeps the worker domain to the two
// commands that use it.
package vaultbackup

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

//
// The vault backup is one JSON document that holds every key the sealed store has.
//
// Every key, not a list of prefixes: a backup that dropped `vault/header` could not be
// opened, one that dropped `record-role/` would let a later save change a participant's
// side, and one that dropped `market/` would forget which negotiation had already been
// accepted. The values are the stored bytes, which for everything except the KDF header
// are ciphertext: the file is exactly as private as the passphrase, no more and no less.
//

const (
	// Kind identifies the file, so a JSON document that happens to parse is
	// not mistaken for a backup.
	Kind = "blake-swap-vault"
	// Version is the format. A restore of an unknown version is refused
	// rather than guessed at.
	Version = 1
)

// The bounds, all checked before anything is allocated from the file.
//
// They are generous for a real vault and small enough that a hostile file cannot make the
// worker allocate without limit: a browser tab is one place where an unbounded parse is a
// dead client rather than a slow one.
const (
	MaxBytes      = 16 << 20
	MaxEntries    = 10_000
	MaxKeyBytes   = 256
	MaxValueBytes = 8 << 20
	// MaxTextBytes is what the base64 encoding of 16 MiB can occupy, with a
	// little room for the rest of the document.
	MaxTextBytes = 24 << 20
)

// Envelope is the backup document.
type Envelope struct {
	Kind    string  `json:"kind"`
	Version int     `json:"version"`
	Created string  `json:"created"`
	Entries []Entry `json:"entries"`
	SHA256  string  `json:"sha256"`
}

// Entry is one stored key and its stored bytes, base64 standard encoded.
type Entry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Digest covers the document's own facts and every entry, in sorted order.
//
// The input is spelled out rather than derived from the JSON so two encoders that indent
// differently still produce the same digest: kind, the version in decimal, the creation
// time, then one key and one value per line, each terminated by a newline. The value is
// the base64 text exactly as the file carries it, so a restore can check the file it read
// rather than a re-encoding of it.
func Digest(kind string, version int, created string, entries []Entry) string {
	var builder strings.Builder

	builder.WriteString(kind)
	builder.WriteString("\n")
	builder.WriteString(strconv.Itoa(version))
	builder.WriteString("\n")
	builder.WriteString(created)
	builder.WriteString("\n")

	for _, entry := range entries {
		builder.WriteString(entry.Key)
		builder.WriteString("\n")
		builder.WriteString(entry.Value)
		builder.WriteString("\n")
	}

	sum := sha256.Sum256([]byte(builder.String()))

	return hex.EncodeToString(sum[:])
}

// FileName is the file name a backup is offered under.
//
// It carries the moment of export in UTC, because two backups of the same vault are two
// different files and a person who keeps them in one directory needs to tell them apart
// without opening either.
func FileName(now time.Time) string {
	return "blake-swap-vault-" + now.UTC().Format("20060102-150405Z") + ".json"
}

// Encode builds the document from a key-value snapshot.
func Encode(
	now time.Time,
	entries map[string][]byte,
) ([]byte, Envelope, error) {
	if len(entries) > MaxEntries {
		return nil, Envelope{}, fmt.Errorf(
			"the vault has %d entries, more than the %d a backup holds",
			len(entries), MaxEntries,
		)
	}

	sorted := make([]Entry, 0, len(entries))

	total := 0

	for key, value := range entries {
		if err := validateKey(key); err != nil {
			return nil, Envelope{}, err
		}

		total += len(value)

		if total > MaxBytes {
			return nil, Envelope{}, fmt.Errorf(
				"the vault holds more than the %d bytes a backup holds", MaxBytes,
			)
		}

		if len(value) > MaxValueBytes {
			return nil, Envelope{}, fmt.Errorf(
				"the value under %q is %d bytes, more than the %d a backup holds",
				key, len(value), MaxValueBytes,
			)
		}

		sorted = append(sorted, Entry{
			Key:   key,
			Value: base64.StdEncoding.EncodeToString(value),
		})
	}

	SortEntries(sorted)

	document := Envelope{
		Kind:    Kind,
		Version: Version,
		Created: now.UTC().Format(time.RFC3339),
		Entries: sorted,
	}

	document.SHA256 = Digest(document.Kind, document.Version, document.Created, sorted)

	encoded, err := json.MarshalIndent(document, "", "  ")

	if err != nil {
		return nil, Envelope{}, err
	}

	return append(encoded, '\n'), document, nil
}

// SortEntries orders entries by key, ascending.
//
// A backup may hold ten thousand entries, so this is a real sort rather than the insertion
// sort a short list would justify: the digest and the file are both defined in this order,
// and a quadratic sort would make exporting a large vault the slowest thing the client
// does.
func SortEntries(entries []Entry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
}

// validateKey refuses a key a vault could not have written.
func validateKey(key string) error {
	switch {
	case key == "":
		return fmt.Errorf("a backup entry has an empty key")
	case len(key) > MaxKeyBytes:
		return fmt.Errorf(
			"a backup entry key is %d bytes, more than the %d a backup holds",
			len(key), MaxKeyBytes,
		)
	}

	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("a backup entry key contains a control character")
		}
	}

	return nil
}

// Parse validates a backup file and returns its entries.
//
// Every check happens before the store is touched, and the entries come back as a new map
// rather than by mutating anything the caller holds. The file is unverified input: it may
// have been edited, truncated by a full disk, or produced by a build that meant something
// else.
func Parse(text string) (Envelope, map[string][]byte, error) {
	if len(text) > MaxTextBytes {
		return Envelope{}, nil, fmt.Errorf(
			"the backup file is %d bytes, more than the %d a backup holds",
			len(text), MaxTextBytes,
		)
	}

	var document Envelope

	if err := json.Unmarshal([]byte(text), &document); err != nil {
		return Envelope{}, nil, fmt.Errorf("the backup is not a readable vault file")
	}

	if document.Kind != Kind {
		return Envelope{}, nil, fmt.Errorf("the file is not a blake-swap vault backup")
	}

	if document.Version != Version {
		return Envelope{}, nil, fmt.Errorf(
			"the backup is version %d; this client restores version %d",
			document.Version, Version,
		)
	}

	if len(document.Entries) == 0 {
		return Envelope{}, nil, fmt.Errorf("the backup holds no entries")
	}

	if len(document.Entries) > MaxEntries {
		return Envelope{}, nil, fmt.Errorf(
			"the backup holds %d entries, more than the %d a backup holds",
			len(document.Entries), MaxEntries,
		)
	}

	entries := make(map[string][]byte, len(document.Entries))

	total := 0

	for _, entry := range document.Entries {
		if err := validateKey(entry.Key); err != nil {
			return Envelope{}, nil, err
		}

		if _, seen := entries[entry.Key]; seen {
			return Envelope{}, nil, fmt.Errorf(
				"the backup names %q twice", entry.Key,
			)
		}

		if len(entry.Value) > base64.StdEncoding.EncodedLen(MaxValueBytes) {
			return Envelope{}, nil, fmt.Errorf(
				"the value under %q is larger than the %d bytes a backup holds",
				entry.Key, MaxValueBytes,
			)
		}

		decoded, err := base64.StdEncoding.DecodeString(entry.Value)

		if err != nil {
			return Envelope{}, nil, fmt.Errorf(
				"the value under %q is not valid base64", entry.Key,
			)
		}

		total += len(decoded)

		if total > MaxBytes {
			return Envelope{}, nil, fmt.Errorf(
				"the backup decodes to more than the %d bytes a backup holds",
				MaxBytes,
			)
		}

		if len(decoded) > MaxValueBytes {
			return Envelope{}, nil, fmt.Errorf(
				"the value under %q decodes to %d bytes, more than the %d a backup holds",
				entry.Key, len(decoded), MaxValueBytes,
			)
		}

		entries[entry.Key] = decoded
	}

	// Sorting before the digest is what makes the check independent of the order the file
	// happens to list its entries in: a file whose entries were reshuffled still verifies,
	// while one whose bytes changed does not.
	SortEntries(document.Entries)

	want := Digest(document.Kind, document.Version, document.Created, document.Entries)

	if !strings.EqualFold(strings.TrimSpace(document.SHA256), want) {
		return Envelope{}, nil, fmt.Errorf(
			"the backup does not match its own checksum; it was edited or damaged",
		)
	}

	// A vault is only a vault with both of these: the header is what derives the key and
	// the verifier is what proves the passphrase. A file missing either would be restored
	// into a store that cannot be opened.
	for _, required := range []string{HeaderKey, VerifierKey} {
		if _, ok := entries[required]; !ok {
			return Envelope{}, nil, fmt.Errorf(
				"the backup is missing %q, so it is not a vault", required,
			)
		}
	}

	return document, entries, nil
}

// The two keys a vault cannot exist without. They are the client store's own names.
const (
	HeaderKey   = "vault/header"
	VerifierKey = "vault/verifier"
)
