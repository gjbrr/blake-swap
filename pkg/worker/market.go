package worker

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/marketops"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/proto"
	"github.com/gjbrr/blake-swap/pkg/swap"
	"github.com/gjbrr/blake-swap/pkg/view"
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
	// engine builds the swap engine, which the accept path uses to create and publish the
	// swap a take or a match produces. It is a function rather than a field because the
	// engine is rebuilt per call from the vault, exactly as the engine domain does.
	engine func(ctx context.Context) (*swap.Engine, error)
	// now is the clock, for evidence-based expiry decisions.
	now func() time.Time
}

// newMarketDomain builds the market domain over a vault and a dialer.
func newMarketDomain(vault *vaultDomain, dialer nostr.Dialer) *marketDomain {
	return &marketDomain{vault: vault, dialer: dialer, now: time.Now}
}

// withEngine returns the domain with the engine builder the accept path needs.
func (d *marketDomain) withEngine(build func(context.Context) (*swap.Engine, error)) *marketDomain {
	d.engine = build

	return d
}

// Handle runs one market command.
func (d *marketDomain) Handle(ctx context.Context, request proto.Request) proto.Result {
	switch request.Command {
	case "market.relays":
		relays, err := d.relayList(ctx)

		if err != nil {
			return failed(request.Command, err)
		}

		return withView(proto.Result{
			Command: request.Command,
			OK:      true,
			Text:    "relays: " + strings.Join(relays, ", "),
		}, proto.MarketView{Relays: relays})

	case "market.set-relays":
		return d.setRelays(ctx, request)

	case "market.list":
		return d.list(ctx, request)

	case "market.publish":
		return d.publish(ctx, request)

	case "market.take":
		return d.take(ctx, request)

	case "market.takes":
		return d.takes(ctx, request)

	case "market.matches":
		return d.matches(ctx, request)

	case "market.accept-take":
		return d.acceptTake(ctx, request)

	case "market.accept-match":
		return d.acceptMatch(ctx, request)

	case "market.accept-reverse-match":
		return d.acceptReverseMatch(ctx, request)

	case "market.cancel":
		return d.cancel(ctx, request)
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

	read := "Nostr market read: " + strconv.Itoa(len(report.Events)) + " unique event(s) from " +
		strconv.Itoa(report.Answered) + "/" + strconv.Itoa(report.Relays) + " relay(s)"

	// The view says which offers are this participant's, which is what decides whether the
	// Market screen offers to take or to wait for a take.
	me := ""

	if key, err := d.identity(ctx); err == nil {
		me = key.Public
	}

	var mine []marketflow.Entry

	for _, entry := range entries {
		if me != "" && entry.Offer.Maker == me {
			mine = append(mine, entry)
		}
	}

	relays, _ := d.relayList(ctx)

	return withView(proto.Result{
		Command: request.Command,
		OK:      true,
		Text:    read + "\n" + renderBrowserMarket(open, d.now().Unix()),
	}, view.Market(read, relays, open, mine, nil, nil, nil, me))
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

	action := view.Action(request.Command, "", "publish-market-offer")
	action.Message = built.ID
	action.Sender = key.Public
	action.Details = []proto.ActionFact{
		{Label: "direction", Value: string(offer.Direction)},
		{Label: "BLAKE", Value: offer.BlakeAmount.Format()},
		{Label: "BTC sats", Value: strconv.FormatInt(offer.BTCSats, 10)},
		{Label: "expires", Value: strconv.FormatInt(offer.ExpiresAt, 10)},
	}

	// The text is the CLI's shape, which the terminal and the worker's own tests read; the
	// view is what the page renders.
	return withView(proto.Result{
		Command: request.Command,
		OK:      true,
		Text: "MARKET OFFER PUBLISHED\noffer id: " + built.ID +
			"\nmaker:    " + key.Public +
			"\ndirection: " + string(offer.Direction),
	}, action)
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

	action := view.Action(request.Command, "", "take-market-offer")
	action.Message = built.ID
	action.Sender = key.Public
	action.Details = []proto.ActionFact{
		{Label: "offer id", Value: offerID},
	}

	return withView(proto.Result{
		Command: request.Command,
		OK:      true,
		Text: "MARKET OFFER TAKEN\noffer id: " + offerID +
			"\ntake id:  " + built.ID +
			"\ntaker:    " + key.Public,
	}, action)
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
	return readRelayList(ctx, d.vault)
}

// readRelayList is the relay list every domain that talks to relays uses.
//
// The market and the settlement transport share it on purpose: a participant who
// configured where they read the market has configured where they coordinate, and two
// lists would be two answers to the same question.
func readRelayList(ctx context.Context, vault *vaultDomain) ([]string, error) {
	stored, err := vault.readSecret(ctx, SecretRelays)

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

// ------------------------------------------------------------------ accept path ---

// vaultFiles keeps the market's durable records in the vault.
//
// The selection records and match bindings are the receipts a negotiation rests on, so they
// live where every other durable fact lives: sealed, in the one store the client has. The
// names are the same ones the CLI uses, so an operator who moves between the two front ends
// is looking at the same documents.
type vaultFiles struct{ vault *vaultDomain }

// Read implements marketops.Files.
func (f vaultFiles) Read(ctx context.Context, name string) ([]byte, error) {
	value, err := f.vault.readSecret(ctx, marketFilePrefix+name)

	if err != nil {
		return nil, err
	}

	if value == "" {
		return nil, fs.ErrNotExist
	}

	return []byte(value), nil
}

// Write implements marketops.Files.
func (f vaultFiles) Write(ctx context.Context, name string, data []byte) error {
	return f.vault.writeSecret(ctx, marketFilePrefix+name, string(data))
}

// marketFilePrefix keeps the market's documents in their own corner of the vault.
const marketFilePrefix = "market/"

// engineFor builds the engine the accept path needs.
func (d *marketDomain) engineFor(ctx context.Context) (*swap.Engine, error) {
	if d.engine == nil {
		return nil, fmt.Errorf("this worker's market domain has no engine")
	}

	return d.engine(ctx)
}

// acceptEnv builds the environment the market operations run in.
func (d *marketDomain) acceptEnv(ctx context.Context) (*marketops.Env, *swap.Engine, error) {
	key, err := d.identity(ctx)

	if err != nil {
		return nil, nil, err
	}

	engine, err := d.engineFor(ctx)

	if err != nil {
		return nil, nil, err
	}

	client, err := d.client(ctx)

	if err != nil {
		return nil, nil, err
	}

	return &marketops.Env{
		Client:   client,
		Identity: key,
		Records:  d.vault.store,
		Files:    vaultFiles{vault: d.vault},
		Engine:   engine,
		Random:   randomReader{},
		Now:      d.now,
	}, engine, nil
}

// takes lists the valid takes on one of this participant's offers.
func (d *marketDomain) takes(ctx context.Context, request proto.Request) proto.Result {
	offerID := strings.ToLower(strings.TrimSpace(request.Args["offer_id"]))

	if len(offerID) != 64 {
		return proto.Result{
			Command: request.Command,
			Error:   "offer_id must be the full 64-character Nostr event ID",
		}
	}

	env, _, err := d.acceptEnv(ctx)

	if err != nil {
		return failed(request.Command, err)
	}

	set, err := marketops.ReadTakeSet(ctx, env, offerID, 200, nil, false, false)

	if err != nil {
		return failed(request.Command, err)
	}

	read := "Nostr take read: " + strconv.Itoa(set.Candidates) + " unique event(s) from " +
		strconv.Itoa(set.Answered) + "/" + strconv.Itoa(set.Relays) + " relay(s)"

	var lines []string

	for _, take := range set.Takes {
		lines = append(lines, take.EventID+"\t"+take.Taker+"\t"+
			take.Take.BlakeAmount.Format()+" BLAKE\t"+
			strconv.FormatInt(take.Take.BTCSats, 10)+" sats")
	}

	text := read

	if len(lines) == 0 {
		text += "\nNo valid takes."
	} else {
		text += "\n" + strings.Join(lines, "\n")
	}

	market := view.Market(read, nil, nil, nil, set.Takes, nil, nil, env.Identity.Public)

	return withView(proto.Result{Command: request.Command, OK: true, Text: text}, market)
}

// matches lists the verified matches that name this participant.
func (d *marketDomain) matches(ctx context.Context, request proto.Request) proto.Result {
	env, _, err := d.acceptEnv(ctx)

	if err != nil {
		return failed(request.Command, err)
	}

	evidence, err := marketops.MarketEvidence(ctx, env, 200)

	if err != nil {
		return failed(request.Command, err)
	}

	found := marketflow.MatchesFor(env.Identity.Public, evidence, d.now())

	var lines []string

	for _, match := range found {
		lines = append(lines, match.Match.EventID+"\tswap "+match.Match.SwapID)
	}

	text := strconv.Itoa(len(found)) + " match(es)"

	if len(lines) > 0 {
		text += "\n" + strings.Join(lines, "\n")
	}

	return withView(proto.Result{Command: request.Command, OK: true, Text: text},
		view.Market("", nil, nil, nil, nil, found, nil, env.Identity.Public))
}

// acceptTake turns a signed take into a swap, as the offer's maker.
func (d *marketDomain) acceptTake(ctx context.Context, request proto.Request) proto.Result {
	takeID := strings.ToLower(strings.TrimSpace(request.Args["take_id"]))

	if len(takeID) != 64 {
		return proto.Result{
			Command: request.Command,
			Error:   "take_id must be the full 64-character Nostr event ID",
		}
	}

	env, _, err := d.acceptEnv(ctx)

	if err != nil {
		return failed(request.Command, err)
	}

	acceptance, err := marketops.AcceptTake(ctx, env, takeID, nil, marketops.TakeHooks{})

	if reverse, ok := marketops.IsReverse(err); ok {
		return proto.Result{
			Command: request.Command,
			Error: "the reverse market direction is not implemented in this client yet " +
				"(take " + reverse.TakeID + "); the CLI can accept it",
		}
	}

	if err != nil {
		return failed(request.Command, err)
	}

	action := view.Action(request.Command, acceptance.SwapID, "accept-market-take")
	action.Message = acceptance.MatchID
	action.Details = []proto.ActionFact{
		{Label: "offer id", Value: acceptance.OfferID},
		{Label: "take id", Value: acceptance.TakeID},
		{Label: "match id", Value: acceptance.MatchID},
		{Label: "BLAKE", Value: acceptance.Blake.Format()},
		{Label: "BTC sats", Value: strconv.FormatInt(acceptance.BTCSats, 10)},
	}

	if acceptance.Repeated {
		action.Details = append(action.Details, proto.ActionFact{
			Label: "note", Value: "this take had already been accepted; the existing swap was reused",
		})
	}

	return withView(proto.Result{
		Command: request.Command,
		OK:      true,
		Text: "accepted take " + acceptance.TakeID + " as swap " + acceptance.SwapID +
			" (match " + acceptance.MatchID + ")",
	}, action)
}

// acceptMatch imports the swap a market match binds, as the taker.
func (d *marketDomain) acceptMatch(ctx context.Context, request proto.Request) proto.Result {
	matchID := strings.ToLower(strings.TrimSpace(request.Args["match_id"]))

	if len(matchID) != 64 {
		return proto.Result{
			Command: request.Command,
			Error:   "match_id must be the full 64-character Nostr event ID",
		}
	}

	env, _, err := d.acceptEnv(ctx)

	if err != nil {
		return failed(request.Command, err)
	}

	imported, err := marketops.AcceptMatch(ctx, env, matchID, nil, marketops.MatchHooks{})

	if err != nil {
		return failed(request.Command, err)
	}

	action := view.Action(request.Command, imported.SwapID, "accept-market-match")
	action.Message = imported.TakeID
	action.Details = []proto.ActionFact{
		{Label: "match id", Value: imported.MatchID},
		{Label: "offer id", Value: imported.OfferID},
		{Label: "take id", Value: imported.TakeID},
		{Label: "maker", Value: imported.Maker},
		{Label: "BLAKE", Value: imported.Blake.Format()},
		{Label: "BTC sats", Value: strconv.FormatInt(imported.BTCSats, 10)},
	}

	if imported.Repeated {
		action.Details = append(action.Details, proto.ActionFact{
			Label: "note", Value: "this match had already been imported; the existing swap was reused",
		})
	}

	return withView(proto.Result{
		Command: request.Command,
		OK:      true,
		Text:    "imported match " + imported.MatchID + " as swap " + imported.SwapID,
	}, action)
}

// acceptReverseMatch imports a btc-for-blake match as its taker.
//
// In this direction the maker is protocol Bob and the taker is protocol Alice, so this is
// where the taker creates the secret against deadlines it derived itself — which is why it
// is a separate command rather than a flag on the forward one.
func (d *marketDomain) acceptReverseMatch(ctx context.Context, request proto.Request) proto.Result {
	matchID := strings.ToLower(strings.TrimSpace(request.Args["match_id"]))

	if len(matchID) != 64 {
		return proto.Result{
			Command: request.Command,
			Error:   "match_id must be the full 64-character Nostr event ID",
		}
	}

	env, _, err := d.acceptEnv(ctx)

	if err != nil {
		return failed(request.Command, err)
	}

	imported, err := marketops.ImportReverseMatch(ctx, env, matchID, nil, marketops.MatchHooks{})

	if err != nil {
		return failed(request.Command, err)
	}

	action := view.Action(request.Command, imported.SwapID, "accept-reverse-market-match")
	action.Message = imported.MatchID
	action.Details = []proto.ActionFact{
		{Label: "match id", Value: imported.MatchID},
		{Label: "maker", Value: imported.Maker},
		{Label: "taker", Value: imported.Taker},
		{Label: "BLAKE", Value: imported.Blake.Format()},
		{Label: "BTC sats", Value: strconv.FormatInt(imported.BTCSats, 10)},
		{Label: "direction", Value: "btc-for-blake"},
	}

	if imported.Repeated {
		action.Details = append(action.Details, proto.ActionFact{
			Label: "note", Value: "this match had already been imported; the existing swap was reused",
		})
	}

	return withView(proto.Result{
		Command: request.Command,
		OK:      true,
		Text:    "imported reverse match " + imported.MatchID + " as swap " + imported.SwapID,
	}, action)
}

// cancel publishes this participant's cancellation of an offer they made.
func (d *marketDomain) cancel(ctx context.Context, request proto.Request) proto.Result {
	offerID := strings.ToLower(strings.TrimSpace(request.Args["offer_id"]))

	if len(offerID) != 64 {
		return proto.Result{
			Command: request.Command,
			Error:   "offer_id must be the full 64-character Nostr event ID",
		}
	}

	key, err := d.identity(ctx)

	if err != nil {
		return failed(request.Command, err)
	}

	client, err := d.client(ctx)

	if err != nil {
		return failed(request.Command, err)
	}

	env := &marketops.Env{Client: client, Identity: key, Now: d.now}

	offer, err := marketops.FetchEvent(ctx, env, offerID)

	if err != nil {
		return failed(request.Command, err)
	}

	if offer.Pubkey != key.Public {
		return proto.Result{
			Command: request.Command,
			Error:   "SECURITY FAILURE: current Nostr identity is not the maker of this offer",
		}
	}

	built, _, err := marketflow.BuildCancel(key, marketflow.BuildCancelOptions{OfferID: offerID})

	if err != nil {
		return failed(request.Command, err)
	}

	report, err := client.Publish(ctx, built)

	if err != nil {
		return failed(request.Command, err)
	}

	action := view.Action(request.Command, "", "cancel-market-offer")
	action.Message = built.ID
	action.Sender = key.Public
	action.Details = []proto.ActionFact{
		{Label: "offer id", Value: offerID},
		{Label: "relays accepted", Value: strconv.Itoa(report.Accepted) + "/" + strconv.Itoa(len(report.Relays))},
	}

	return withView(proto.Result{
		Command: request.Command,
		OK:      true,
		Text:    "cancelled offer " + offerID + " (event " + built.ID + ")",
	}, action)
}
