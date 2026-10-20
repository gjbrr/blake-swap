//go:build !js

package marketops

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/coord"
	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/platform"
	"github.com/gjbrr/blake-swap/pkg/relaystub"
	"github.com/gjbrr/blake-swap/pkg/store"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// handshakeEngine is a scripted engine for the reverse maker's import.
//
// It writes the record AcceptOffer would have written and records what it was asked for, and
// it can be told to stop at the point the real engine becomes vulnerable: the record durable,
// the accept message never sent.
type handshakeEngine struct {
	records store.Store
	// crashBeforeSend leaves the record with its keys and without Bob's identity, which is
	// exactly the state a process death between the two leaves.
	crashBeforeSend bool
	// sent is every accept the engine claimed to send.
	sent []string
	// transport records the messages the resume path sends.
	transport *recordingTransport
}

func (e *handshakeEngine) New(context.Context, swap.NewRequest) (*swap.NewResult, error) {
	return nil, errors.New("this fixture only accepts offers")
}

func (e *handshakeEngine) PublishOffer(context.Context, string) (*swap.MessageResult, error) {
	return nil, errors.New("this fixture only accepts offers")
}

func (e *handshakeEngine) AcceptOffer(
	_ context.Context, req swap.AcceptRequest,
) (*swap.AcceptResult, error) {
	if req.ExpectedAlicePub == "" {
		return nil, errors.New("the maker must be pinned")
	}

	sats := int64(0)

	if req.ExpectedBTCSats != nil {
		sats = *req.ExpectedBTCSats
	}

	base := amount.Blake(0)

	if req.ExpectedBlakeAmount != nil {
		base = *req.ExpectedBlakeAmount
	}

	alice, bobPub := req.ExpectedAlicePub, strings.Repeat("77", 32)

	record := &model.Swap{
		SwapID:               req.SwapID,
		State:                "OFFERED",
		SecretHash:           strings.Repeat("00", 32),
		BlakeAmountBaseUnits: int64(base),
		BTCSats:              sats,
		NostrAlicePub:        &alice,
		BlakeBobPub:          &bobPub,
		BTCBobPub:            &bobPub,
	}

	if !e.crashBeforeSend {
		record.NostrBobPub = &bobPub
		e.sent = append(e.sent, req.SwapID)
	}

	if err := e.records.Save(context.Background(), store.Bob, record); err != nil {
		return nil, err
	}

	return &swap.AcceptResult{Swap: record, Location: "accept-" + req.SwapID}, nil
}

func (e *handshakeEngine) Coordination() (coord.Transport, nostr.Mode) {
	if e.transport == nil {
		e.transport = &recordingTransport{}
	}

	return e.transport, nostr.ModeNIP44
}

// recordingTransport records what the recovery path sends, and where.
type recordingTransport struct {
	messages []map[string]any
	keys     []string
}

func (t *recordingTransport) Send(
	_ context.Context, swapID, kind string, body map[string]any, options coord.SendOptions,
) (string, error) {
	if kind != "accept" {
		return "", errors.New("unexpected message kind: " + kind)
	}

	if options.PeerPubkey == "" {
		return "", errors.New("the peer must be pinned")
	}

	t.messages = append(t.messages, body)
	t.keys = append(t.keys, swapID)

	return "spool/" + swapID + ".accept.json", nil
}

func (t *recordingTransport) Receive(
	context.Context, string, string, coord.ReceiveOptions,
) (*coord.Message, error) {
	return nil, errors.New("the reverse offer import receives nothing")
}

// reverseOfferFixture is one reverse negotiation with a valid signed match.
type reverseOfferFixture struct {
	env       *Env
	engine    *handshakeEngine
	bob, hers *nostr.Key
	match     event.Event
	matchID   string
	swapID    string
}

// newReverseOfferFixture builds the market evidence for a reverse match.
func newReverseOfferFixture(t *testing.T) *reverseOfferFixture {
	t.Helper()

	stub, err := relaystub.New()

	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(stub.Close)

	bob, err := nostr.NewKey(strings.Repeat("11", 32))

	if err != nil {
		t.Fatal(err)
	}

	alice, err := nostr.NewKey(strings.Repeat("22", 32))

	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().Unix()

	offerEvent, _, err := marketflow.BuildOffer(bob, marketflow.BuildOfferOptions{
		BlakeAmount: amount.MustParseBlake("10"),
		BTCSats:     50_000,
		Minutes:     30,
		Direction:   market.BTCForBlake,
		CreatedAt:   now,
	})

	if err != nil {
		t.Fatal(err)
	}

	listed, err := marketflow.ParseOffer(offerEvent, time.Now())

	if err != nil {
		t.Fatal(err)
	}

	takeEvent, take, err := marketflow.BuildTake(alice, marketflow.BuildTakeOptions{
		Offer: *listed, CreatedAt: now,
	})

	if err != nil {
		t.Fatal(err)
	}

	swapID := "aaaa00000001"

	built, _, err := marketflow.BuildMatch(bob, marketflow.BuildMatchOptions{
		Offer:       *listed,
		OfferEvent:  offerEvent,
		TakeEvent:   takeEvent,
		Taker:       &take,
		SwapID:      swapID,
		BlakeAmount: listed.BlakeAmount,
		BTCSats:     listed.BTCSats,
		CreatedAt:   now,
	})

	if err != nil {
		t.Fatal(err)
	}

	for _, published := range []event.Event{offerEvent, takeEvent, built} {
		stub.Inject(published)
	}

	records := store.NewMemory()

	engine := &handshakeEngine{records: records}

	env := &Env{
		Client: &nostr.Client{
			Dialer: func(ctx context.Context, url string) (nostr.Conn, error) {
				return platform.DialWebSocket(ctx, url)
			},
			Relays: []string{stub.URL},
			Cache:  nostr.NewMemoryCache(),
		},
		Identity: bob,
		Records:  records,
		Files:    memoryFiles{},
		Engine:   engine,
		Random:   strings.NewReader(strings.Repeat("a", 64)),
		Now:      time.Now,
	}

	return &reverseOfferFixture{
		env: env, engine: engine, bob: bob, hers: alice,
		match: built, matchID: built.ID, swapID: swapID,
	}
}

// TestAcceptReverseOfferImportsTheTakersOffer: the maker's second step.
func TestAcceptReverseOfferImportsTheTakersOffer(t *testing.T) {
	fixture := newReverseOfferFixture(t)

	imported, err := AcceptReverseOffer(
		context.Background(), fixture.env, fixture.matchID, &fixture.match, MatchHooks{},
	)

	if err != nil {
		t.Fatalf("importing the reverse offer: %v", err)
	}

	if imported.SwapID != fixture.swapID {
		t.Fatalf("swap id = %q", imported.SwapID)
	}

	if imported.Alice != fixture.hers.Public || imported.Bob != fixture.bob.Public {
		t.Fatalf("identities = %q/%q", imported.Alice, imported.Bob)
	}

	if imported.Direction != market.BTCForBlake {
		t.Fatalf("direction = %q", imported.Direction)
	}

	if imported.Repeated || imported.Recovered {
		t.Fatalf("a fresh import reported %+v", imported)
	}

	if len(fixture.engine.sent) != 1 {
		t.Fatalf("the engine sent %d accepts, want 1", len(fixture.engine.sent))
	}

	record, err := fixture.env.Records.Load(context.Background(), fixture.swapID)

	if err != nil {
		t.Fatal(err)
	}

	if record.SecretHex != nil {
		t.Fatal("the maker holds a secret it must not have")
	}

	if record.NostrBobPub == nil || *record.NostrBobPub != fixture.bob.Public {
		t.Fatal("Bob's authenticated identity was not pinned")
	}
}

// TestAcceptReverseOfferRepeatsWithoutNewKeys: a second import is a success, not new keys.
func TestAcceptReverseOfferRepeatsWithoutNewKeys(t *testing.T) {
	fixture := newReverseOfferFixture(t)
	ctx := context.Background()

	if _, err := AcceptReverseOffer(ctx, fixture.env, fixture.matchID, &fixture.match, MatchHooks{}); err != nil {
		t.Fatal(err)
	}

	before, err := fixture.env.Records.Load(ctx, fixture.swapID)

	if err != nil {
		t.Fatal(err)
	}

	again, err := AcceptReverseOffer(ctx, fixture.env, fixture.matchID, &fixture.match, MatchHooks{})

	if err != nil {
		t.Fatalf("the repeat was refused: %v", err)
	}

	if !again.Repeated {
		t.Fatal("the repeat did not report itself")
	}

	if len(fixture.engine.sent) != 1 {
		t.Fatalf("the repeat sent %d accepts, want 1", len(fixture.engine.sent))
	}

	after, err := fixture.env.Records.Load(ctx, fixture.swapID)

	if err != nil {
		t.Fatal(err)
	}

	if *before.BlakeBobPub != *after.BlakeBobPub || *before.BTCBobPub != *after.BTCBobPub {
		t.Fatal("the repeat replaced Bob's settlement keys")
	}
}

// seedPartial writes the record a process death between Bob's keys being durable and his
// accept message being sent leaves behind: the settlement keys exist, his authenticated
// identity does not.
//
// The current engine saves the identity with the keys, so this shape comes from a record an
// earlier build or another front end wrote. That is exactly what the guard is for.
func seedPartial(t *testing.T, fixture *reverseOfferFixture) *model.Swap {
	t.Helper()

	alice, bobKey := fixture.hers.Public, strings.Repeat("77", 32)

	record := &model.Swap{
		SwapID:               fixture.swapID,
		State:                "OFFERED",
		SecretHash:           strings.Repeat("00", 32),
		BlakeAmountBaseUnits: int64(amount.MustParseBlake("10")),
		BTCSats:              50_000,
		NostrAlicePub:        &alice,
		BlakeBobPub:          &bobKey,
		BTCBobPub:            &bobKey,
	}

	if err := fixture.env.Records.Save(context.Background(), store.Bob, record); err != nil {
		t.Fatal(err)
	}

	return record
}

// TestAcceptReverseOfferResumesTheCrashWindow: the keys are reused and exactly one accept is
// sent, which is the whole point of the durable-before-send ordering.
func TestAcceptReverseOfferResumesTheCrashWindow(t *testing.T) {
	fixture := newReverseOfferFixture(t)
	ctx := context.Background()

	partial := seedPartial(t, fixture)

	recovered, err := AcceptReverseOffer(ctx, fixture.env, fixture.matchID, &fixture.match, MatchHooks{})

	if err != nil {
		t.Fatalf("the resume was refused: %v", err)
	}

	if !recovered.Recovered {
		t.Fatal("the resume did not report itself as a recovery")
	}

	if len(fixture.engine.sent) != 0 {
		t.Fatalf("the resume created new keys: %d accepts", len(fixture.engine.sent))
	}

	if fixture.engine.transport == nil || len(fixture.engine.transport.messages) != 1 {
		t.Fatalf("the resume sent %d accept messages, want 1", len(fixture.engine.transport.messages))
	}

	sent := fixture.engine.transport.messages[0]

	if sent["blake_bob_pub"] != *partial.BlakeBobPub {
		t.Fatal("the resume sent different BLAKE settlement keys")
	}

	if sent["btc_bob_pub"] != *partial.BTCBobPub {
		t.Fatal("the resume sent different BTC settlement keys")
	}

	if sent["secret_hash"] != partial.SecretHash {
		t.Fatal("the resume sent a different hashlock")
	}

	resumed, err := fixture.env.Records.Load(ctx, fixture.swapID)

	if err != nil {
		t.Fatal(err)
	}

	if resumed.NostrBobPub == nil || *resumed.NostrBobPub != fixture.bob.Public {
		t.Fatal("the resume did not pin Bob's identity")
	}

	// And a third run is a plain repeat: one accept, ever.
	third, err := AcceptReverseOffer(ctx, fixture.env, fixture.matchID, &fixture.match, MatchHooks{})

	if err != nil {
		t.Fatal(err)
	}

	if !third.Repeated {
		t.Fatal("the run after a recovery was not a repeat")
	}

	if len(fixture.engine.transport.messages) != 1 {
		t.Fatalf("a second accept was sent: %d", len(fixture.engine.transport.messages))
	}
}

// TestAcceptReverseOfferRefusesAnUnsafeResume: money that has moved cannot be re-handshaked.
func TestAcceptReverseOfferRefusesAnUnsafeResume(t *testing.T) {
	for name, mutate := range map[string]func(*model.Swap){
		"funding": func(s *model.Swap) {
			txid := strings.Repeat("88", 32)
			s.BTCFundingTxID = &txid
		},
		"claim": func(s *model.Swap) {
			txid := strings.Repeat("88", 32)
			s.BlakeClaimTxID = &txid
		},
		"refund": func(s *model.Swap) {
			txid := strings.Repeat("88", 32)
			s.BTCRefundTxID = &txid
		},
		"secret": func(s *model.Swap) {
			secretHex, secretHash, err := model.NewSecret(strings.NewReader(
				strings.Repeat("b", 32),
			))

			if err != nil {
				t.Fatal(err)
			}

			s.SecretHex = &secretHex
			s.SecretHash = secretHash
		},
		"missing keys": func(s *model.Swap) { s.BlakeBobPub = nil },
		"state":        func(s *model.Swap) { s.State = "BLAKE_FUNDED" },
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newReverseOfferFixture(t)
			ctx := context.Background()

			partial := seedPartial(t, fixture)
			mutate(partial)

			if err := fixture.env.Records.Save(ctx, store.Bob, partial); err != nil {
				t.Fatal(err)
			}

			if _, err := AcceptReverseOffer(ctx, fixture.env, fixture.matchID, &fixture.match, MatchHooks{}); err == nil {
				t.Fatalf("%s was resumable", name)
			}

			if fixture.engine.transport != nil && len(fixture.engine.transport.messages) != 0 {
				t.Fatalf("%s sent an accept", name)
			}
		})
	}
}

// TestAcceptReverseOfferRefusesTheWrongDirectionOrIdentity: only the reverse maker's own
// btc-for-blake match may be imported here.
func TestAcceptReverseOfferRefusesTheWrongDirectionOrIdentity(t *testing.T) {
	t.Run("other signer", func(t *testing.T) {
		fixture := newReverseOfferFixture(t)
		fixture.env.Identity = fixture.hers

		_, err := AcceptReverseOffer(
			context.Background(), fixture.env, fixture.matchID, &fixture.match, MatchHooks{},
		)

		if err == nil || !strings.Contains(err.Error(), "did not sign this market match") {
			t.Fatalf("another participant's match was importable: %v", err)
		}
	})

	t.Run("other match", func(t *testing.T) {
		fixture := newReverseOfferFixture(t)

		// A match whose terms are not this negotiation's: re-signed by Bob over a different
		// take, so the swap id differs from the one the fixture reserved.
		before := fixture.match
		before.Content = strings.Replace(before.Content, `"swap_id":"`+fixture.swapID,
			`"swap_id":"bbbb00000002`, 1)

		_, err := AcceptReverseOffer(
			context.Background(), fixture.env, fixture.matchID, &before, MatchHooks{},
		)

		if err == nil {
			t.Fatal("a tampered match was imported")
		}
	})
}
