//go:build !js

package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gjbrr/blake-swap/pkg/platform"
	"github.com/gjbrr/blake-swap/pkg/proto"
)

// activityVault is an unlocked vault with no records, for the log's own tests.
func activityVault(t *testing.T) *vaultDomain {
	t.Helper()

	vault := NewVaultDomain(platform.NewMemoryKV(), nil)

	if result := run(t, vault, "vault.create", map[string]string{
		"passphrase": "activity passphrase",
	}); !result.OK {
		t.Fatalf("create = %+v", result)
	}

	return vault
}

// logged appends one entry at a fixed time and returns the error.
func logged(
	t *testing.T, vault *vaultDomain, command string, args map[string]string, result proto.Result,
) error {
	t.Helper()

	policy, needed := activityPolicyFor(command)

	if !needed {
		t.Fatalf("%s is not in the activity table", command)
	}

	return appendActivity(context.Background(), vault, proto.Request{
		Command: command, Args: args,
	}, result, policy, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
}

// readLog reads the log back.
func readLog(t *testing.T, vault *vaultDomain) []activityEntry {
	t.Helper()

	raw, err := vault.readSecret(context.Background(), activityName)

	if err != nil {
		t.Fatal(err)
	}

	var entries []activityEntry

	if strings.TrimSpace(raw) == "" {
		return nil
	}

	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		t.Fatalf("the log is not readable: %v", err)
	}

	return entries
}

// TestActivityEntriesAreNewestFirst: three commands, in the order they ran.
func TestActivityEntriesAreNewestFirst(t *testing.T) {
	vault := activityVault(t)

	for _, command := range []string{"market.publish", "market.take", "market.accept-take"} {
		if err := logged(t, vault, command, nil, proto.Result{
			Command: command, OK: true, Text: "did the thing\na second line",
		}); err != nil {
			t.Fatal(err)
		}
	}

	entries := readLog(t, vault)

	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}

	for index, want := range []string{"market.accept-take", "market.take", "market.publish"} {
		if entries[index].Command != want {
			t.Fatalf("entry %d is %q, want %q", index, entries[index].Command, want)
		}

		// The summary is one line, and the timestamp is seconds in UTC.
		if entries[index].Summary != "did the thing" {
			t.Fatalf("entry %d summary is %q", index, entries[index].Summary)
		}

		if entries[index].At != "2026-10-20T12:00:00Z" {
			t.Fatalf("entry %d at = %q", index, entries[index].At)
		}
	}

	// And the command renders them newest first too.
	result := run(t, vault, "activity.list", nil)

	if !result.OK {
		t.Fatalf("activity.list = %+v", result)
	}

	var view proto.ActivityView

	if err := proto.DecodeData(result, &view); err != nil {
		t.Fatal(err)
	}

	if view.Total != 3 || len(view.Entries) != 3 {
		t.Fatalf("view = %+v", view)
	}

	if view.Entries[0].Command != "market.accept-take" {
		t.Fatalf("the view is not newest first: %+v", view.Entries)
	}
}

// TestActivityIsBounded: five hundred and one entries keep five hundred, and the oldest is
// the one that went.
func TestActivityIsBounded(t *testing.T) {
	vault := activityVault(t)

	for index := 0; index <= activityMaxEntries; index++ {
		policy, needed := activityPolicyFor("market.publish")

		if !needed {
			t.Fatal("market.publish is not logged")
		}

		if err := appendActivity(context.Background(), vault, proto.Request{
			Command: "market.publish",
			Args:    map[string]string{"swap_id": "aaaa00000001"},
		}, proto.Result{
			Command: "market.publish", OK: true, Text: "entry " + itoa(uint32(index)),
		}, policy, time.Unix(int64(index), 0).UTC()); err != nil {
			t.Fatal(err)
		}
	}

	entries := readLog(t, vault)

	if len(entries) != activityMaxEntries {
		t.Fatalf("the log holds %d entries, want %d", len(entries), activityMaxEntries)
	}

	// The newest is the last one appended, and the oldest kept is the second.
	if entries[0].At != time.Unix(int64(activityMaxEntries), 0).UTC().Format(time.RFC3339) {
		t.Fatalf("the newest entry is %q", entries[0].At)
	}

	for _, entry := range entries {
		if entry.Summary == "entry 0" {
			t.Fatal("the oldest entry was kept and a newer one dropped")
		}
	}
}

// TestActivityRedactsWhatMustNotBeStored: a passphrase and a secret must not reach the log,
// and the raw sealed value must not hold either in the clear.
func TestActivityRedactsWhatMustNotBeStored(t *testing.T) {
	vault := activityVault(t)
	ctx := context.Background()
	passphrase := "activity passphrase"
	secret := strings.Repeat("ab", 32)

	// A passphrase-bearing command is not in the table at all.
	for _, command := range []string{"vault.create", "vault.unlock", "vault.import"} {
		if _, needed := activityPolicyFor(command); needed {
			t.Fatalf("%s is in the activity table", command)
		}

		recordActivity(ctx, vault, proto.Request{
			Command: command,
			Args:    map[string]string{"passphrase": passphrase, "json": "{}"},
		}, proto.Result{Command: command, OK: false, Error: "wrong passphrase"}, time.Now())
	}

	// A secret-bearing one is in the table and records a fixed sentence instead.
	if err := logged(t, vault, "engine.extract-secret", map[string]string{
		"swap_id": "aaaa00000001",
	}, proto.Result{
		Command: "engine.extract-secret",
		OK:      true,
		Text:    "secret extracted: " + secret,
	}); err != nil {
		t.Fatal(err)
	}

	if err := logged(t, vault, "identity.generate", nil, proto.Result{
		Command: "identity.generate", OK: true, Text: "identity generated: " + secret,
	}); err != nil {
		t.Fatal(err)
	}

	// A logged command whose arguments happen to carry a passphrase: the worker ignores the
	// argument, so this is the realistic leak — an argument map copied into the log would
	// carry whatever a caller chose to send.
	if err := logged(t, vault, "market.publish", map[string]string{
		"blake":      "1",
		"sats":       "1000",
		"passphrase": passphrase,
		"secret":     secret,
	}, proto.Result{
		Command: "market.publish", OK: true, Text: "published an offer",
	}); err != nil {
		t.Fatal(err)
	}

	// And the export, whose reply is the whole vault.
	if err := logged(t, vault, "vault.export", nil, proto.Result{
		Command: "vault.export", OK: true,
		Text: "vault exported: 3 entries, 40 bytes",
	}); err != nil {
		t.Fatal(err)
	}

	entries := readLog(t, vault)

	if len(entries) != 4 {
		t.Fatalf("the log holds %d entries, want 4", len(entries))
	}

	want := map[string]string{
		"engine.extract-secret": "secret extracted",
		"identity.generate":     "identity generated",
		"vault.export":          "vault exported",
	}

	for _, entry := range entries {
		if fixed, ok := want[entry.Command]; ok && entry.Summary != fixed {
			t.Fatalf("%s summary = %q, want %q", entry.Command, entry.Summary, fixed)
		}

		// Nothing an argument carried may reach the entry itself either.
		for name, needle := range map[string]string{
			"the passphrase":  passphrase,
			"the secret":      secret,
			"an argument map": "passphrase:",
		} {
			if strings.Contains(entry.Summary+entry.Error+entry.Swap, needle) {
				t.Fatalf("%s entry holds %s", entry.Command, name)
			}
		}
	}

	// The stored value is sealed, so the command text is not in it either, and neither is
	// any of the plaintext that must not be.
	raw, found, err := vault.kv.Get(ctx, "secret/"+activityName)

	if err != nil || !found {
		t.Fatalf("the log is not in the store: %v", err)
	}

	for name, needle := range map[string]string{
		"the passphrase":       passphrase,
		"the extracted secret": secret,
		"a command name":       "engine.extract-secret",
	} {
		if strings.Contains(string(raw), needle) {
			t.Fatalf("the sealed log contains %s", name)
		}
	}
}

// TestActivityTruncatesToTheByteLimits: the two free-text fields are bounded.
func TestActivityTruncatesToTheByteLimits(t *testing.T) {
	vault := activityVault(t)

	long := strings.Repeat("s", activitySummaryBytes+100)
	refusal := strings.Repeat("e", activityErrorBytes+100)

	if err := logged(t, vault, "market.take", nil, proto.Result{
		Command: "market.take", OK: true, Text: long,
	}); err != nil {
		t.Fatal(err)
	}

	if err := logged(t, vault, "market.take", nil, proto.Result{
		Command: "market.take", OK: false, Error: refusal,
	}); err != nil {
		t.Fatal(err)
	}

	entries := readLog(t, vault)

	if len(entries[0].Error) != activityErrorBytes {
		t.Fatalf("the error is %d bytes, want %d", len(entries[0].Error), activityErrorBytes)
	}

	if entries[0].Summary != "" {
		t.Fatalf("a failure kept a summary: %q", entries[0].Summary)
	}

	if len(entries[1].Summary) != activitySummaryBytes {
		t.Fatalf("the summary is %d bytes, want %d",
			len(entries[1].Summary), activitySummaryBytes)
	}

	if entries[1].Error != "" {
		t.Fatalf("a success kept an error: %q", entries[1].Error)
	}

	// A multi-byte character is not cut in half.
	if got := clip(strings.Repeat("é", 200), 201); len(got) != 200 {
		t.Fatalf("clip split a character: %d bytes", len(got))
	}
}

// TestActivitySurvivesANewDomain: the log is sealed in the store, so a new domain over the
// same KV reads it after unlock.
func TestActivitySurvivesANewDomain(t *testing.T) {
	kv := platform.NewMemoryKV()
	first := NewVaultDomain(kv, nil)

	if result := run(t, first, "vault.create", map[string]string{
		"passphrase": "activity passphrase",
	}); !result.OK {
		t.Fatalf("create = %+v", result)
	}

	if err := logged(t, first, "market.publish", nil, proto.Result{
		Command: "market.publish", OK: true, Text: "published an offer",
	}); err != nil {
		t.Fatal(err)
	}

	second := NewVaultDomain(kv, nil)

	if result := run(t, second, "vault.unlock", map[string]string{
		"passphrase": "activity passphrase",
	}); !result.OK {
		t.Fatalf("unlock = %+v", result)
	}

	entries := readLog(t, second)

	if len(entries) != 1 || entries[0].Summary != "published an offer" {
		t.Fatalf("the log did not survive: %+v", entries)
	}
}

// TestActivityRefusesToOverwriteAnUnreadableLog: a log that will not open is evidence, and
// clearing it is the only thing that works.
func TestActivityRefusesToOverwriteAnUnreadableLog(t *testing.T) {
	vault := activityVault(t)
	ctx := context.Background()

	if err := vault.writeSecret(ctx, activityName, "{not json"); err != nil {
		t.Fatal(err)
	}

	err := logged(t, vault, "market.publish", nil, proto.Result{
		Command: "market.publish", OK: true, Text: "published",
	})

	if err == nil || !strings.Contains(err.Error(), "unreadable") ||
		!strings.Contains(err.Error(), "clear it to continue") {
		t.Fatalf("an unreadable log was overwritten: %v", err)
	}

	// The list refuses too, rather than reporting an empty log.
	if result := run(t, vault, "activity.list", nil); result.OK {
		t.Fatalf("an unreadable log was listed as empty: %+v", result)
	}

	// And clearing works.
	cleared := run(t, vault, "activity.clear", map[string]string{"confirm": "clear"})

	if !cleared.OK {
		t.Fatalf("activity.clear = %+v", cleared)
	}

	if stored, _ := vault.readSecret(ctx, activityName); stored != "" {
		t.Fatalf("the log survived a clear: %q", stored)
	}

	// After a clear, appending works again.
	if err := logged(t, vault, "market.publish", nil, proto.Result{
		Command: "market.publish", OK: true, Text: "published",
	}); err != nil {
		t.Fatal(err)
	}

	if entries := readLog(t, vault); len(entries) != 1 {
		t.Fatalf("entries after the clear = %d", len(entries))
	}
}

// TestActivityClearNeedsTheConfirmation: a destructive command needs its second gesture,
// and the argument is what says the person made it.
func TestActivityClearNeedsTheConfirmation(t *testing.T) {
	vault := activityVault(t)

	if err := logged(t, vault, "market.publish", nil, proto.Result{
		Command: "market.publish", OK: true, Text: "published",
	}); err != nil {
		t.Fatal(err)
	}

	for _, confirm := range []string{"", "yes", "clear?"} {
		result := run(t, vault, "activity.clear", map[string]string{"confirm": confirm})

		if result.OK {
			t.Fatalf("confirm=%q cleared the log", confirm)
		}
	}

	cleared := run(t, vault, "activity.clear", map[string]string{"confirm": "CLEAR"})

	if !cleared.OK || !strings.Contains(cleared.Text, "1 entries removed") {
		t.Fatalf("clear = %+v", cleared)
	}

	if stored, _ := vault.readSecret(context.Background(), activityName); stored != "" {
		t.Fatalf("the key survived the clear: %q", stored)
	}
}

// TestActivityNeedsAnUnlockedVault: both commands read and write through the key.
func TestActivityNeedsAnUnlockedVault(t *testing.T) {
	vault := activityVault(t)

	if err := logged(t, vault, "market.publish", nil, proto.Result{
		Command: "market.publish", OK: true, Text: "published",
	}); err != nil {
		t.Fatal(err)
	}

	if result := run(t, vault, "vault.lock", nil); !result.OK {
		t.Fatalf("lock = %+v", result)
	}

	for _, command := range []string{"activity.list", "activity.clear"} {
		result := run(t, vault, command, map[string]string{"confirm": "clear"})

		if result.OK || !strings.Contains(result.Error, "the vault is locked") {
			t.Fatalf("%s on a locked vault = %+v", command, result)
		}
	}
}

// TestActivityRecordsTheCommandsThatChangeSomething: the table's two halves, checked
// against the commands the worker actually dispatches.
func TestActivityRecordsTheCommandsThatChangeSomething(t *testing.T) {
	logged := []string{
		"market.publish", "market.take", "market.cancel", "market.accept-take",
		"market.accept-match", "market.accept-reverse-match", "market.accept-reverse-offer",
		"market.ignore-match", "wallets.create", "wallets.set", "proxy.set",
		"coord.set-mode", "engine.extract-secret", "identity.generate", "vault.export",
		"engine.new", "engine.accept-offer", "engine.publish-offer", "engine.fund-blake",
		"engine.fund-btc", "engine.claim-btc", "engine.claim-blake", "engine.refund-btc",
		"engine.refund-blake", "engine.complete", "engine.sync-funding",
		"engine.sync-settlement", "engine.resume", "engine.resume-all", "engine.watch-htlc",
		"engine.refresh-timeouts", "engine.publish-blake-funded",
		"engine.receive-blake-funded", "engine.publish-btc-claimed",
		"engine.receive-btc-claimed", "coord.accept.send", "coord.accept.receive",
		"coord.offer.send", "coord.blake-funded.send",
	}

	for _, command := range logged {
		if _, needed := activityPolicyFor(command); !needed {
			t.Errorf("%s is not logged", command)
		}
	}

	for _, command := range []string{
		"vault.import", "vault.status", "vault.unlock", "vault.create", "vault.lock",
		"wallets.status", "engine.status", "engine.list", "engine.show", "engine.dashboard",
		"engine.next", "engine.reconcile", "engine.recovery-plan", "market.list",
		"market.takes", "market.matches", "market.relays", "coord.mode", "activity.list",
		"activity.clear", "ui.get", "ui.set", "state.get", "vault.selftest",
	} {
		if policy, needed := activityPolicyFor(command); needed {
			t.Errorf("%s is logged (%+v)", command, policy)
		}
	}
}

// TestActivityIsWrittenAtTheDispatchBoundary: one command through the worker's own entry
// point produces one entry, and a read produces none.
func TestActivityIsWrittenAtTheDispatchBoundary(t *testing.T) {
	vault := activityVault(t)
	ctx := context.Background()

	// A command whose domain does not exist still goes through the boundary, and a
	// refusal is a thing that happened.
	body, err := proto.EncodeRequest(proto.Request{
		Command: "market.publish",
		Args:    map[string]string{"blake": "1", "sats": "1000"},
	})

	if err != nil {
		t.Fatal(err)
	}

	handleCommand(ctx, vault, nil, nil, nil, body)

	entries := readLog(t, vault)

	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}

	if entries[0].Command != "market.publish" || entries[0].OK {
		t.Fatalf("entry = %+v", entries[0])
	}

	if entries[0].Error == "" {
		t.Fatal("a refusal was logged with no error")
	}

	// A read is not logged.
	read, err := proto.EncodeRequest(proto.Request{Command: "vault.status"})

	if err != nil {
		t.Fatal(err)
	}

	handleCommand(ctx, vault, nil, nil, nil, read)

	if entries := readLog(t, vault); len(entries) != 1 {
		t.Fatalf("a read was logged: %d entries", len(entries))
	}
}

// TestActivityListHonoursTheLimit: the page asks for a page of the log.
func TestActivityListHonoursTheLimit(t *testing.T) {
	vault := activityVault(t)

	for index := 0; index < 5; index++ {
		if err := logged(t, vault, "market.publish", nil, proto.Result{
			Command: "market.publish", OK: true, Text: "entry " + itoa(uint32(index)),
		}); err != nil {
			t.Fatal(err)
		}
	}

	result := run(t, vault, "activity.list", map[string]string{"limit": "2"})

	var view proto.ActivityView

	if err := proto.DecodeData(result, &view); err != nil {
		t.Fatal(err)
	}

	if view.Total != 5 || len(view.Entries) != 2 {
		t.Fatalf("view = %+v", view)
	}

	// An absurd limit is capped rather than refused, and a bad one is refused.
	if result := run(t, vault, "activity.list", map[string]string{"limit": "100000"}); !result.OK {
		t.Fatalf("a large limit was refused: %+v", result)
	}

	for _, bad := range []string{"0", "-1", "many"} {
		if result := run(t, vault, "activity.list", map[string]string{"limit": bad}); result.OK {
			t.Fatalf("limit=%q was accepted", bad)
		}
	}
}
