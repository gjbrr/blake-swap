package worker

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/proto"
)

// SecretRelays is the vault value holding the market's relay list.
//
// Relays are not secret, but they live beside the identity because they are this
// participant's configuration: the browser client is the product, and where it reads the
// market from is a choice the person using it makes, not one the build makes for them.
const SecretRelays = "relays"

// DefaultRelays is the market's fallback when nothing is configured.
//
// One public relay is a weak default on purpose: it is enough to see a market, and a client
// that shipped a longer list would be quietly choosing a network for its user.
var DefaultRelays = []string{"wss://nos.lol"}

// marketDomain is the browser's market: reading it, and publishing into it.
//
// It is the same code the CLI's market commands run — the same builders, the same
// validators, the same evidence-based state derivation — with two differences that matter in
// a browser: the connection is a `platform.WebSocket` rather than a host socket, and the
// signing identity comes from the unlocked vault rather than from a key file.
type marketDomain struct {
	vault *vaultDomain
	// dialer is the platform seam. It is injectable so the whole domain can be tested
	// against a relay stub without a browser.
	dialer nostr.Dialer
	// now is the clock, for evidence-based expiry decisions.
	now func() time.Time
}

// newMarketDomain builds the market domain over a vault and a dialer.
func newMarketDomain(vault *vaultDomain, dialer nostr.Dialer) *marketDomain {
	return &marketDomain{vault: vault, dialer: dialer, now: time.Now}
}

// Handle runs one market command.
func (d *marketDomain) Handle(ctx context.Context, request proto.Request) proto.Result {
	switch request.Command {
	case "market.relays":
		relays, err := d.relayList(ctx)

		if err != nil {
			return failed(request.Command, err)
		}

		return proto.Result{
			Command: request.Command,
			OK:      true,
			Text:    "relays: " + strings.Join(relays, ", "),
		}

	case "market.set-relays":
		return d.setRelays(ctx, request)

	case "market.list":
		return d.list(ctx, request)

	case "market.publish":
		return d.publish(ctx, request)

	case "market.take":
		return d.take(ctx, request)
	}

	return proto.Result{
		Command: request.Command,
		Error:   "unsupported command: " + request.Command,
	}
}

// setRelays stores the participant's relay list.
func (d *marketDomain) setRelays(ctx context.Context, request proto.Request) proto.Result {
	relays := splitRelays(request.Args["relays"])

	if len(relays) == 0 {
		return proto.Result{Command: request.Command, Error: "at least one relay is required"}
	}

	for _, relay := range relays {
		if !strings.HasPrefix(relay, "ws://") && !strings.HasPrefix(relay, "wss://") {
			return proto.Result{
				Command: request.Command,
				Error:   "a relay must be a ws:// or wss:// URL: " + relay,
			}
		}
	}

	if err := d.vault.writeSecret(ctx, SecretRelays, strings.Join(relays, ",")); err != nil {
		return failed(request.Command, err)
	}

	return proto.Result{
		Command: request.Command,
		OK:      true,
		Text:    "relays set: " + strings.Join(relays, ", "),
	}
}

// list reads the market and renders the offers that are open now.
func (d *marketDomain) list(ctx context.Context, request proto.Request) proto.Result {
	client, err := d.client(ctx)

	if err != nil {
		return failed(request.Command, err)
	}

	report, err := client.FetchReport(ctx, marketflow.MarketFilters(50)...)

	if err != nil {
		return failed(request.Command, err)
	}

	for _, seen := range report.Events {
		// Cache what verified: a relay's retention is not the protocol's history, and a
		// market read that forgets is a market read that invents cancellations.
		_ = client.Cache.Put(ctx, seen)
	}

	events := append([]event.Event{}, report.Events...)
	events = append(events, cachedEvents(ctx, client.Cache)...)

	entries, err := marketflow.Book(events, d.now())

	if err != nil {
		return failed(request.Command, err)
	}

	open := marketflow.OpenOffers(entries)

	return proto.Result{
		Command: request.Command,
		OK:      true,
		Text: "Nostr market read: " + strconv.Itoa(len(report.Events)) + " unique event(s) from " +
			strconv.Itoa(report.Answered) + "/" + strconv.Itoa(report.Relays) + " relay(s)\n" +
			renderBrowserMarket(open, d.now().Unix()),
	}
}

// publish advertises one offer signed by this participant.
func (d *marketDomain) publish(ctx context.Context, request proto.Request) proto.Result {
	key, err := d.identity(ctx)

	if err != nil {
		return failed(request.Command, err)
	}

	blake, err := amount.ParseBlake(request.Args["blake"])

	if err != nil {
		return proto.Result{Command: request.Command, Error: "BLAKE amount: " + err.Error()}
	}

	// The market's sats argument is a whole number of satoshis, exactly as the CLI's
	// --sats is. Parsing it as a coin value would multiply it by a hundred million and
	// advertise a trade nobody asked for.
	sats, err := strconv.ParseInt(strings.TrimSpace(request.Args["sats"]), 10, 64)

	if err != nil || sats <= 0 {
		return proto.Result{
			Command: request.Command,
			Error:   "BTC amount: must be a positive whole number of satoshis",
		}
	}

	minutes := int64(60)

	if raw := strings.TrimSpace(request.Args["minutes"]); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)

		if err != nil || parsed <= 0 {
			return proto.Result{Command: request.Command, Error: "offer lifetime must be positive"}
		}

		minutes = parsed
	}

	direction := market.BlakeForBTC

	if raw := strings.TrimSpace(request.Args["direction"]); raw != "" {
		parsed, err := market.ParseDirection(raw)

		if err != nil {
			return proto.Result{Command: request.Command, Error: err.Error()}
		}

		direction = parsed
	}

	built, offer, err := marketflow.BuildOffer(key, marketflow.BuildOfferOptions{
		BlakeAmount: blake,
		BTCSats:     sats,
		Minutes:     minutes,
		Direction:   direction,
		CreatedAt:   d.now().Unix(),
	})

	if err != nil {
		return failed(request.Command, err)
	}

	client, err := d.client(ctx)

	if err != nil {
		return failed(request.Command, err)
	}

	if _, err := client.Publish(ctx, built); err != nil {
		return failed(request.Command, err)
	}

	return proto.Result{
		Command: request.Command,
		OK:      true,
		Text: "MARKET OFFER PUBLISHED\noffer id: " + built.ID +
			"\nmaker:    " + key.Public +
			"\ndirection: " + string(offer.Direction),
	}
}

// take publishes this participant's take of one open offer.
func (d *marketDomain) take(ctx context.Context, request proto.Request) proto.Result {
	key, err := d.identity(ctx)

	if err != nil {
		return failed(request.Command, err)
	}

	offerID := strings.ToLower(strings.TrimSpace(request.Args["offer_id"]))

	if len(offerID) != 64 {
		return proto.Result{
			Command: request.Command,
			Error:   "offer_id must be the full 64-character Nostr event ID",
		}
	}

	client, err := d.client(ctx)

	if err != nil {
		return failed(request.Command, err)
	}

	report, err := client.FetchReport(ctx, marketflow.MarketFilters(50)...)

	if err != nil {
		return failed(request.Command, err)
	}

	events := append([]event.Event{}, report.Events...)
	events = append(events, cachedEvents(ctx, client.Cache)...)

	entries, err := marketflow.Book(events, d.now())

	if err != nil {
		return failed(request.Command, err)
	}

	var selected *marketflow.Entry

	for index := range entries {
		if entries[index].EventID() == offerID {
			selected = &entries[index]

			break
		}
	}

	if selected == nil || !selected.Open() {
		return proto.Result{
			Command: request.Command,
			Error:   "Offer is not currently open. It may be expired, cancelled, invalid, or unavailable from this relay.",
		}
	}

	built, _, err := marketflow.BuildTake(key, marketflow.BuildTakeOptions{
		Offer: selected.Offer, CreatedAt: d.now().Unix(),
	})

	if err != nil {
		return failed(request.Command, err)
	}

	if _, err := client.Publish(ctx, built); err != nil {
		return failed(request.Command, err)
	}

	return proto.Result{
		Command: request.Command,
		OK:      true,
		Text: "MARKET OFFER TAKEN\noffer id: " + offerID +
			"\ntake id:  " + built.ID +
			"\ntaker:    " + key.Public,
	}
}

// client builds a relay client from the configured relays.
func (d *marketDomain) client(ctx context.Context) (*nostr.Client, error) {
	relays, err := d.relayList(ctx)

	if err != nil {
		return nil, err
	}

	return &nostr.Client{
		Dialer: d.dialer,
		Relays: relays,
		Cache:  nostr.NewMemoryCache(),
	}, nil
}

// relayList is the configured relay list, or the default.
func (d *marketDomain) relayList(ctx context.Context) ([]string, error) {
	stored, err := d.vault.readSecret(ctx, SecretRelays)

	if err != nil {
		return nil, err
	}

	relays := splitRelays(stored)

	if len(relays) == 0 {
		return DefaultRelays, nil
	}

	return relays, nil
}

// identity is this participant's signing key, from the unlocked vault.
func (d *marketDomain) identity(ctx context.Context) (*nostr.Key, error) {
	secret, err := d.vault.readSecret(ctx, SecretIdentity)

	if err != nil {
		return nil, err
	}

	if secret == "" {
		return nil, errMissingIdentity
	}

	return nostr.NewKey(secret)
}

// errMissingIdentity is what a publish without a vault identity reports.
var errMissingIdentity = fmt.Errorf("set an identity before publishing to the market")

// splitRelays parses a comma or space separated relay list.
func splitRelays(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t'
	})

	relays := make([]string, 0, len(fields))

	for _, field := range fields {
		trimmed := strings.TrimSpace(field)

		if trimmed != "" {
			relays = append(relays, trimmed)
		}
	}

	return relays
}

// cachedEvents reads every cached event, which is the same merge the CLI does.
func cachedEvents(ctx context.Context, cache nostr.Cache) []event.Event {
	ids, err := cache.IDs(ctx)

	if err != nil {
		return nil
	}

	events := make([]event.Event, 0, len(ids))

	for _, id := range ids {
		stored, ok, err := cache.Get(ctx, id)

		if err != nil || !ok {
			continue
		}

		events = append(events, stored)
	}

	return events
}

// renderBrowserMarket is the market view, in the CLI's shape.
//
// The browser is not a different market: the same columns and the same wording mean an
// operator who has used one client can read the other.
func renderBrowserMarket(offers []marketflow.Entry, now int64) string {
	if len(offers) == 0 {
		return "\nNo valid open offers found.\n"
	}

	sorted := append([]marketflow.Entry{}, offers...)

	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Offer.CreatedAt != sorted[j].Offer.CreatedAt {
			return sorted[i].Offer.CreatedAt > sorted[j].Offer.CreatedAt
		}

		return sorted[i].EventID() > sorted[j].EventID()
	})

	var out strings.Builder

	out.WriteString("\nAVAILABLE BLAKE/BTC OFFERS\n")
	out.WriteString("==========================\n\n")

	for _, entry := range sorted {
		out.WriteString(entry.EventID()[:12] + "  " + entry.Offer.Maker[:12] + "  " +
			entry.Offer.BlakeAmount.Format() + " BLAKE  " +
			strconv.FormatInt(entry.Offer.BTCSats, 10) + " sats\n")
	}

	out.WriteString("\n" + strconv.Itoa(len(sorted)) + " open offer(s)\n")

	return out.String()
}
