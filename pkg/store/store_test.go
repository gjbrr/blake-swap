package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/model"
)

// newSwap builds a valid record for the tests.
func newSwap(t *testing.T, id string) *model.Swap {
	t.Helper()

	swap, err := model.New(strings.NewReader(strings.Repeat("ab", 64)), model.NewParams{
		SwapID:       id,
		BlakeAmount:  amount.Blake(1_000_000),
		BTCSats:      25_000,
		BlakeTimeout: 500,
		BTCTimeout:   400,
	})

	if err != nil {
		t.Fatal(err)
	}

	return swap
}

// testStore runs the store's behavioural tests against one implementation.
//
// Both implementations have to behave identically, so the tests are written
// once. A store that only worked in memory would pass a browser build and lose a
// host's state on a crash, and one that only worked on disk could not run in the
// browser at all.
func testStore(t *testing.T, st Store) {
	t.Helper()

	ctx := context.Background()

	t.Run("save and load round trip", func(t *testing.T) {
		swap := newSwap(t, "aaaa00000001")

		if err := st.Save(ctx, Alice, swap); err != nil {
			t.Fatal(err)
		}

		loaded, err := st.Load(ctx, swap.SwapID)

		if err != nil {
			t.Fatal(err)
		}

		if loaded.SwapID != swap.SwapID ||
			loaded.BlakeAmountBaseUnits != swap.BlakeAmountBaseUnits ||
			loaded.BTCSats != swap.BTCSats ||
			loaded.SecretHash != swap.SecretHash ||
			string(loaded.State) != string(swap.State) {
			t.Fatalf("round trip changed the record: %+v", loaded)
		}
	})

	t.Run("save refuses an invalid record", func(t *testing.T) {
		swap := newSwap(t, "aaaa00000002")
		swap.SecretHash = "nothex"

		if err := st.Save(ctx, Alice, swap); err == nil {
			t.Fatal("an invalid record was saved")
		}
	})

	t.Run("save refuses an unknown role", func(t *testing.T) {
		swap := newSwap(t, "aaaa00000003")

		if err := st.Save(ctx, "mallory", swap); err == nil {
			t.Fatal("an unknown role was accepted")
		}
	})

	t.Run("load refuses a non-canonical id", func(t *testing.T) {
		if _, err := st.Load(ctx, "../etc/passwd"); err == nil {
			t.Fatal("a non-canonical id was loaded")
		}
	})

	t.Run("load of a missing record is not found", func(t *testing.T) {
		_, err := st.Load(ctx, "ffff00000000")

		if err == nil || !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("archive retires the record", func(t *testing.T) {
		swap := newSwap(t, "aaaa00000004")

		if err := st.Save(ctx, Alice, swap); err != nil {
			t.Fatal(err)
		}

		if err := st.Archive(ctx, Alice, swap.SwapID); err != nil {
			t.Fatal(err)
		}

		retired, err := st.Retired(ctx, swap.SwapID)

		if err != nil || !retired {
			t.Fatalf("retired = %v, err = %v", retired, err)
		}

		if _, err := st.Load(ctx, swap.SwapID); err == nil {
			t.Fatal("an archived record is still in the live set")
		}

		ids, err := st.List(ctx)

		if err != nil {
			t.Fatal(err)
		}

		for _, id := range ids {
			if id == swap.SwapID {
				t.Fatal("an archived record is still listed")
			}
		}
	})

	t.Run("messages round trip with the sender identity", func(t *testing.T) {
		swap := newSwap(t, "aaaa00000005")
		sender := strings.Repeat("cd", 32)

		body := map[string]any{"message_type": "offer", "swap_id": swap.SwapID}

		if err := st.SendMessage(ctx, swap.SwapID, "offer", sender, body); err != nil {
			t.Fatal(err)
		}

		message, err := st.ReceiveMessage(ctx, swap.SwapID, "offer")

		if err != nil {
			t.Fatal(err)
		}

		if message.Sender != sender {
			t.Fatalf("sender = %q", message.Sender)
		}

		if message.Body["_nostr_pubkey"] != sender {
			t.Fatalf("the sender was not attached as transport metadata: %v", message.Body)
		}

		if message.Body["message_type"] != "offer" {
			t.Fatalf("body = %v", message.Body)
		}

		kinds, err := st.ListMessages(ctx, swap.SwapID)

		if err != nil || len(kinds) != 1 || kinds[0] != "offer" {
			t.Fatalf("kinds = %v, err = %v", kinds, err)
		}
	})

	t.Run("messages refuse a bad sender", func(t *testing.T) {
		swap := newSwap(t, "aaaa00000006")

		for _, sender := range []string{"", "nope", strings.Repeat("zz", 32), strings.Repeat("cd", 31)} {
			if err := st.SendMessage(ctx, swap.SwapID, "offer", sender, map[string]any{}); err == nil {
				t.Fatalf("sender %q was accepted", sender)
			}
		}
	})

	t.Run("receiving an absent message is not found", func(t *testing.T) {
		swap := newSwap(t, "aaaa00000007")

		_, err := st.ReceiveMessage(ctx, swap.SwapID, "offer")

		if err == nil || !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestMemoryStore(t *testing.T) { testStore(t, NewMemory()) }

// TestCanonicalID pins the rule a state scan uses: the state root is a directory
// the operator also uses for other things, and a scan that tried to load those
// would report them as corrupt swaps.
func TestCanonicalID(t *testing.T) {
	cases := map[string]bool{
		"aaaa00000001.json": true,
		"notes.json":        false,
		"aaaa00000001.txt":  false,
		".json":             false,
		"aaaa0000000.json":  false,
		"AAAA00000001.json": false,
	}

	for name, want := range cases {
		got, ok := CanonicalID(name)

		if ok != want {
			t.Fatalf("CanonicalID(%q) = %q, %v", name, got, ok)
		}
	}
}

// TestSupersededPlaceholders covers the auto-archive rule: only a record nobody
// else can reference may be archived behind the operator's back.
func TestSupersededPlaceholders(t *testing.T) {
	ctx := context.Background()
	st := NewMemory()

	placeholder := newSwap(t, "aaaa00000101")

	if err := st.Save(ctx, Alice, placeholder); err != nil {
		t.Fatal(err)
	}

	announced := newSwap(t, "aaaa00000102")
	pub := strings.Repeat("cd", 32)
	announced.NostrBobPub = &pub

	if err := st.Save(ctx, Alice, announced); err != nil {
		t.Fatal(err)
	}

	funded := newSwap(t, "aaaa00000103")
	txid := strings.Repeat("ee", 32)
	funded.BlakeFundingTxID = &txid

	if err := st.Save(ctx, Alice, funded); err != nil {
		t.Fatal(err)
	}

	placeholders, err := SupersededPlaceholders(ctx, st, "aaaa00000109")

	if err != nil {
		t.Fatal(err)
	}

	if len(placeholders) != 1 || placeholders[0].SwapID != placeholder.SwapID {
		t.Fatalf("placeholders = %+v", placeholders)
	}

	archived, err := ArchiveSuperseded(ctx, st, "aaaa00000109", Alice)

	if err != nil {
		t.Fatal(err)
	}

	if len(archived) != 1 || archived[0] != placeholder.SwapID {
		t.Fatalf("archived = %v", archived)
	}

	// The announced record must still be live: a peer may be holding it.
	if retired, _ := st.Retired(ctx, announced.SwapID); retired {
		t.Fatal("an announced record was archived")
	}

	if retired, _ := st.Retired(ctx, funded.SwapID); retired {
		t.Fatal("a funded record was archived")
	}
}

// TestRecordIsSortedAndNewlineTerminated keeps the stored form diffable, which
// is what the parity suites do with two implementations' records.
func TestRecordIsSortedAndNewlineTerminated(t *testing.T) {
	swap := newSwap(t, "aaaa00000201")

	encoded, err := encodeRecord(swap)

	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasSuffix(string(encoded), "\n") {
		t.Fatal("the record is not newline-terminated")
	}

	var record map[string]any

	if err := json.Unmarshal(encoded, &record); err != nil {
		t.Fatal(err)
	}

	keys := make([]string, 0, len(record))

	seen := map[string]bool{}

	for _, line := range strings.Split(string(encoded), "\n") {
		var key string

		if _, err := fmt.Sscanf(strings.TrimSpace(line), "%q:", &key); err == nil {
			if seen[key] {
				t.Fatalf("key %q appears twice", key)
			}

			seen[key] = true
			keys = append(keys, key)
		}
	}

	for i := 1; i < len(keys); i++ {
		if keys[i-1] > keys[i] {
			t.Fatalf("keys are not sorted: %q before %q", keys[i-1], keys[i])
		}
	}
}

// TestCorruptEnvelopeIsRefused: a spool file whose sender is not a public key
// cannot be attributed, and an unattributable message must not be treated as if
// it came from the counterparty.
func TestCorruptEnvelopeIsRefused(t *testing.T) {
	st := NewMemory()
	swapID := "aaaa00000801"

	st.StoreRawMessage(swapID, "offer", []byte(`{"_file_transport_version":2,"sender_pubkey":"notakey","message":{}}`))

	if _, err := st.ReceiveMessage(context.Background(), swapID, "offer"); err == nil {
		t.Fatal("a message with an unattributable sender was accepted")
	}
}
