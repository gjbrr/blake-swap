//go:build !js

package marketops

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/store"
)

// ignoreFixture is the reverse negotiation seen by its taker: ignoring is the taker's act, so
// the local identity is the one the match addresses.
func ignoreFixture(t *testing.T) *reverseOfferFixture {
	t.Helper()

	fixture := newReverseOfferFixture(t)
	fixture.env.Identity = fixture.hers

	return fixture
}

// TestIgnoreMatchWritesTheTombstoneTheReferenceWrites pins the document shape, which is the
// only thing that makes the marker portable between the two clients.
func TestIgnoreMatchWritesTheTombstoneTheReferenceWrites(t *testing.T) {
	fixture := ignoreFixture(t)
	ctx := context.Background()

	ignored, err := IgnoreMatch(ctx, fixture.env, fixture.matchID, 0)

	if err != nil {
		t.Fatalf("ignoring the match: %v", err)
	}

	if ignored.MatchID != fixture.matchID || ignored.SwapID != fixture.swapID {
		t.Fatalf("ignored = %+v", ignored)
	}

	if ignored.Repeated || ignored.AlreadyIgnored {
		t.Fatalf("a fresh tombstone reported %+v", ignored)
	}

	raw, ok := fixture.env.Files.(memoryFiles)[tombstoneName(fixture.matchID)]

	if !ok {
		t.Fatal("no tombstone was written")
	}

	for _, want := range []string{
		`"match_id": "` + fixture.matchID + `"`,
		`"swap_id": "` + fixture.swapID + `"`,
		`"offer_id"`, `"take_id"`, `"maker"`, `"taker"`,
		`"direction": "btc-for-blake"`,
		`"blake_amount": 10`,
		`"btc_sats": 50000`,
		`"ignored_at":`,
		`"local_only": true`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("the tombstone is missing %q:\n%s", want, raw)
		}
	}

	// The ignore sent nothing and created nothing.
	for _, published := range fixture.stub.Events() {
		if published.ID == fixture.matchID {
			continue
		}

		if published.HasTag("t", "ignored") {
			t.Fatal("ignoring a match published an event")
		}
	}

	if _, err := fixture.env.Records.Load(ctx, fixture.swapID); err == nil {
		t.Fatal("ignoring a match created swap state")
	}

	// A second ignore is a success that says so.
	again, err := IgnoreMatch(ctx, fixture.env, fixture.matchID, 0)

	if err != nil {
		t.Fatalf("the repeat was refused: %v", err)
	}

	if !again.AlreadyIgnored || !again.Repeated {
		t.Fatalf("the repeat reported %+v", again)
	}
}

// TestIgnoreMatchRefusesAPrefixThatIsShortOrUnknown: the prefix has to name something, and it
// has to name something unambiguously.
func TestIgnoreMatchRefusesAPrefixThatIsShortOrUnknown(t *testing.T) {
	fixture := ignoreFixture(t)
	ctx := context.Background()

	if _, err := IgnoreMatch(ctx, fixture.env, "abc", 0); err == nil ||
		!strings.Contains(err.Error(), "at least 8 characters") {
		t.Fatalf("a short prefix was accepted: %v", err)
	}

	if _, err := IgnoreMatch(ctx, fixture.env, strings.Repeat("99", 32), 0); err == nil ||
		!strings.Contains(err.Error(), "No valid market match begins with") {
		t.Fatalf("an unknown prefix was accepted: %v", err)
	}

	// A prefix of the right length resolves the one match.
	if _, err := IgnoreMatch(ctx, fixture.env, strings.ToUpper(fixture.matchID[:8]), 0); err != nil {
		t.Fatalf("a unique prefix was refused: %v", err)
	}
}

// TestIgnoreMatchRefusesATradeInProgress: a match with local swap state is being negotiated,
// and hiding it would hide the trade.
func TestIgnoreMatchRefusesATradeInProgress(t *testing.T) {
	fixture := ignoreFixture(t)

	alice := fixture.hers.Public

	if err := fixture.env.Records.Save(context.Background(), store.Alice, &model.Swap{
		SwapID:               fixture.swapID,
		State:                "OFFERED",
		SecretHash:           strings.Repeat("00", 32),
		BlakeAmountBaseUnits: int64(amount.MustParseBlake("10")),
		BTCSats:              50_000,
		NostrAlicePub:        &alice,
	}); err != nil {
		t.Fatal(err)
	}

	_, err := IgnoreMatch(context.Background(), fixture.env, fixture.matchID, 0)

	if err == nil || !strings.Contains(err.Error(), "already has local swap state") {
		t.Fatalf("a live trade was ignorable: %v", err)
	}

	if _, ok := fixture.env.Files.(memoryFiles)[tombstoneName(fixture.matchID)]; ok {
		t.Fatal("a refused ignore still wrote a tombstone")
	}
}

// TestIgnoredMatchIDsHonourTheTTL: zero means forever, a positive TTL bounds it, and an
// expired tombstone stops existing so a match dismissed by mistake comes back.
func TestIgnoredMatchIDsHonourTheTTL(t *testing.T) {
	fixture := ignoreFixture(t)
	ctx := context.Background()

	if _, err := IgnoreMatch(ctx, fixture.env, fixture.matchID, 0); err != nil {
		t.Fatal(err)
	}

	ignored, err := IgnoredMatchIDs(ctx, fixture.env, 0)

	if err != nil {
		t.Fatal(err)
	}

	if !ignored[fixture.matchID] {
		t.Fatal("a tombstone did not suppress its match under a zero TTL")
	}

	// A TTL that has not run out still suppresses.
	ignored, err = IgnoredMatchIDs(ctx, fixture.env, time.Hour)

	if err != nil {
		t.Fatal(err)
	}

	if !ignored[fixture.matchID] {
		t.Fatal("a fresh tombstone expired early")
	}

	// A TTL that has run out does not, and the tombstone is gone.
	files := fixture.env.Files.(memoryFiles)
	document, ok := files[tombstoneName(fixture.matchID)]

	if !ok {
		t.Fatal("the tombstone disappeared")
	}

	var record map[string]any

	if err := json.Unmarshal(document, &record); err != nil {
		t.Fatal(err)
	}

	record["ignored_at"] = float64(time.Now().Add(-2 * time.Minute).Unix())

	document, err = json.Marshal(record)

	if err != nil {
		t.Fatal(err)
	}

	files[tombstoneName(fixture.matchID)] = document

	ignored, err = IgnoredMatchIDs(ctx, fixture.env, time.Minute)

	if err != nil {
		t.Fatal(err)
	}

	if ignored[fixture.matchID] {
		t.Fatal("an expired tombstone still suppressed its match")
	}

	if _, ok := files[tombstoneName(fixture.matchID)]; ok {
		t.Fatal("an expired tombstone was left to be misread later")
	}
}

// TestIgnoredMatchIDsOnlyCountTombstones: a document that is not named after the match it
// records is not a suppression.
func TestIgnoredMatchIDsOnlyCountTombstones(t *testing.T) {
	files := memoryFiles{
		tombstonesDirectory + "/not-a-tombstone.json": []byte(`{"match_id":"` +
			strings.Repeat("ab", 32) + `"}`),
		tombstonesDirectory + "/" + strings.Repeat("ab", 32) + ".json": []byte(`{"match_id":"` +
			strings.Repeat("cd", 32) + `"}`),
	}

	ignored, err := IgnoredMatchIDs(context.Background(), testEnv(files), 0)

	if err != nil {
		t.Fatal(err)
	}

	if len(ignored) != 0 {
		t.Fatalf("a document that is not a tombstone suppressed something: %v", ignored)
	}
}

// TestReadMatchesIsTheTakersInbox: the listing is the taker's, because a match the participant
// made is their own receipt.
func TestReadMatchesIsTheTakersInbox(t *testing.T) {
	fixture := newReverseOfferFixture(t)
	ctx := context.Background()

	// The fixture's match names Bob as maker and Alice as taker, and the local identity is
	// Bob: he made it, so it is not in his inbox.
	mine, err := ReadMatches(ctx, fixture.env, 100)

	if err != nil {
		t.Fatal(err)
	}

	if len(mine) != 0 {
		t.Fatalf("the maker's own match appeared in its inbox: %+v", mine)
	}

	// As the taker, the same evidence is the inbox.
	fixture.env.Identity = fixture.hers

	hers, err := ReadMatches(ctx, fixture.env, 100)

	if err != nil {
		t.Fatal(err)
	}

	if len(hers) != 1 || hers[0].Event.ID != fixture.matchID {
		t.Fatalf("the taker's inbox = %+v", hers)
	}

	// And it is the same protocol roles on both sides of the listing.
	if hers[0].Match.Direction != market.BTCForBlake ||
		hers[0].Match.MakerRole != "bob" || hers[0].Match.TakerRole != "alice" {
		t.Fatalf("match roles = %+v", hers[0].Match)
	}
}
