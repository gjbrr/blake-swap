package marketflow

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/keys"
	"github.com/gjbrr/blake-swap/pkg/nostr"
)

// taker is the identity that takes offers in these tests.
func taker(t *testing.T) *nostr.Key {
	t.Helper()

	key, err := nostr.NewKey(strings.Repeat("22", 32))

	if err != nil {
		t.Fatal(err)
	}

	return key
}

// listed builds a parsed offer to take.
func listed(t *testing.T) ListedOffer {
	t.Helper()

	built, offer := buildTestOffer(t)

	parsed, err := ParseOffer(built, time.Unix(1_700_000_100, 0))

	if err != nil {
		t.Fatal(err)
	}

	_ = offer

	return *parsed
}

func TestTakeRoundTrip(t *testing.T) {
	offer := listed(t)

	built, take, err := BuildTake(taker(t), BuildTakeOptions{
		Offer: offer, CreatedAt: 1_700_000_200,
	})

	if err != nil {
		t.Fatal(err)
	}

	parsed, err := ParseTake(built, time.Unix(1_700_000_300, 0))

	if err != nil {
		t.Fatal(err)
	}

	if parsed.OfferID != offer.EventID || parsed.Maker != offer.Maker {
		t.Fatalf("parsed = %+v", parsed)
	}

	if parsed.Taker != taker(t).Public {
		t.Fatalf("taker = %q", parsed.Taker)
	}

	if parsed.BlakeAmount != take.BlakeAmount || parsed.BTCSats != take.BTCSats {
		t.Fatalf("economics changed: %+v", parsed)
	}

	// The roles come from the offer's direction, not from the taker's opinion.
	if parsed.MakerRole != offer.MakerRole || parsed.TakerRole != offer.TakerRole {
		t.Fatalf("roles = %s/%s", parsed.MakerRole, parsed.TakerRole)
	}
}

// TestTakeRolesComeFromTheDirection: a taker that could name its own role could
// claim the side that funds later.
func TestTakeRolesComeFromTheDirection(t *testing.T) {
	offer := listed(t)

	built, _, err := BuildTake(taker(t), BuildTakeOptions{Offer: offer, CreatedAt: 1_700_000_200})

	if err != nil {
		t.Fatal(err)
	}

	// Rewrite the roles in the content, keeping everything else valid, and re-sign.
	mangled := resignWithContent(t, built, func(message map[string]any) {
		message["maker_role"] = "bob"
		message["taker_role"] = "alice"
	})

	parsed, err := ParseTake(mangled, time.Unix(1_700_000_300, 0))

	//
	// The vocabulary accepts the fields (they are strings), so the event layer is
	// where the contradiction has to surface: the direction no longer implies these
	// roles, and a take whose roles do not follow its direction is not a valid take.
	//
	if err == nil || !strings.Contains(err.Error(), "role mismatch") {
		t.Fatalf("err = %v, parsed = %+v", err, parsed)
	}
}

// TestTakeRefusesAnotherOffer: the `e` tag and the content must name the same offer.
func TestTakeRefusesAnotherOffer(t *testing.T) {
	offer := listed(t)

	built, _, err := BuildTake(taker(t), BuildTakeOptions{Offer: offer, CreatedAt: 1_700_000_200})

	if err != nil {
		t.Fatal(err)
	}

	mangled := built
	mangled.Tags = [][]string{
		{"t", TakeTag},
		{"e", strings.Repeat("aa", 32)},
		{"p", offer.Maker},
		{"direction", string(offer.Direction)},
	}

	resignEvent(t, &mangled, taker(t))

	if _, err := ParseTake(mangled, time.Unix(1_700_000_300, 0)); err == nil ||
		!strings.Contains(err.Error(), "different offer") {
		t.Fatalf("err = %v", err)
	}
}

// TestTakeRefusesAnotherMaker: the `p` tag and the content must name the same maker.
func TestTakeRefusesAnotherMaker(t *testing.T) {
	offer := listed(t)

	built, _, err := BuildTake(taker(t), BuildTakeOptions{Offer: offer, CreatedAt: 1_700_000_200})

	if err != nil {
		t.Fatal(err)
	}

	mangled := built
	mangled.Tags = [][]string{
		{"t", TakeTag},
		{"e", offer.EventID},
		{"p", strings.Repeat("bb", 32)},
		{"direction", string(offer.Direction)},
	}

	resignEvent(t, &mangled, taker(t))

	if _, err := ParseTake(mangled, time.Unix(1_700_000_300, 0)); err == nil ||
		!strings.Contains(err.Error(), "different maker") {
		t.Fatalf("err = %v", err)
	}
}

// TestTakeRefusesAnImpersonatedTaker: the taker is the author, so a take that names
// someone else as its taker is not that person's take.
func TestTakeRefusesAnImpersonatedTaker(t *testing.T) {
	offer := listed(t)

	built, _, err := BuildTake(taker(t), BuildTakeOptions{Offer: offer, CreatedAt: 1_700_000_200})

	if err != nil {
		t.Fatal(err)
	}

	mangled := resignWithContent(t, built, func(message map[string]any) {
		message["taker"] = strings.Repeat("cc", 32)
	})

	if _, err := ParseTake(mangled, time.Unix(1_700_000_300, 0)); err == nil ||
		!strings.Contains(err.Error(), "not the event author") {
		t.Fatalf("err = %v", err)
	}
}

func TestCancelRoundTrip(t *testing.T) {
	offer := listed(t)

	built, cancel, err := BuildCancel(maker(t), BuildCancelOptions{
		OfferID: offer.EventID, CreatedAt: 1_700_000_400,
	})

	if err != nil {
		t.Fatal(err)
	}

	parsed, err := ParseCancel(built, time.Unix(1_700_000_500, 0))

	if err != nil {
		t.Fatal(err)
	}

	if parsed.OfferID != cancel.OfferID || parsed.OfferID != offer.EventID {
		t.Fatalf("parsed = %+v", parsed)
	}
}

// TestCancelRefusesAMismatchedReference: the signed `e` tag and the content must
// name the same offer.
func TestCancelRefusesAMismatchedReference(t *testing.T) {
	built, _, err := BuildCancel(maker(t), BuildCancelOptions{
		OfferID: strings.Repeat("dd", 32), CreatedAt: 1_700_000_400,
	})

	if err != nil {
		t.Fatal(err)
	}

	mangled := built
	mangled.Tags = [][]string{
		{"t", CancelTag},
		{"e", strings.Repeat("ee", 32)},
	}

	resignEvent(t, &mangled, maker(t))

	if _, err := ParseCancel(mangled, time.Unix(1_700_000_500, 0)); err == nil ||
		!strings.Contains(err.Error(), "different offer") {
		t.Fatalf("err = %v", err)
	}
}

func TestCancelRefusesAShortOfferID(t *testing.T) {
	if _, _, err := BuildCancel(maker(t), BuildCancelOptions{OfferID: "abc"}); err == nil {
		t.Fatal("a short offer id was accepted")
	}
}

// TestBuildTakeForUnsupportedVersion: a version the taker does not understand is
// refused rather than guessed at.
func TestBuildTakeForUnsupportedVersion(t *testing.T) {
	offer := listed(t)
	offer.Version = 3

	if _, _, err := BuildTake(taker(t), BuildTakeOptions{Offer: offer}); err == nil {
		t.Fatal("a take was built for an unsupported offer version")
	}
}

// resignWithContent rewrites an event's content with a mutation and re-signs it.
func resignWithContent(t *testing.T, built event.Event, mutate func(map[string]any)) event.Event {
	t.Helper()

	var message map[string]any

	if err := json.Unmarshal([]byte(built.Content), &message); err != nil {
		t.Fatal(err)
	}

	mutate(message)

	content, err := event.CanonicalJSON(message)

	if err != nil {
		t.Fatal(err)
	}

	mangled := built
	mangled.Content = string(content)

	resignEvent(t, &mangled, taker(t))

	return mangled
}

// resignEvent signs an event again after a test changed it.
func resignEvent(t *testing.T, e *event.Event, key *nostr.Key) {
	t.Helper()

	e.Pubkey = key.Public

	id, err := e.ComputeID()

	if err != nil {
		t.Fatal(err)
	}

	e.ID = id

	signer, err := keys.FromSecretHex(key.Secret)

	if err != nil {
		t.Fatal(err)
	}

	if err := event.Sign(e, signer); err != nil {
		t.Fatal(err)
	}
}
