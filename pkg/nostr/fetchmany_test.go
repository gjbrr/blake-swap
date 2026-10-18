package nostr

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/keys"
)

// topicRelay is a relay that answers a subscription according to the `#t` topics in
// each filter it was asked for, and records what it was asked.
type topicRelay struct {
	byTopic map[string][]event.Event
	// requests holds one entry per connection: the filters that arrived in its REQ.
	requests [][]map[string]any
}

func (r *topicRelay) dial(_ context.Context, _ string) (Conn, error) {
	return &topicConn{relay: r}, nil
}

// topicConn answers one connection.
type topicConn struct {
	relay     *topicRelay
	responses []string
	index     int
	sub       string
}

func (c *topicConn) Send(_ context.Context, payload string) error {
	var frame []json.RawMessage

	if err := json.Unmarshal([]byte(payload), &frame); err != nil {
		return err
	}

	if len(frame) < 2 || string(frame[0]) != `"REQ"` {
		return nil
	}

	if err := json.Unmarshal(frame[1], &c.sub); err != nil {
		return err
	}

	filters := make([]map[string]any, 0, len(frame)-2)

	seen := map[string]bool{}

	for _, raw := range frame[2:] {
		var filter map[string]any
		if err := json.Unmarshal(raw, &filter); err != nil {
			return err
		}

		filters = append(filters, filter)

		topics, _ := filter["#t"].([]any)

		for _, topic := range topics {
			name, _ := topic.(string)

			for _, e := range c.relay.byTopic[name] {
				if seen[e.ID] {
					continue
				}

				seen[e.ID] = true

				encoded, err := json.Marshal(e)
				if err != nil {
					return err
				}

				c.responses = append(c.responses, `["EVENT","`+c.sub+`",`+string(encoded)+`]`)
			}
		}
	}

	c.relay.requests = append(c.relay.requests, filters)
	c.responses = append(c.responses, `["EOSE","`+c.sub+`"]`)

	return nil
}

func (c *topicConn) Receive(context.Context) (string, error) {
	if c.index >= len(c.responses) {
		return "", fmt.Errorf("end of script")
	}

	response := c.responses[c.index]
	c.index++

	return response, nil
}

func (c *topicConn) Close() error { return nil }

// signedEvent builds a signed kind-1 event carrying one topic tag.
func signedEvent(t *testing.T, secret, topic string, createdAt int64) event.Event {
	t.Helper()

	key, err := NewKey(strings.Repeat(secret, 32))

	if err != nil {
		t.Fatal(err)
	}

	built, err := event.Build(key.PublicKey(),
		event.WithContent([]byte(`{"type":"market-offer"}`)),
		event.WithTags(event.MarketTags(topic, "blake-for-btc")),
		event.WithCreatedAt(createdAt),
	)

	if err != nil {
		t.Fatal(err)
	}

	id, err := built.ComputeID()

	if err != nil {
		t.Fatal(err)
	}

	built.ID = id

	signer, err := keys.FromSecretHex(key.Secret)

	if err != nil {
		t.Fatal(err)
	}

	if err := event.Sign(&built, signer); err != nil {
		t.Fatal(err)
	}

	return built
}

// TestFilterTopicsRendersAsTag: a topic filter must reach the relay as `#t`, or the
// relay answers with every kind-1 event it holds.
func TestFilterTopicsRendersAsTag(t *testing.T) {
	offer := signedEvent(t, "11", event.TopicMarket, 1_700_000_000)
	relay := &topicRelay{byTopic: map[string][]event.Event{event.TopicMarket: {offer}}}

	client := &Client{Dialer: relay.dial, Relays: []string{"wss://a"}, Now: func() time.Time {
		return time.Unix(1_700_000_100, 0)
	}}

	if _, err := client.Fetch(context.Background(), Filter{
		Kinds: []int{1}, Topics: []string{event.TopicMarket}, Limit: 5,
	}); err != nil {
		t.Fatal(err)
	}

	if len(relay.requests) != 1 || len(relay.requests[0]) != 1 {
		t.Fatalf("requests = %+v", relay.requests)
	}

	topics, ok := relay.requests[0][0]["#t"].([]any)

	if !ok || len(topics) != 1 || topics[0] != event.TopicMarket {
		t.Fatalf("filter = %+v, want a #t topic", relay.requests[0][0])
	}
}

// TestFetchManyAsksEveryFilterAtOnce: the three market questions travel in one
// subscription, and the answer is the union with duplicates removed.
func TestFetchManyAsksEveryFilterAtOnce(t *testing.T) {
	offer := signedEvent(t, "11", event.TopicMarket, 1_700_000_000)
	cancel := signedEvent(t, "22", event.TopicCancel, 1_700_000_100)
	match := signedEvent(t, "33", event.TopicMatch, 1_700_000_200)

	relay := &topicRelay{byTopic: map[string][]event.Event{
		event.TopicMarket: {offer},
		event.TopicCancel: {cancel, offer}, // the overlap must not double-count
		event.TopicMatch:  {match},
	}}

	client := &Client{Dialer: relay.dial, Relays: []string{"wss://a"}, Now: func() time.Time {
		return time.Unix(1_700_000_300, 0)
	}}

	events, err := client.FetchMany(context.Background(),
		Filter{Kinds: []int{1}, Topics: []string{event.TopicMarket}, Limit: 5},
		Filter{Kinds: []int{1}, Topics: []string{event.TopicCancel}, Limit: 20},
		Filter{Kinds: []int{1}, Topics: []string{event.TopicMatch}, Limit: 20},
	)

	if err != nil {
		t.Fatal(err)
	}

	if len(relay.requests) != 1 {
		t.Fatalf("connections = %d, want one subscription per relay", len(relay.requests))
	}

	if got := len(relay.requests[0]); got != 3 {
		t.Fatalf("filters per request = %d, want 3", got)
	}

	if len(events) != 3 {
		t.Fatalf("events = %d, want 3: %+v", len(events), events)
	}

	if events[0].ID != match.ID || events[1].ID != cancel.ID || events[2].ID != offer.ID {
		t.Fatalf("events are not newest first: %+v", events)
	}
}

// TestFetchManyRefusesAnEmptyFilterList: asking for nothing is a caller bug, not an
// empty marketplace.
func TestFetchManyRefusesAnEmptyFilterList(t *testing.T) {
	client := &Client{Relays: []string{"wss://a"}}

	if _, err := client.FetchMany(context.Background()); err == nil {
		t.Fatal("an empty filter list was accepted")
	}
}

// TestFilterIdsRendersAsIds: a fetch by id must reach the relay as `ids`, or the relay
// answers with everything it holds.
func TestFilterIdsRendersAsIds(t *testing.T) {
	offer := signedEvent(t, "11", event.TopicMarket, 1_700_000_000)
	relay := &topicRelay{byTopic: map[string][]event.Event{event.TopicMarket: {offer}}}

	client := &Client{Dialer: relay.dial, Relays: []string{"wss://a"}, Now: func() time.Time {
		return time.Unix(1_700_000_100, 0)
	}}

	// The fake relay answers by topic, so the assertion is on the filter that arrived.
	if _, err := client.Fetch(context.Background(), Filter{Kinds: []int{1}, Ids: []string{offer.ID}}); err != nil {
		t.Fatal(err)
	}

	if len(relay.requests) != 1 || len(relay.requests[0]) != 1 {
		t.Fatalf("requests = %+v", relay.requests)
	}

	ids, ok := relay.requests[0][0]["ids"].([]any)

	if !ok || len(ids) != 1 || ids[0] != offer.ID {
		t.Fatalf("filter = %+v, want an ids filter", relay.requests[0][0])
	}
}
