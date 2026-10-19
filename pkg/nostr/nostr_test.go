package nostr

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/keys"
	"github.com/gjbrr/blake-swap/pkg/nip44"
)

// scriptedConn is one relay connection whose answers are decided in advance.
type scriptedConn struct {
	responses []string
	sent      []string
	index     int
	closed    bool
}

func (c *scriptedConn) Send(_ context.Context, payload string) error {
	c.sent = append(c.sent, payload)

	return nil
}

func (c *scriptedConn) Receive(context.Context) (string, error) {
	if c.index >= len(c.responses) {
		return "", io.EOF
	}

	response := c.responses[c.index]
	c.index++

	if response == "OK:echo" {
		return `["OK","` + lastEventID(c.sent) + `",true]`, nil
	}

	if response == "OK:rejected" {
		return `["OK","` + lastEventID(c.sent) + `",false,"blocked: no"]`, nil
	}

	return response, nil
}

func (c *scriptedConn) Close() error {
	c.closed = true

	return nil
}

// lastEventID recovers the id of the event a connection was last sent.
func lastEventID(sent []string) string {
	if len(sent) == 0 {
		return ""
	}

	var frame []json.RawMessage

	if err := json.Unmarshal([]byte(sent[len(sent)-1]), &frame); err != nil {
		return ""
	}

	if len(frame) < 2 {
		return ""
	}

	var e struct {
		ID string `json:"id"`
	}

	if err := json.Unmarshal(frame[1], &e); err != nil {
		return ""
	}

	return e.ID
}

// relaySet is a dialer over a map of scripted connections.
type relaySet struct {
	scripts map[string][]string
	dialled []string
}

func (r *relaySet) dial(ctx context.Context, url string) (Conn, error) {
	r.dialled = append(r.dialled, url)

	script, ok := r.scripts[url]

	if !ok {
		return nil, fmt.Errorf("no script for %s", url)
	}

	return &scriptedConn{responses: script}, nil
}

// identities returns two deterministic collaborative identities.
func identities(t *testing.T) (*Key, *Key) {
	t.Helper()

	alice, err := NewKey(strings.Repeat("11", 32))

	if err != nil {
		t.Fatal(err)
	}

	bob, err := NewKey(strings.Repeat("22", 32))

	if err != nil {
		t.Fatal(err)
	}

	return alice, bob
}

// message is one coordination message.
func message(swapID, kind string) map[string]any {
	return map[string]any{
		"swap_id":      swapID,
		"type":         kind,
		"blake_amount": "0.00100000",
	}
}

// Parity: tests/swap_encrypted_coordination_tests.py
func TestParseMode(t *testing.T) {
	for raw, want := range map[string]Mode{
		"":           ModeOff,
		"0":          ModeOff,
		"false":      ModeOff,
		"no":         ModeOff,
		"off":        ModeOff,
		" NIP44_V2 ": ModeNIP44,
	} {
		got, err := ParseMode(raw)

		if err != nil || got != want {
			t.Fatalf("ParseMode(%q) = %q, %v", raw, got, err)
		}
	}

	for _, raw := range []string{"on", "yes", "nip44", "v2"} {
		if _, err := ParseMode(raw); err == nil {
			t.Fatalf("ParseMode(%q) was accepted", raw)
		}
	}
}

func TestPlaintextRoundTrip(t *testing.T) {
	alice, _ := identities(t)

	swapID := "aaaa00000001"

	built, err := BuildCoordinationEvent(alice, swapID, "offer", message(swapID, "offer"), BuildOptions{
		Mode: ModeOff, CreatedAt: 1_700_000_000,
	})

	if err != nil {
		t.Fatal(err)
	}

	if err := event.Verify(built, time.Unix(1_700_000_000, 0)); err != nil {
		t.Fatalf("the built event does not verify: %v", err)
	}

	decoded, err := DecodeContent(alice, built, swapID, "offer", ModeOff)

	if err != nil {
		t.Fatal(err)
	}

	if decoded == nil || decoded["swap_id"] != swapID || decoded["type"] != "offer" {
		t.Fatalf("decoded = %v", decoded)
	}

	// The same event is not the message being looked for under another type or
	// another swap, and that is not an error.
	for _, probe := range []struct{ swapID, kind string }{
		{swapID, "accept"},
		{"aaaa00000002", "offer"},
	} {
		other, err := DecodeContent(alice, built, probe.swapID, probe.kind, ModeOff)

		if err != nil || other != nil {
			t.Fatalf("probe %+v: %v, %v", probe, other, err)
		}
	}
}

func TestEncryptingRequiresAPeer(t *testing.T) {
	alice, _ := identities(t)

	_, err := BuildCoordinationEvent(alice, "aaaa00000001", "accept", message("aaaa00000001", "accept"), BuildOptions{
		Mode: ModeNIP44,
	})

	if err == nil || !strings.Contains(err.Error(), "encrypt to") {
		t.Fatalf("err = %v", err)
	}
}

func TestEncryptedRoundTrip(t *testing.T) {
	alice, bob := identities(t)

	swapID := "aaaa00000001"

	built, err := BuildCoordinationEvent(alice, swapID, "accept", message(swapID, "accept"), BuildOptions{
		Mode:       ModeNIP44,
		PeerPubkey: bob.Public,
		CreatedAt:  1_700_000_000,
	})

	if err != nil {
		t.Fatal(err)
	}

	if declared, ok := EventEncryption(built); !ok || declared != EncryptionTag {
		t.Fatalf("the event declares %q, %v", declared, ok)
	}

	if strings.Contains(built.Content, "swap_id") {
		t.Fatalf("the content is plaintext: %q", built.Content)
	}

	decoded, err := DecodeContent(bob, built, swapID, "accept", ModeNIP44)

	if err != nil {
		t.Fatal(err)
	}

	if decoded == nil || decoded["swap_id"] != swapID {
		t.Fatalf("decoded = %v", decoded)
	}

	//
	// Reading it back as the author is a different key: NIP-44 decrypts with
	// (own secret, author's pubkey), and here the author is Alice, so the pair
	// is (Alice, Alice), which is not the pair that encrypted it. The engine
	// only ever decodes the counterparty's events, so this is the boundary of
	// the function rather than a defect — and it is worth pinning, because a
	// future "read my own message" feature would silently get the wrong key.
	//
	if _, err := DecodeContent(alice, built, swapID, "accept", ModeNIP44); err == nil {
		t.Fatal("the author's own event decrypted with the author's own key")
	}
}

func TestDecodeRefusesEncryptedWithoutTheMode(t *testing.T) {
	alice, bob := identities(t)

	swapID := "aaaa00000001"

	built, err := BuildCoordinationEvent(alice, swapID, "accept", message(swapID, "accept"), BuildOptions{
		Mode:       ModeNIP44,
		PeerPubkey: bob.Public,
		CreatedAt:  1_700_000_000,
	})

	if err != nil {
		t.Fatal(err)
	}

	_, err = DecodeContent(bob, built, swapID, "accept", ModeOff)

	if err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeRefusesAnUntaggedEncryptedPayload(t *testing.T) {
	alice, bob := identities(t)

	swapID := "aaaa00000001"

	// Encrypt by hand, then declare nothing: this is what a stripped tag looks
	// like, and it must not be treated as plaintext.
	conversationKey, err := nip44.ConversationKey(mustHex(alice.Secret), mustHex(bob.Public))

	if err != nil {
		t.Fatal(err)
	}

	encrypted, err := nip44.Encrypt(conversationKey, []byte(`{"swap_id":"aaaa00000001","type":"accept"}`), nil)

	if err != nil {
		t.Fatal(err)
	}

	built, err := event.Build(bob.Public, event.WithContent([]byte(encrypted)), event.WithTags(
		event.CoordinationTags(swapID, "accept"),
	))

	if err != nil {
		t.Fatal(err)
	}

	built.CreatedAt = 1_700_000_000

	id, err := built.ComputeID()

	if err != nil {
		t.Fatal(err)
	}

	built.ID = id

	signer, err := keys.FromSecretHex(bob.Secret)

	if err != nil {
		t.Fatal(err)
	}

	if err := event.Sign(&built, signer); err != nil {
		t.Fatal(err)
	}

	_, err = DecodeContent(alice, built, swapID, "accept", ModeOff)

	if err == nil || !strings.Contains(err.Error(), "no encryption tag") {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeRefusesAPlaintextDowngrade(t *testing.T) {
	alice, _ := identities(t)

	swapID := "aaaa00000001"

	built, err := BuildCoordinationEvent(alice, swapID, "accept", message(swapID, "accept"), BuildOptions{
		Mode: ModeOff, CreatedAt: 1_700_000_000,
	})

	if err != nil {
		t.Fatal(err)
	}

	_, err = DecodeContent(alice, built, swapID, "accept", ModeNIP44)

	if err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("err = %v", err)
	}
}

func TestPublishReportsEveryRelay(t *testing.T) {
	alice, _ := identities(t)

	swapID := "aaaa00000001"

	built, err := BuildCoordinationEvent(alice, swapID, "offer", message(swapID, "offer"), BuildOptions{
		Mode: ModeOff, CreatedAt: time.Now().Unix(),
	})

	if err != nil {
		t.Fatal(err)
	}

	relays := &relaySet{scripts: map[string][]string{
		"wss://a": {"OK:echo"},
		"wss://b": {"OK:rejected"},
		"wss://c": nil, // the connection answers nothing, as a dead relay would
	}}

	cache := NewMemoryCache()

	client := &Client{Dialer: relays.dial, Relays: []string{"wss://a", "wss://b", "wss://c"}, Cache: cache}

	report, err := client.Publish(context.Background(), built)

	if err != nil {
		t.Fatalf("one accepting relay should be enough: %v", err)
	}

	if report.Accepted != 1 || len(report.Relays) != 3 {
		t.Fatalf("report = %+v", report)
	}

	if got := report.AcceptedRelays(); len(got) != 1 || got[0] != "wss://a" {
		t.Fatalf("accepted = %v", got)
	}

	if _, found, _ := cache.Get(context.Background(), built.ID); !found {
		t.Fatal("the event was not cached")
	}
}

func TestPublishCachesEvenWhenNoRelayAccepts(t *testing.T) {
	alice, _ := identities(t)

	swapID := "aaaa00000001"

	built, err := BuildCoordinationEvent(alice, swapID, "offer", message(swapID, "offer"), BuildOptions{
		Mode: ModeOff, CreatedAt: time.Now().Unix(),
	})

	if err != nil {
		t.Fatal(err)
	}

	relays := &relaySet{scripts: map[string][]string{"wss://a": {"OK:rejected"}}}

	cache := NewMemoryCache()

	client := &Client{Dialer: relays.dial, Relays: []string{"wss://a"}, Cache: cache}

	if _, err := client.Publish(context.Background(), built); err == nil {
		t.Fatal("an event no relay accepted was reported as published")
	}

	// Our own signed copy has to survive a relay refusing it: it is the only
	// copy we can be sure of.
	if _, found, _ := cache.Get(context.Background(), built.ID); !found {
		t.Fatal("the event was not cached before the relays refused it")
	}
}

func TestAuthIsNotAcceptance(t *testing.T) {
	alice, _ := identities(t)

	swapID := "aaaa00000001"

	built, err := BuildCoordinationEvent(alice, swapID, "offer", message(swapID, "offer"), BuildOptions{
		Mode: ModeOff, CreatedAt: time.Now().Unix(),
	})

	if err != nil {
		t.Fatal(err)
	}

	relays := &relaySet{scripts: map[string][]string{
		"wss://a": {`["AUTH","challenge-123"]`},
	}}

	client := &Client{Dialer: relays.dial, Relays: []string{"wss://a"}}

	report, err := client.Publish(context.Background(), built)

	if err == nil {
		t.Fatal("a relay asking for authentication was treated as accepting")
	}

	if !report.Relays[0].AuthRequired || report.Relays[0].Accepted {
		t.Fatalf("result = %+v", report.Relays[0])
	}
}

func TestFetchMergesVerifiesAndSorts(t *testing.T) {
	alice, bob := identities(t)

	swapID := "aaaa00000001"

	older, err := BuildCoordinationEvent(alice, swapID, "offer", message(swapID, "offer"), BuildOptions{
		Mode: ModeOff, CreatedAt: 1_700_000_000,
	})

	if err != nil {
		t.Fatal(err)
	}

	newer, err := BuildCoordinationEvent(alice, swapID, "offer", message(swapID, "offer"), BuildOptions{
		Mode: ModeOff, CreatedAt: 1_700_000_100,
	})

	if err != nil {
		t.Fatal(err)
	}

	// A forged event: correct shape, signed by the wrong key for its pubkey.
	forged := newer
	forged.ID = strings.Repeat("aa", 32)
	forged.Sig = strings.Repeat("00", 64)

	// The subscription id is generated by the client, so the relay script cannot
	// know it; the client ignores frames whose id does not match, which is what
	// the real client does too. Script the EOSE with a wildcard instead by
	// having the fake derive it from the request.
	relays := &echoRelay{
		events: map[string][]event.Event{
			"wss://a": {older, forged},
			"wss://b": {newer, older},
		},
	}

	client := &Client{Dialer: relays.dial, Relays: []string{"wss://a", "wss://b"}, Now: func() time.Time {
		return time.Unix(1_700_000_200, 0)
	}}

	events, err := client.Fetch(context.Background(), Filter{Kinds: []int{1}, DTags: []string{swapID}, Limit: 20})

	if err != nil {
		t.Fatal(err)
	}

	if len(events) != 2 {
		t.Fatalf("events = %d: %+v", len(events), events)
	}

	if events[0].ID != newer.ID {
		t.Fatalf("the newest event is not first")
	}

	if events[1].ID != older.ID {
		t.Fatalf("the older event is not second")
	}

	for _, e := range events {
		if e.ID == forged.ID {
			t.Fatal("a forged event survived verification")
		}
	}

	_ = bob
}

// echoRelay is a dialer whose connections echo a fixed set of events with the
// subscription id they were asked for, then an EOSE.
type echoRelay struct {
	events map[string][]event.Event
}

func (r *echoRelay) dial(ctx context.Context, url string) (Conn, error) {
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
	sub       string
}

func (c *echoConn) Send(_ context.Context, payload string) error {
	var frame []json.RawMessage

	if err := json.Unmarshal([]byte(payload), &frame); err != nil {
		return err
	}

	if len(frame) >= 2 && string(frame[0]) == `"REQ"` {
		if err := json.Unmarshal(frame[1], &c.sub); err != nil {
			return err
		}

		c.responses = nil

		for _, e := range c.events {
			encoded, err := json.Marshal(e)

			if err != nil {
				return err
			}

			c.responses = append(c.responses, `["EVENT","`+c.sub+`",`+string(encoded)+`]`)
		}

		c.responses = append(c.responses, `["EOSE","`+c.sub+`"]`)
	}

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

func TestFetchFailsWhenNoRelayAnswers(t *testing.T) {
	client := &Client{
		Dialer: func(context.Context, string) (Conn, error) {
			return nil, fmt.Errorf("connection refused")
		},
		Relays: []string{"wss://a", "wss://b"},
	}

	if _, err := client.Fetch(context.Background(), Filter{Kinds: []int{1}}); err == nil {
		t.Fatal("no relay answering was reported as an empty result")
	}
}

func TestOversizedEventIsRefused(t *testing.T) {
	alice, _ := identities(t)

	huge := map[string]any{
		"swap_id": "aaaa00000001",
		"type":    "offer",
		"pad":     strings.Repeat("x", MaxEventBytes),
	}

	if _, err := BuildCoordinationEvent(alice, "aaaa00000001", "offer", huge, BuildOptions{
		Mode: ModeOff, CreatedAt: 1_700_000_000,
	}); err == nil {
		t.Fatal("an oversized event was built")
	}
}

func TestCacheRefusesAnUnverifiedEvent(t *testing.T) {
	cache := NewMemoryCache()

	forged := event.Event{
		Pubkey:    strings.Repeat("11", 32),
		CreatedAt: 1_700_000_000,
		Kind:      1,
		Tags:      [][]string{},
		Content:   "{}",
		ID:        strings.Repeat("aa", 32),
		Sig:       strings.Repeat("00", 64),
	}

	if err := cache.Put(context.Background(), forged); err == nil {
		t.Fatal("an unverified event was cached")
	}
}

func mustHex(value string) []byte {
	decoded, err := hex.DecodeString(value)

	if err != nil {
		panic(err)
	}

	return decoded
}
