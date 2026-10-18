// Package nostr is the relay side of coordination.
//
// The engine's protocol does not depend on any relay: every fact a coordination
// message carries is re-verified against a chain, and the secret never travels.
// What a relay provides is availability, so this layer's job is to be strict
// about the two things a relay cannot be trusted for — who signed an event, and
// whether the content is the thing it claims to be.
//
// The WebSocket itself is supplied by the platform. On a host that is a socket
// library; in the browser it is a JavaScript bridge, because a wasm build has no
// sockets of its own. Everything above that seam is ordinary Go and is tested
// here against scripted connections.
package nostr

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/keys"
	"github.com/gjbrr/blake-swap/pkg/nip44"
)

// Conn is one WebSocket connection to a relay.
type Conn interface {
	// Send writes one text frame.
	Send(ctx context.Context, payload string) error
	// Receive reads one text frame.
	Receive(ctx context.Context) (string, error)
	Close() error
}

// Dialer opens a relay connection. It is the platform seam.
type Dialer func(ctx context.Context, url string) (Conn, error)

// Mode is the coordination confidentiality mode.
type Mode string

// The two modes. Coordination content is plaintext by default: the protocol does
// not depend on transport confidentiality, so encryption is an opt-in that
// shields the metadata as well.
const (
	// ModeOff keeps coordination content in plaintext.
	ModeOff Mode = "off"
	// ModeNIP44 encrypts every pairwise settlement message.
	ModeNIP44 Mode = "nip44_v2"
)

// EncryptionTag is the signed tag value that declares NIP-44 v2 content.
const EncryptionTag = "nip44_v2"

// ParseMode reads the configured mode.
//
// An unrecognised value is refused rather than treated as off: a participant who
// typed the mode wrong should not silently publish plaintext.
func ParseMode(raw string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "0", "false", "no", "off":
		return ModeOff, nil
	case EncryptionTag:
		return ModeNIP44, nil
	}

	return "", fmt.Errorf(
		"BLAKE_SWAP_NOSTR_ENCRYPTION must be one of off, %s", EncryptionTag,
	)
}

// EncryptableTypes are the message types that must be encrypted when the mode is
// on.
//
// The offer is exempt in both directions: it is first contact, published before
// the maker knows the taker's identity, and NIP-44 is pairwise by construction,
// so there is no key to encrypt it to. It must also stay readable on receipt, or
// two participants could never meet.
var EncryptableTypes = map[string]bool{
	"accept":        true,
	"blake-funded":  true,
	"btc-funded":    true,
	"btc-claimed":   true,
	"blake-claimed": true,
	"btc-refunded":  true,
}

// RequiresEncryption reports whether a message type must be encrypted.
func RequiresEncryption(mode Mode, messageType string) bool {
	return mode == ModeNIP44 && EncryptableTypes[messageType]
}

// MaxEventBytes is the largest serialized event this engine will publish.
//
// Relays enforce their own limits and the useful range across public relays is
// wide. This is deliberately not an attempt to match any particular relay: it is
// a bound the engine applies before handing an event to a network it does not
// control, so a runaway content field is refused here with a clear message
// instead of being rejected by whichever relay happens to be strictest, after
// seven others accepted it.
const MaxEventBytes = 64 * 1024

// Key is the local Nostr identity.
type Key struct {
	// Secret is the private key, 32 bytes of hex.
	Secret string
	// Public is the x-only public key, 32 bytes of hex.
	Public string
}

// NewKey derives the identity from a private key.
func NewKey(secretHex string) (*Key, error) {
	secret, err := keys.FromSecretHex(secretHex)

	if err != nil {
		return nil, err
	}

	return &Key{Secret: secretHex, Public: secret.PublicHex()}, nil
}

// BuildOptions says how one event is built.
type BuildOptions struct {
	// Mode is the confidentiality mode.
	Mode Mode
	// PeerPubkey is the counterparty's x-only key. It is required whenever the
	// message must be encrypted: encryption to nobody is a plaintext message
	// wearing a tag.
	PeerPubkey string
	// CreatedAt is the event time. Zero means now.
	CreatedAt int64
}

// BuildCoordinationEvent builds and signs one coordination event.
//
// The content is either the canonical JSON of the message or its NIP-44
// payload, and when it is encrypted the signed tags say so. The tag has to be
// inside the signature: a relay that could add or remove it could turn an
// encrypted message into an unreadable one, or claim one is encrypted when it is
// not.
func BuildCoordinationEvent(
	key *Key,
	swapID, messageType string,
	message map[string]any,
	opts BuildOptions,
) (event.Event, error) {
	if key == nil {
		return event.Event{}, fmt.Errorf("a coordination event needs an identity")
	}

	if swapID == "" || messageType == "" {
		return event.Event{}, fmt.Errorf("a coordination event needs a swap id and a type")
	}

	canonical, err := event.CanonicalJSON(message)

	if err != nil {
		return event.Event{}, err
	}

	tags := event.CoordinationTags(swapID, messageType)
	content := canonical

	if RequiresEncryption(opts.Mode, messageType) {
		if opts.PeerPubkey == "" {
			return event.Event{}, fmt.Errorf(
				"SECURITY FAILURE: no pinned counterparty Nostr identity to encrypt to",
			)
		}

		secret, err := keys.FromSecretHex(key.Secret)

		if err != nil {
			return event.Event{}, err
		}

		peer, err := hex.DecodeString(opts.PeerPubkey)

		if err != nil {
			return event.Event{}, fmt.Errorf(
				"counterparty Nostr identity must be 32 bytes of hex",
			)
		}

		conversationKey, err := nip44.ConversationKey(secret.Secret(), peer)

		if err != nil {
			return event.Event{}, err
		}

		encrypted, err := nip44.Encrypt(conversationKey, canonical, nil)

		if err != nil {
			return event.Event{}, err
		}

		content = []byte(encrypted)
		tags = append(tags, []string{"encryption", EncryptionTag})
	}

	built, err := event.Build(key.Public, event.WithContent(content), event.WithTags(tags))

	if err != nil {
		return event.Event{}, err
	}

	if opts.CreatedAt != 0 {
		built.CreatedAt = opts.CreatedAt

		// Recompute the id, because the time is part of it.
		id, err := built.ComputeID()

		if err != nil {
			return event.Event{}, err
		}

		built.ID = id
	}

	signer, err := keys.FromSecretHex(key.Secret)

	if err != nil {
		return event.Event{}, err
	}

	if err := event.Sign(&built, signer); err != nil {
		return event.Event{}, err
	}

	if err := checkPublishableSize(built); err != nil {
		return event.Event{}, err
	}

	return built, nil
}

// checkPublishableSize refuses an event a relay would not take.
func checkPublishableSize(e event.Event) error {
	serialized, err := json.Marshal(e)

	if err != nil {
		return err
	}

	if len(serialized) > MaxEventBytes {
		return fmt.Errorf(
			"event is %d bytes, above the %d-byte limit (BLAKE_SWAP_MAX_EVENT_BYTES)",
			len(serialized), MaxEventBytes,
		)
	}

	return nil
}

// EventEncryption reads the encryption an event declares.
func EventEncryption(e event.Event) (string, bool) {
	return e.TagValue("encryption")
}

// EventMessageType reads the coordination type an event declares.
//
// It is read from the signed tag rather than from the content, because the
// content may be unreadable until the right key is known — and a receiver must
// decide what the event claims to be before it can decide whether it can read it.
func EventMessageType(e event.Event) (string, bool) {
	return e.TagValue("type")
}

// DecodeContent is the coordination message inside a verified event.
//
// The event is expected to be the counterparty's: the conversation key is
// derived from the local secret and the event's author, which is the pair that
// encrypted it. Decoding one's own event derives (own, own) and fails to
// authenticate, which is why the engine never feeds itself here.
//
// It returns nil, nil when the event is simply not the one being looked for (a
// different swap or type, or content that does not parse). It returns an error
// when the *declared* encryption cannot be honoured, because that is a security
// condition and not a relay artefact:
//
//   - the event declares nip44_v2 and we cannot read it;
//   - the content looks encrypted but declares nothing, while we require
//     encryption;
//   - the event is plaintext while we require encryption, which is a downgrade.
func DecodeContent(
	key *Key,
	e event.Event,
	swapID, messageType string,
	mode Mode,
) (map[string]any, error) {
	if key == nil {
		return nil, fmt.Errorf("decoding coordination content needs an identity")
	}

	//
	// Filter by the event's own signed type first. The encryption policy is
	// keyed on the message type being looked for, and applying it before this
	// filter would make an unrelated plaintext event look like a downgrade of a
	// settlement message.
	//
	if declaredType, ok := EventMessageType(e); !ok || declaredType != messageType {
		return nil, nil
	}

	declared, hasDeclaration := EventEncryption(e)
	raw := e.Content

	if raw == "" {
		return nil, nil
	}

	var payload []byte

	if hasDeclaration {
		if declared != EncryptionTag {
			return nil, fmt.Errorf(
				"SECURITY FAILURE: coordination event declares an unsupported encryption %q",
				declared,
			)
		}

		if mode != ModeNIP44 {
			return nil, fmt.Errorf(
				"SECURITY FAILURE: coordination event is encrypted but " +
					"BLAKE_SWAP_NOSTR_ENCRYPTION is not enabled; refusing to guess at its content",
			)
		}

		secret, err := keys.FromSecretHex(key.Secret)

		if err != nil {
			return nil, err
		}

		peer, err := hex.DecodeString(e.Pubkey)

		if err != nil {
			return nil, fmt.Errorf("event pubkey must be 32 bytes of hex")
		}

		conversationKey, err := nip44.ConversationKey(secret.Secret(), peer)

		if err != nil {
			return nil, err
		}

		plaintext, err := nip44.Decrypt(conversationKey, raw)

		if err != nil {
			return nil, fmt.Errorf(
				"SECURITY FAILURE: coordination event declares %s but did not decrypt: %w",
				EncryptionTag, err,
			)
		}

		payload = []byte(plaintext)
	} else {
		if nip44.LooksEncrypted(raw) {
			//
			// A payload with no encryption tag. Either the sender is
			// misconfigured or something is stripping the tag; both are refused
			// rather than treated as plaintext.
			//
			return nil, fmt.Errorf(
				"SECURITY FAILURE: coordination content looks like a %s payload "+
					"but carries no encryption tag", EncryptionTag,
			)
		}

		if RequiresEncryption(mode, messageType) {
			return nil, fmt.Errorf(
				"SECURITY FAILURE: refusing a plaintext coordination event while "+
					"BLAKE_SWAP_NOSTR_ENCRYPTION=%s is required (downgrade attempt)",
				EncryptionTag,
			)
		}

		payload = []byte(raw)
	}

	var message map[string]any

	if err := json.Unmarshal(payload, &message); err != nil {
		if hasDeclaration {
			return nil, fmt.Errorf(
				"SECURITY FAILURE: decrypted coordination content is not a JSON message: %w",
				err,
			)
		}

		return nil, nil
	}

	if message == nil {
		return nil, nil
	}

	if message["swap_id"] != swapID || message["type"] != messageType {
		return nil, nil
	}

	return message, nil
}

// Filter is a NIP-01 subscription filter, reduced to the shapes coordination uses.
type Filter struct {
	Kinds   []int
	Authors []string
	DTags   []string
	Limit   int
}

// request renders the filter as a subscription object.
func (f Filter) request() map[string]any {
	object := map[string]any{
		"kinds": f.Kinds,
	}

	if len(f.Authors) > 0 {
		object["authors"] = f.Authors
	}

	if len(f.DTags) > 0 {
		object["#d"] = f.DTags
	}

	if f.Limit > 0 {
		object["limit"] = f.Limit
	}

	return object
}

// RelayResult is one relay's answer to a publish.
type RelayResult struct {
	Relay        string
	Accepted     bool
	AuthRequired bool
	Message      []any
	Err          error
}

// Report is the result of publishing to every configured relay.
type Report struct {
	EventID  string
	Relays   []RelayResult
	Accepted int
}

// AcceptedRelays lists the relays that took the event.
func (r Report) AcceptedRelays() []string {
	var accepted []string

	for _, relay := range r.Relays {
		if relay.Accepted {
			accepted = append(accepted, relay.Relay)
		}
	}

	sort.Strings(accepted)

	return accepted
}

// Client publishes and queries coordination events.
type Client struct {
	// Dialer opens relay connections.
	Dialer Dialer
	// Relays are the configured relay URLs.
	Relays []string
	// Timeout bounds each connection and read. Zero means DefaultTimeout.
	Timeout time.Duration
	// Cache keeps our own signed events before any relay sees them. It is
	// optional.
	Cache Cache
	// Now is the clock used to verify events. Zero means time.Now.
	Now func() time.Time
}

// DefaultTimeout bounds one relay interaction.
const DefaultTimeout = 15 * time.Second

func (c *Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}

	return DefaultTimeout
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}

	return time.Now()
}

// Publish sends one signed event to every configured relay.
//
// The event is cached before it is offered to any relay: our own exact signed
// event is the only copy we can be sure of, and a relay's acceptance is not a
// storage guarantee. One accepting relay is enough; zero is an error, because an
// event nobody holds is an event the counterparty will never see.
func (c *Client) Publish(ctx context.Context, e event.Event) (Report, error) {
	if len(c.Relays) == 0 {
		return Report{}, fmt.Errorf("no Nostr relays configured")
	}

	if err := checkPublishableSize(e); err != nil {
		return Report{}, err
	}

	if err := event.Verify(e, c.now()); err != nil {
		return Report{}, fmt.Errorf("refusing to publish an event that does not verify: %w", err)
	}

	if c.Cache != nil {
		if err := c.Cache.Put(ctx, e); err != nil {
			return Report{}, err
		}
	}

	report := Report{EventID: e.ID}

	for _, relay := range c.Relays {
		result := c.publishToRelay(ctx, relay, e)

		if result.Accepted {
			report.Accepted++
		}

		report.Relays = append(report.Relays, result)
	}

	if report.Accepted == 0 {
		return report, fmt.Errorf("No configured Nostr relay accepted the event")
	}

	return report, nil
}

// publishToRelay sends one event and waits for its acknowledgement.
func (c *Client) publishToRelay(ctx context.Context, relay string, e event.Event) RelayResult {
	result := RelayResult{Relay: relay}

	conn, err := c.dial(ctx, relay)

	if err != nil {
		result.Err = err

		return result
	}

	defer func() { _ = conn.Close() }()

	payload, err := json.Marshal([]any{"EVENT", e})

	if err != nil {
		result.Err = err

		return result
	}

	attemptCtx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()

	if err := conn.Send(attemptCtx, string(payload)); err != nil {
		result.Err = err

		return result
	}

	for {
		raw, err := conn.Receive(attemptCtx)

		if err != nil {
			result.Err = err

			return result
		}

		frame, err := parseFrame(raw)

		if err != nil {
			result.Err = err

			return result
		}

		if len(frame) == 0 {
			continue
		}

		switch frame[0] {
		case "AUTH":
			// Some relays ask for authentication before acknowledging; that is
			// not an acceptance, and pretending otherwise would report a
			// message as published when the relay will drop it.
			result.AuthRequired = true
			result.Message = frame

			return result
		case "OK":
			if len(frame) >= 3 && frame[1] == e.ID {
				accepted, _ := frame[2].(bool)
				result.Accepted = accepted
				result.Message = frame

				return result
			}
		case "NOTICE":
			result.Message = frame
		}
	}
}

// Fetch queries every relay and returns the distinct events that verified.
//
// Relays and their contents are untrusted, so an event that does not verify is
// dropped rather than reported: a single relay can otherwise pretend to be the
// counterparty. One relay answering is enough to proceed; none answering is an
// error, because "no message" and "no relay" are different conditions.
func (c *Client) Fetch(ctx context.Context, filter Filter) ([]event.Event, error) {
	if len(c.Relays) == 0 {
		return nil, fmt.Errorf("no Nostr relays configured")
	}

	byID := map[string]event.Event{}
	answered := 0

	for _, relay := range c.Relays {
		events, err := c.fetchFromRelay(ctx, relay, filter)

		if err != nil {
			continue
		}

		answered++

		for _, candidate := range events {
			if candidate.ID == "" {
				continue
			}

			if _, seen := byID[candidate.ID]; seen {
				continue
			}

			if err := event.Verify(candidate, c.now()); err != nil {
				continue
			}

			byID[candidate.ID] = candidate
		}
	}

	if answered == 0 {
		return nil, fmt.Errorf("Could not query any configured Nostr relay")
	}

	events := make([]event.Event, 0, len(byID))

	for _, e := range byID {
		events = append(events, e)
	}

	// Newest first, with the id as a deterministic tie-break: two events can
	// share a timestamp, and picking arbitrarily would make the result depend on
	// map iteration order.
	sort.Slice(events, func(i, j int) bool {
		if events[i].CreatedAt != events[j].CreatedAt {
			return events[i].CreatedAt > events[j].CreatedAt
		}

		return events[i].ID < events[j].ID
	})

	return events, nil
}

// fetchFromRelay opens a subscription, reads until EOSE and closes it.
func (c *Client) fetchFromRelay(ctx context.Context, relay string, filter Filter) ([]event.Event, error) {
	conn, err := c.dial(ctx, relay)

	if err != nil {
		return nil, err
	}

	defer func() { _ = conn.Close() }()

	attemptCtx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()

	subscription := "blake-swap-" + randomSuffix()

	request, err := json.Marshal([]any{"REQ", subscription, filter.request()})

	if err != nil {
		return nil, err
	}

	if err := conn.Send(attemptCtx, string(request)); err != nil {
		return nil, err
	}

	var events []event.Event

	for {
		raw, err := conn.Receive(attemptCtx)

		if err != nil {
			return events, err
		}

		frame, err := parseFrame(raw)

		if err != nil {
			return events, err
		}

		if len(frame) < 2 || frame[1] != subscription {
			continue
		}

		switch frame[0] {
		case "EVENT":
			if len(frame) < 3 {
				continue
			}

			encoded, err := json.Marshal(frame[2])

			if err != nil {
				continue
			}

			var candidate event.Event

			if err := json.Unmarshal(encoded, &candidate); err != nil {
				continue
			}

			events = append(events, candidate)
		case "EOSE":
			close, err := json.Marshal([]any{"CLOSE", subscription})

			if err == nil {
				_ = conn.Send(attemptCtx, string(close))
			}

			return events, nil
		case "CLOSED":
			return events, fmt.Errorf("the relay closed the subscription")
		}
	}
}

// dial opens a connection with the client's timeout.
func (c *Client) dial(ctx context.Context, relay string) (Conn, error) {
	if c.Dialer == nil {
		return nil, fmt.Errorf("no relay dialer configured")
	}

	dialCtx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()

	return c.Dialer(dialCtx, relay)
}

// parseFrame decodes one relay frame.
func parseFrame(raw string) ([]any, error) {
	var frame []any

	if err := json.Unmarshal([]byte(raw), &frame); err != nil {
		return nil, fmt.Errorf("relay sent a frame that is not a JSON array: %w", err)
	}

	return frame, nil
}
