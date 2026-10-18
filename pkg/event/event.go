// Package event is the Nostr event layer: canonical ids, kind-1 construction,
// signing and verification.
//
// It is the port of nostr_event_id, build_nostr_event and verify_nostr_event.
// The events themselves are plain kind-1 notes whose content is a JSON message
// from pkg/wire or pkg/market, and whose tags carry the swap id, the protocol
// message type and, for the market, the direction.
//
// Verification here proves authorship and integrity, and deliberately also
// bounds the timestamp: a valid signature proves who signed, not when, and an
// event dated far in the future would win every "latest event" comparison and
// stay fresh forever.
package event

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gjbrr/blake-swap/pkg/keys"
)

// FutureSkewSeconds is how far ahead of local time an event may be dated.
const FutureSkewSeconds = 300

// KindTextNote is the only event kind this protocol uses.
const KindTextNote = 1

// Well-known tag names.
const (
	TagType       = "type"
	TagSwapID     = "d"
	TagTopic      = "t"
	TagDirection  = "direction"
	TagEvent      = "e"
	TagPubkey     = "p"
	TagEncryption = "encryption"
)

// Well-known topic values.
const (
	TopicSwap   = "blake-swap"
	TopicMarket = "blake-swap-market"
	TopicTake   = "blake-swap-market-take"
	TopicMatch  = "blake-swap-market-match"
	TopicCancel = "blake-swap-market-cancel"
)

// Event is a Nostr event.
//
// Tags are a list of string lists, exactly as the wire carries them. The order
// of both the tags and their items is significant: it is part of the id.
type Event struct {
	ID        string     `json:"id"`
	Pubkey    string     `json:"pubkey"`
	CreatedAt int64      `json:"created_at"`
	Kind      uint16     `json:"kind"`
	Tags      [][]string `json:"tags"`
	Content   string     `json:"content"`
	Sig       string     `json:"sig"`
}

// Error is a structural or cryptographic refusal.
type Error struct{ msg string }

func (e *Error) Error() string { return e.msg }

func refuse(format string, args ...any) error {
	return &Error{msg: fmt.Sprintf(format, args...)}
}

// Serialize returns the canonical array the event id is the hash of:
//
//	[0, pubkey, created_at, kind, tags, content]
//
// It is compact, with no ASCII escaping, because that is what the protocol's
// other implementations hash.
func (e Event) Serialize() ([]byte, error) {
	array := []any{
		int64(0),
		e.Pubkey,
		e.CreatedAt,
		int64(e.Kind),
		normalizeTags(e.Tags),
		e.Content,
	}

	var buffer bytes.Buffer

	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(array); err != nil {
		return nil, err
	}

	return bytes.TrimRight(buffer.Bytes(), "\n"), nil
}

// normalizeTags makes a nil tag list encode as [] rather than null.
func normalizeTags(tags [][]string) [][]string {
	if tags == nil {
		return [][]string{}
	}

	return tags
}

// ComputeID returns the hex event id.
func (e Event) ComputeID() (string, error) {
	serialized, err := e.Serialize()

	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(serialized)

	return hex.EncodeToString(sum[:]), nil
}

// Sign sets the id and signature on an event built by Build.
func Sign(e *Event, key *keys.Key) error {
	if key == nil {
		return refuse("no key to sign with")
	}

	if e.Pubkey == "" {
		e.Pubkey = key.PublicHex()
	}

	if e.Pubkey != key.PublicHex() {
		return refuse("event pubkey does not match the signing key")
	}

	if e.Kind == 0 {
		e.Kind = KindTextNote
	}

	id, err := e.ComputeID()

	if err != nil {
		return err
	}

	rawID, err := hex.DecodeString(id)

	if err != nil {
		return err
	}

	signature, err := key.Sign(rawID)

	if err != nil {
		return err
	}

	e.ID = id
	e.Sig = hex.EncodeToString(signature)

	return nil
}

// Verify checks the id, the signature, and the timestamp bound.
//
// `now` is passed in rather than read from the clock so the check is testable.
func Verify(e Event, now time.Time) error {
	if len(e.ID) != 64 || len(e.Pubkey) != 64 || len(e.Sig) != 128 {
		return refuse("nostr event has malformed id, pubkey or signature")
	}

	if e.Kind != KindTextNote {
		return refuse("nostr event kind is not %d", KindTextNote)
	}

	computed, err := e.ComputeID()

	if err != nil {
		return err
	}

	if computed != e.ID {
		return refuse("nostr event ID verification failed")
	}

	signature, err := hex.DecodeString(e.Sig)

	if err != nil {
		return refuse("nostr signature is not hex")
	}

	rawID, err := hex.DecodeString(e.ID)

	if err != nil {
		return refuse("nostr event id is not hex")
	}

	valid, err := keys.VerifySchnorr(e.Pubkey, rawID, signature)

	if err != nil {
		return refuse("nostr signature verification failed: %v", err)
	}

	if !valid {
		return refuse("nostr Schnorr signature verification failed")
	}

	if e.CreatedAt > now.Unix()+FutureSkewSeconds {
		return refuse(
			"nostr event created_at is too far in the future (skew limit %ds)",
			FutureSkewSeconds,
		)
	}

	return nil
}

// TagValue returns the first value of a tag by name.
func (e Event) TagValue(name string) (string, bool) {
	for _, tag := range e.Tags {
		if len(tag) >= 2 && tag[0] == name {
			return tag[1], true
		}
	}

	return "", false
}

// HasTag reports whether a tag name/value pair is present.
func (e Event) HasTag(name, value string) bool {
	for _, tag := range e.Tags {
		if len(tag) >= 2 && tag[0] == name && tag[1] == value {
			return true
		}
	}

	return false
}

// DedupeTags removes empty tag items and refuses tags that have none, because a
// tag with no name is not addressable by any reader.
func DedupeTags(tags [][]string) ([][]string, error) {
	out := make([][]string, 0, len(tags))

	for _, tag := range tags {
		cleaned := make([]string, 0, len(tag))

		for _, item := range tag {
			if strings.TrimSpace(item) == "" {
				continue
			}

			cleaned = append(cleaned, item)
		}

		if len(cleaned) == 0 {
			return nil, refuse("nostr tag has no name")
		}

		out = append(out, cleaned)
	}

	return out, nil
}

// BuildOption configures Build.
type BuildOption func(*buildOptions)

type buildOptions struct {
	content []byte
	tags    [][]string
	now     int64
}

// WithContent sets the content bytes (a serialized protocol message).
func WithContent(content []byte) BuildOption {
	return func(o *buildOptions) { o.content = content }
}

// WithTags sets the tags. Build adds none of its own.
func WithTags(tags [][]string) BuildOption {
	return func(o *buildOptions) { o.tags = tags }
}

// WithCreatedAt overrides the timestamp, for tests and replay fixtures.
func WithCreatedAt(seconds int64) BuildOption {
	return func(o *buildOptions) { o.now = seconds }
}

// Build constructs an unsigned event.
//
// The caller supplies the content and the tags; Sign sets the id and signature.
// Keeping construction and signing separate means a caller can inspect or
// reject an event before committing to a signature over it.
func Build(pubkey string, options ...BuildOption) (Event, error) {
	opts := buildOptions{now: time.Now().Unix()}

	for _, option := range options {
		option(&opts)
	}

	if len(opts.content) == 0 {
		return Event{}, refuse("event content is empty")
	}

	tags := opts.tags

	// Sort a copy? No: tag order is part of the id, so it is preserved exactly
	// as the caller gave it.
	event := Event{
		Pubkey:    pubkey,
		CreatedAt: opts.now,
		Kind:      KindTextNote,
		Tags:      normalizeTags(tags),
		Content:   string(opts.content),
	}

	return event, nil
}

// CoordinationTags are the tags every swap coordination message carries.
func CoordinationTags(swapID, messageType string) [][]string {
	return [][]string{
		{TagTopic, TopicSwap},
		{TagSwapID, swapID},
		{TagType, messageType},
	}
}

// MarketTags are the tags a market offer or take carries.
func MarketTags(topic, direction string) [][]string {
	tags := [][]string{{TagTopic, topic}}

	if direction != "" {
		tags = append(tags, []string{TagDirection, direction})
	}

	return tags
}

// ReferencesTags are the tags a take or match adds to point at what it answers.
func ReferencesTags(eventIDs, pubkeys []string) [][]string {
	var tags [][]string

	for _, id := range eventIDs {
		tags = append(tags, []string{TagEvent, id})
	}

	for _, pubkey := range pubkeys {
		tags = append(tags, []string{TagPubkey, pubkey})
	}

	return tags
}

// CanonicalJSON renders a decoded message the way event content is built:
// compact, sorted keys, no escaping.
func CanonicalJSON(message any) ([]byte, error) {
	switch value := message.(type) {
	case []byte:
		return value, nil
	case string:
		return []byte(value), nil
	case map[string]any:
		return canonicalMap(value)
	default:
		encoded, err := json.Marshal(value)

		if err != nil {
			return nil, err
		}

		var asMap map[string]any

		if err := json.Unmarshal(encoded, &asMap); err != nil {
			return nil, refuse("message is not a JSON object")
		}

		return canonicalMap(asMap)
	}
}

func canonicalMap(message map[string]any) ([]byte, error) {
	// encoding/json sorts map keys, so this is already canonical; the only
	// thing to change is HTML escaping, which Python's json.dumps does not do.
	var buffer bytes.Buffer

	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(message); err != nil {
		return nil, err
	}

	return bytes.TrimRight(buffer.Bytes(), "\n"), nil
}
