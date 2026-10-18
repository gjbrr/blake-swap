package coord

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/store"
)

// swapID is a canonical identifier these tests use.
const swapID = "aaaa00000001"

// acceptMessage is a well-formed settlement message: exactly the fields the wire
// schema requires, and no others.
func acceptMessage() map[string]any {
	return map[string]any{
		"swap_id":       swapID,
		"type":          "accept",
		"secret_hash":   strings.Repeat("ab", 32),
		"blake_bob_pub": "03c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5",
		"btc_bob_pub":   "03c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5",
	}
}

func identities(t *testing.T) (*nostr.Key, *nostr.Key) {
	t.Helper()

	alice, err := nostr.NewKey(strings.Repeat("11", 32))

	if err != nil {
		t.Fatal(err)
	}

	bob, err := nostr.NewKey(strings.Repeat("22", 32))

	if err != nil {
		t.Fatal(err)
	}

	return alice, bob
}

// TestValidateRefusesTheThingsTheProtocolRefuses covers the checks every
// transport runs, in both directions.
func TestValidateRefusesTheThingsTheProtocolRefuses(t *testing.T) {
	good := acceptMessage()

	if err := Validate(swapID, "accept", good, false); err != nil {
		t.Fatalf("a well-formed message was refused: %v", err)
	}

	cases := map[string]struct {
		mutate  func(map[string]any)
		kind    string
		swap    string
		message string
	}{
		"wrong type": {
			mutate:  func(m map[string]any) { m["type"] = "offer" },
			message: "type does not match",
		},
		"wrong swap": {
			mutate:  func(m map[string]any) { m["swap_id"] = "aaaa00000002" },
			message: "swap_id does not match",
		},
		"the original secret": {
			mutate:  func(m map[string]any) { m["secret_hex"] = strings.Repeat("11", 32) },
			message: "refusing to transport secret_hex",
		},
		"an internal field on the wire": {
			mutate:  func(m map[string]any) { m["_nostr_pubkey"] = strings.Repeat("cd", 32) },
			message: "engine-internal field",
		},
		"an unknown field": {
			mutate:  func(m map[string]any) { m["negotiated_by"] = "someone" },
			message: "canonical wire schema",
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			message := acceptMessage()
			c.mutate(message)

			err := Validate(swapID, "accept", message, false)

			if err == nil || !strings.Contains(err.Error(), c.message) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// TestValidateAllowsInternalFieldsOnReceive: the receive path attaches the
// authenticated sender, so the internal-field refusal is a send-side rule.
func TestValidateAllowsInternalFieldsOnReceive(t *testing.T) {
	message := acceptMessage()
	message["_nostr_pubkey"] = strings.Repeat("cd", 32)

	if err := Validate(swapID, "accept", message, true); err != nil {
		t.Fatalf("the receive path refused its own metadata: %v", err)
	}
}

func TestFileTransportRoundTrip(t *testing.T) {
	spool := store.NewMemory()
	alice, _ := identities(t)

	transport := &FileTransport{Spool: spool, Sender: alice.Public}

	location, err := transport.Send(context.Background(), swapID, "accept", acceptMessage(), SendOptions{
		LocalRole: "bob", ExpectedRole: "bob",
	})

	if err != nil {
		t.Fatal(err)
	}

	if location == "" {
		t.Fatal("no location was reported")
	}

	received, err := transport.Receive(context.Background(), swapID, "accept", ReceiveOptions{
		ExpectedPubkey: alice.Public,
	})

	if err != nil {
		t.Fatal(err)
	}

	if received.Sender != alice.Public || received.Body["type"] != "accept" {
		t.Fatalf("received = %+v", received)
	}
}

// TestFileTransportRefusesTheWrongIdentity is the local mirror of the relay
// check: a message from anyone but the pinned counterparty is not the
// counterparty's message.
func TestFileTransportRefusesTheWrongIdentity(t *testing.T) {
	spool := store.NewMemory()
	alice, bob := identities(t)

	transport := &FileTransport{Spool: spool, Sender: alice.Public}

	if _, err := transport.Send(context.Background(), swapID, "accept", acceptMessage(), SendOptions{}); err != nil {
		t.Fatal(err)
	}

	_, err := transport.Receive(context.Background(), swapID, "accept", ReceiveOptions{
		ExpectedPubkey: bob.Public,
	})

	if err == nil || !strings.Contains(err.Error(), "wrong Nostr identity") {
		t.Fatalf("err = %v", err)
	}
}

func TestRoleGuardRefusesSigningAsTheWrongRole(t *testing.T) {
	alice, _ := identities(t)

	transport := &FileTransport{Spool: store.NewMemory(), Sender: alice.Public}

	_, err := transport.Send(context.Background(), swapID, "accept", acceptMessage(), SendOptions{
		LocalRole: "alice", ExpectedRole: "bob",
	})

	if err == nil || !strings.Contains(err.Error(), "protocol requires") {
		t.Fatalf("err = %v", err)
	}

	if kinds, _ := store.NewMemory().ListMessages(context.Background(), swapID); len(kinds) != 0 {
		t.Fatalf("a refused send wrote %v", kinds)
	}
}

// echoRelay is a dialer that answers a subscription with a fixed set of events.
type echoRelay struct {
	events map[string][]event.Event
}

func (r *echoRelay) dial(ctx context.Context, url string) (nostr.Conn, error) {
	events, ok := r.events[url]

	if !ok {
		return nil, fmt.Errorf("no script for %s", url)
	}

	return &echoConn{events: events}, nil
}

type echoConn struct {
	events    []event.Event
	responses []string
	index     int
}

func (c *echoConn) Send(_ context.Context, payload string) error {
	var frame []json.RawMessage

	if err := json.Unmarshal([]byte(payload), &frame); err != nil {
		return err
	}

	if len(frame) < 2 {
		return nil
	}

	var kind, sub string

	if err := json.Unmarshal(frame[0], &kind); err != nil {
		return err
	}

	if kind != "REQ" {
		return nil
	}

	if err := json.Unmarshal(frame[1], &sub); err != nil {
		return err
	}

	c.responses = nil

	for _, e := range c.events {
		encoded, err := json.Marshal(e)

		if err != nil {
			return err
		}

		c.responses = append(c.responses, `["EVENT","`+sub+`",`+string(encoded)+`]`)
	}

	c.responses = append(c.responses, `["EOSE","`+sub+`"]`)

	return nil
}

func (c *echoConn) Receive(context.Context) (string, error) {
	if c.index >= len(c.responses) {
		return "", io.EOF
	}

	response := c.responses[c.index]
	c.index++

	return response, nil
}

func (c *echoConn) Close() error { return nil }

// TestNostrTransportReceivesOnlyFromThePinnedAuthor: relay-side author filtering
// is an optimisation, so a relay that ignores it must not be able to hand over
// someone else's message.
func TestNostrTransportReceivesOnlyFromThePinnedAuthor(t *testing.T) {
	alice, bob := identities(t)

	encrypted, err := nostr.BuildCoordinationEvent(bob, swapID, "accept", acceptMessage(), nostr.BuildOptions{
		Mode:       nostr.ModeNIP44,
		PeerPubkey: alice.Public,
		CreatedAt:  1_700_000_000,
	})

	if err != nil {
		t.Fatal(err)
	}

	relay := &echoRelay{events: map[string][]event.Event{"wss://a": {encrypted}}}

	client := &nostr.Client{
		Dialer: relay.dial,
		Relays: []string{"wss://a"},
		Now:    func() time.Time { return time.Unix(1_700_000_100, 0) },
	}

	transport := &NostrTransport{Client: client, Identity: alice}

	message, err := transport.Receive(context.Background(), swapID, "accept", ReceiveOptions{
		ExpectedPubkey: bob.Public,
		Mode:           nostr.ModeNIP44,
	})

	if err != nil {
		t.Fatal(err)
	}

	if message.Sender != bob.Public || message.Body["type"] != "accept" {
		t.Fatalf("message = %+v", message)
	}

	// A pin that does not match the author is a refusal, not a miss.
	_, err = transport.Receive(context.Background(), swapID, "accept", ReceiveOptions{
		ExpectedPubkey: alice.Public,
		Mode:           nostr.ModeNIP44,
	})

	if err == nil {
		t.Fatal("a message from the wrong author was accepted under a pin")
	}
}

// TestNostrTransportRefusesADowngrade is the receive-side half of the encryption
// policy: the mode requires ciphertext, so plaintext is a refusal.
func TestNostrTransportRefusesADowngrade(t *testing.T) {
	alice, bob := identities(t)

	plaintext, err := nostr.BuildCoordinationEvent(bob, swapID, "accept", acceptMessage(), nostr.BuildOptions{
		Mode: nostr.ModeOff, CreatedAt: 1_700_000_000,
	})

	if err != nil {
		t.Fatal(err)
	}

	relay := &echoRelay{events: map[string][]event.Event{"wss://a": {plaintext}}}

	client := &nostr.Client{
		Dialer: relay.dial,
		Relays: []string{"wss://a"},
		Now:    func() time.Time { return time.Unix(1_700_000_100, 0) },
	}

	transport := &NostrTransport{Client: client, Identity: alice}

	_, err = transport.Receive(context.Background(), swapID, "accept", ReceiveOptions{
		ExpectedPubkey: bob.Public,
		Mode:           nostr.ModeNIP44,
	})

	if err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("err = %v", err)
	}
}

// TestNostrTransportSendRefusesTheWrongRole keeps the send-side guard on the
// route that actually publishes.
func TestNostrTransportSendRefusesTheWrongRole(t *testing.T) {
	alice, _ := identities(t)

	transport := &NostrTransport{
		Client:   &nostr.Client{Relays: []string{"wss://a"}},
		Identity: alice,
	}

	_, err := transport.Send(context.Background(), swapID, "accept", acceptMessage(), SendOptions{
		LocalRole: "alice", ExpectedRole: "bob",
	})

	if err == nil || !strings.Contains(err.Error(), "protocol requires") {
		t.Fatalf("err = %v", err)
	}
}
