// Package coord is how coordination messages travel, independent of the route.
//
// Two routes exist and they differ in almost everything except the message. The
// file route is a local spool for tests and single-host runs; the Nostr route is
// a network of relays that can be down, hostile, or both. What the swap flow
// above this package needs from either is the same: send this message as this
// role, and give me the counterparty's message for this swap and type — or a
// refusal, never a guess.
//
// The validations are the same on both routes by construction. A message that
// would not be allowed onto the wire is refused here, before a transport has a
// chance to be more permissive than the protocol.
package coord

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/store"
	"github.com/gjbrr/blake-swap/pkg/wire"
)

// Message is one coordination message as it arrived.
type Message struct {
	// Kind is the message type.
	Kind string
	// Sender is the authenticated author, x-only hex. It is transport metadata:
	// it comes from the signed envelope, not from the message body.
	Sender string
	// Body is the message itself.
	Body map[string]any
	// Location is where it was found — a spool path or an event id — for
	// operator output.
	Location string
}

// Transport is one route for coordination messages.
type Transport interface {
	// Send delivers one message and returns its location.
	Send(ctx context.Context, swapID, kind string, body map[string]any, opts SendOptions) (string, error)
	// Receive returns the counterparty's message for a swap and type.
	Receive(ctx context.Context, swapID, kind string, opts ReceiveOptions) (*Message, error)
}

// SendOptions is what the flow knows about the message being sent.
type SendOptions struct {
	// LocalRole is the protocol role this participant holds in the swap.
	LocalRole string
	// ExpectedRole is the role the protocol requires to send this message type.
	// When both are set and they differ, the send is refused: signing a
	// settlement message as the wrong role is the one mistake that makes every
	// later verification meaningless.
	ExpectedRole string
	// PeerPubkey is the counterparty's x-only Nostr identity. It is required when
	// the message must be encrypted, because NIP-44 is pairwise.
	PeerPubkey string
	// Mode is the coordination confidentiality mode.
	Mode nostr.Mode
	// CreatedAt pins the event time. Zero means the transport chooses.
	CreatedAt int64
}

// ReceiveOptions is what the flow knows about the message being awaited.
type ReceiveOptions struct {
	// ExpectedPubkey is the pinned counterparty identity for this message type.
	// When set, a message signed by anyone else is refused.
	ExpectedPubkey string
	// Mode is the coordination confidentiality mode.
	Mode nostr.Mode
}

// SettlementTypes are the message types the wire schema governs.
var SettlementTypes = map[string]bool{
	"offer":         true,
	"accept":        true,
	"blake-funded":  true,
	"btc-funded":    true,
	"btc-claimed":   true,
	"blake-claimed": true,
	"btc-refunded":  true,
}

// Validate is the check every transport runs, in both directions.
//
// The schema is authoritative rather than a list of forbidden names: it rejects
// unknown fields, so the engine and the wire specification cannot drift apart
// without a test failing. The named checks that remain are there because they
// report a security failure instead of a schema error.
func Validate(swapID, kind string, body map[string]any, allowInternal bool) error {
	if body == nil {
		return fmt.Errorf("a coordination message must have a body")
	}

	if body["type"] != kind {
		return fmt.Errorf("message type does not match transport type")
	}

	if body["swap_id"] != swapID {
		return fmt.Errorf("message swap_id does not match transport swap_id")
	}

	//
	// Alice's original secret must never travel through the coordination layer.
	// The schema below would also refuse an unknown field, but this one is worth
	// naming: it is the difference between a wire-format mistake and a protocol
	// compromise.
	//
	if _, present := body["secret_hex"]; present {
		return fmt.Errorf("SECURITY FAILURE: refusing to transport secret_hex")
	}

	if !SettlementTypes[kind] {
		return nil
	}

	var internal []string

	for key := range body {
		if strings.HasPrefix(key, "_") {
			internal = append(internal, key)
		}
	}

	if len(internal) > 0 && !allowInternal {
		return fmt.Errorf(
			"SECURITY FAILURE: refusing to transmit engine-internal field(s) %v on the wire",
			sorted(internal),
		)
	}

	view := WireView(body)

	expected := wire.Kind(kind)

	if _, err := wire.Validate(view, &expected); err != nil {
		return fmt.Errorf(
			"SECURITY FAILURE: %s message does not satisfy the canonical wire schema: %w",
			kind, err,
		)
	}

	return nil
}

// WireView is a message without its engine-internal annotations.
//
// The receive path attaches the authenticated sender as `_nostr_pubkey`, and the file
// envelope carries its own underscore keys. Those are transport bookkeeping rather than
// protocol content, so the canonical schema is applied to the message without them — a key
// is internal exactly when it begins with an underscore, and no wire field does.
func WireView(body map[string]any) map[string]any {
	view := make(map[string]any, len(body))

	for key, value := range body {
		if strings.HasPrefix(key, "_") {
			continue
		}

		view[key] = value
	}

	return view
}

// sorted returns a copy of a string list in ascending order, so an error message
// never depends on map iteration order.
func sorted(values []string) []string {
	ordered := make([]string, len(values))
	copy(ordered, values)
	sort.Strings(ordered)

	return ordered
}

// checkRole enforces the local protocol role the message type requires.
func checkRole(kind string, opts SendOptions) error {
	if opts.ExpectedRole == "" {
		return nil
	}

	if opts.ExpectedRole != "alice" && opts.ExpectedRole != "bob" {
		return fmt.Errorf("expected role must be alice or bob")
	}

	if opts.LocalRole != opts.ExpectedRole {
		return fmt.Errorf(
			"SECURITY FAILURE: refusing to sign %s as protocol %q; protocol requires %q",
			kind, opts.LocalRole, opts.ExpectedRole,
		)
	}

	return nil
}

// Spool is the local message store the file route needs.
//
// It is the subset of pkg/store.Store that coordination uses, so a browser store
// can stand in for the directory without either side knowing about the other.
type Spool interface {
	SendMessage(ctx context.Context, swapID, kind, sender string, body map[string]any) error
	ReceiveMessage(ctx context.Context, swapID, kind string) (*store.Message, error)
}

// FileTransport is the local spool route.
//
// It is a test and single-host backend, not an authenticated network: the sender
// identity it records is the one the local participant selected, so the
// identity-binding checks above it run the same way on both routes.
type FileTransport struct {
	Spool Spool
	// Sender is the local participant's x-only Nostr identity.
	Sender string
}

// Send implements Transport.
func (f *FileTransport) Send(
	ctx context.Context, swapID, kind string, body map[string]any, opts SendOptions,
) (string, error) {
	if err := Validate(swapID, kind, body, false); err != nil {
		return "", err
	}

	if err := checkRole(kind, opts); err != nil {
		return "", err
	}

	if f.Spool == nil {
		return "", fmt.Errorf("the file transport has no spool")
	}

	if err := store.ValidateSender(f.Sender); err != nil {
		return "", err
	}

	if err := f.Spool.SendMessage(ctx, swapID, kind, f.Sender, body); err != nil {
		return "", err
	}

	return fmt.Sprintf("%s.%s", swapID, kind), nil
}

// Receive implements Transport.
func (f *FileTransport) Receive(
	ctx context.Context, swapID, kind string, opts ReceiveOptions,
) (*Message, error) {
	if f.Spool == nil {
		return nil, fmt.Errorf("the file transport has no spool")
	}

	stored, err := f.Spool.ReceiveMessage(ctx, swapID, kind)

	if err != nil {
		return nil, err
	}

	if opts.ExpectedPubkey != "" {
		if stored.Sender == "" {
			return nil, fmt.Errorf(
				"SECURITY FAILURE: %s has no authenticated sender", kind,
			)
		}

		if stored.Sender != opts.ExpectedPubkey {
			return nil, fmt.Errorf(
				"SECURITY FAILURE: %s was signed by the wrong Nostr identity",
				kind,
			)
		}
	}

	if err := Validate(swapID, kind, stored.Body, true); err != nil {
		return nil, err
	}

	return &Message{
		Kind:     kind,
		Sender:   stored.Sender,
		Body:     stored.Body,
		Location: stored.Path,
	}, nil
}

// NostrTransport is the relay route.
type NostrTransport struct {
	Client   *nostr.Client
	Identity *nostr.Key
}

// Send implements Transport.
func (n *NostrTransport) Send(
	ctx context.Context, swapID, kind string, body map[string]any, opts SendOptions,
) (string, error) {
	if err := Validate(swapID, kind, body, false); err != nil {
		return "", err
	}

	if err := checkRole(kind, opts); err != nil {
		return "", err
	}

	if n.Client == nil || n.Identity == nil {
		return "", fmt.Errorf("the nostr transport has no client or identity")
	}

	built, err := nostr.BuildCoordinationEvent(n.Identity, swapID, kind, body, nostr.BuildOptions{
		Mode:       opts.Mode,
		PeerPubkey: opts.PeerPubkey,
		CreatedAt:  opts.CreatedAt,
	})

	if err != nil {
		return "", err
	}

	report, err := n.Client.Publish(ctx, built)

	if err != nil {
		return "", err
	}

	return report.EventID, nil
}

// Receive implements Transport.
//
// The relay is asked only for events the counterparty could have signed, but that
// filter is an optimisation; the authoritative check is the local one, applied
// after the signature has been verified.
func (n *NostrTransport) Receive(
	ctx context.Context, swapID, kind string, opts ReceiveOptions,
) (*Message, error) {
	if n.Client == nil || n.Identity == nil {
		return nil, fmt.Errorf("the nostr transport has no client or identity")
	}

	filter := nostr.Filter{
		Kinds: []int{event.KindTextNote},
		DTags: []string{swapID},
		Limit: 20,
	}

	if opts.ExpectedPubkey != "" {
		filter.Authors = []string{opts.ExpectedPubkey}
	}

	events, err := n.Client.Fetch(ctx, filter)

	if err != nil {
		return nil, err
	}

	var found *Message

	for i := range events {
		candidate := events[i]

		if opts.ExpectedPubkey != "" && candidate.Pubkey != opts.ExpectedPubkey {
			continue
		}

		body, err := nostr.DecodeContent(n.Identity, candidate, swapID, kind, opts.Mode)

		//
		// A declared encryption we cannot honour is a security condition, not a
		// relay artefact: report it rather than skipping to the next event, which
		// would hide exactly what the mode exists to detect.
		//
		if err != nil {
			return nil, err
		}

		if body == nil {
			continue
		}

		if err := Validate(swapID, kind, body, true); err != nil {
			return nil, err
		}

		// The fetch returns newest first, so the first match is the message.
		found = &Message{
			Kind:     kind,
			Sender:   candidate.Pubkey,
			Body:     body,
			Location: candidate.ID,
		}

		break
	}

	if found == nil {
		return nil, fmt.Errorf("No valid signed Nostr message matched: %w", store.ErrNotFound)
	}

	return found, nil
}
