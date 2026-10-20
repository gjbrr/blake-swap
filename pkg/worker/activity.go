package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gjbrr/blake-swap/pkg/proto"
	"github.com/gjbrr/blake-swap/pkg/store"
)

//
// The activity log is a local, sealed, bounded record of the commands that changed
// something.
//
// It is not a relay feed, not a second record store and not a debug console. One entry says
// that a command ran, on which swap, whether it worked, and one line about it: enough for a
// person to remember what they did last week, and nothing that could leak a secret.
//

const (
	// activityName is the sealed value's name. Through the store's own PutSecret it is
	// stored at secret/activity.log.
	activityName = "activity.log"
	// activityMaxEntries bounds the log. A person reads the last few dozen; the cap is
	// what keeps a long-lived client from growing one sealed value without limit.
	activityMaxEntries = 500
	// activityDefaultLimit is how many activity.list returns when it is not told.
	activityDefaultLimit = 100
	// The byte caps on the two free-text fields.
	activitySummaryBytes = 200
	activityErrorBytes   = 500
)

// activityEntry is one recorded command, as it is stored.
type activityEntry struct {
	// At is the worker's clock, RFC3339 seconds in UTC.
	At string `json:"at"`
	// Command is the exact command name.
	Command string `json:"command"`
	// Swap is the swap id, when the request named a valid one.
	Swap string `json:"swap,omitempty"`
	// OK is the result's own answer.
	OK bool `json:"ok"`
	// Summary is the first line of the result's text, or a fixed sentence for a command
	// whose text can carry a secret.
	Summary string `json:"summary,omitempty"`
	// Error is the refusal, truncated. A failure has no summary.
	Error string `json:"error,omitempty"`
}

// activityPolicy is what one command contributes to the log.
type activityPolicy struct {
	// summary replaces the text slice. It is how the four commands whose replies can carry
	// a secret are recorded without depending on truncation to remove it.
	summary string
}

// activityPolicyFor reports whether a command is logged, and how.
//
// This table is the whole rule, in one place. It is deliberately a list of commands rather
// than a prefix or a heuristic: "log everything that is not a read" would start logging
// whatever command somebody adds next, and the first such command whose reply contains a
// key would put a key in the log.
func activityPolicyFor(command string) (activityPolicy, bool) {
	// Fixed summaries: the reply itself must never be copied.
	switch command {
	case "engine.extract-secret":
		return activityPolicy{summary: "secret extracted"}, true
	case "identity.generate":
		return activityPolicy{summary: "identity generated"}, true
	case "vault.export":
		// The envelope is the whole vault: not one byte of the reply is copied.
		return activityPolicy{summary: "vault exported"}, true
	}

	//
	// Every coordination message, by its shape rather than by a list of kinds: the kinds
	// come from the wire schema, so a list here would silently stop covering the next one.
	// Both directions get a fixed summary, because a reply quotes the message it carried.
	//
	if strings.HasPrefix(command, "coord.") {
		switch {
		case strings.HasSuffix(command, ".send"):
			return activityPolicy{summary: "sent"}, true
		case strings.HasSuffix(command, ".receive"):
			return activityPolicy{summary: "received"}, true
		}
	}

	// The market's state-changing commands. Its reads are not here.
	switch command {
	case "market.publish", "market.take", "market.cancel", "market.accept-take",
		"market.accept-match", "market.accept-reverse-match", "market.accept-reverse-offer",
		"market.ignore-match", "market.set-relays":
		return activityPolicy{}, true
	}

	// Configuration that outlives the command.
	switch command {
	case "wallets.create", "wallets.set", "proxy.set", "coord.set-mode":
		return activityPolicy{}, true
	}

	// Every engine command that writes, publishes or announces. The reads are excluded by
	// name, because a read is not a thing that happened to a swap.
	if strings.HasPrefix(command, "engine.") {
		switch command {
		case "engine.status", "engine.list", "engine.show", "engine.dashboard",
			"engine.next", "engine.reconcile", "engine.recovery-plan":
			return activityPolicy{}, false
		}

		return activityPolicy{}, true
	}

	return activityPolicy{}, false
}

// appendActivity writes one entry, newest first.
//
// A log that will not open is not overwritten: it is the record of what happened, and
// silently replacing it with one line would destroy evidence at the moment it matters. The
// refusal names the one command that does work on such a log.
func appendActivity(
	ctx context.Context,
	vault *vaultDomain,
	request proto.Request,
	result proto.Result,
	policy activityPolicy,
	now time.Time,
) error {
	existing, err := vault.readSecret(ctx, activityName)

	if err != nil {
		return err
	}

	var entries []activityEntry

	if strings.TrimSpace(existing) != "" {
		if err := json.Unmarshal([]byte(existing), &entries); err != nil {
			return fmt.Errorf("the activity log is unreadable; clear it to continue")
		}
	}

	entry := activityEntry{
		At:      now.UTC().Format(time.RFC3339),
		Command: request.Command,
		Swap:    activitySwap(request.Args),
		OK:      result.OK,
	}

	if result.OK {
		entry.Summary = policy.summary

		if entry.Summary == "" {
			entry.Summary = clip(firstLine(result.Text), activitySummaryBytes)
		}
	} else {
		entry.Error = clip(result.Error, activityErrorBytes)
	}

	entries = append([]activityEntry{entry}, entries...)

	if len(entries) > activityMaxEntries {
		entries = entries[:activityMaxEntries]
	}

	encoded, err := json.Marshal(entries)

	if err != nil {
		return err
	}

	return vault.writeSecret(ctx, activityName, string(encoded))
}

// activitySwap reads the swap a request named, if it named a valid one.
//
// The id is checked rather than copied: an argument is operator input, and a log full of
// "swap ../escape" entries would be worse than a log with a blank column.
func activitySwap(args map[string]string) string {
	for _, name := range []string{"swap_id", "swap"} {
		value := strings.TrimSpace(args[name])

		if value == "" {
			continue
		}

		if store.ValidateSwapID(value) == nil {
			return value
		}
	}

	return ""
}

// clip truncates a string to a byte budget without splitting a character.
func clip(text string, limit int) string {
	if len(text) <= limit {
		return text
	}

	cut := limit

	for cut > 0 && !utf8Start(text[cut]) {
		cut--
	}

	return text[:cut]
}

// utf8Start reports whether a byte begins a UTF-8 sequence.
func utf8Start(b byte) bool { return b&0xc0 != 0x80 }

// firstLine is the part of a command's text a log can keep.
func firstLine(text string) string {
	if at := strings.IndexByte(text, '\n'); at >= 0 {
		return strings.TrimSpace(text[:at])
	}

	return strings.TrimSpace(text)
}

// recordActivity appends one entry for a command, best effort.
//
// It runs after the domain has answered and before the result is encoded. A failure to
// write the log is reported on the worker console and never changes the answer: a funding
// that worked is not a failure because a log line could not be written.
func recordActivity(
	ctx context.Context, vault *vaultDomain, request proto.Request, result proto.Result,
	now time.Time,
) {
	if vault == nil || vault.store == nil {
		return
	}

	policy, needed := activityPolicyFor(request.Command)

	if !needed {
		return
	}

	if err := appendActivity(ctx, vault, request, result, policy, now); err != nil {
		logf("activity: " + request.Command + ": " + err.Error())
	}
}

// listActivity renders the log, newest first.
func (d *vaultDomain) listActivity(ctx context.Context, request proto.Request) proto.Result {
	if d.store == nil {
		return proto.Result{Command: request.Command, Error: "the vault is locked"}
	}

	limit := activityDefaultLimit

	if raw := strings.TrimSpace(request.Args["limit"]); raw != "" {
		parsed, err := strconv.Atoi(raw)

		if err != nil || parsed <= 0 {
			return proto.Result{
				Command: request.Command,
				Error:   "limit must be a positive number",
			}
		}

		limit = parsed
	}

	if limit > activityMaxEntries {
		limit = activityMaxEntries
	}

	stored, err := d.readSecret(ctx, activityName)

	if err != nil {
		return failed(request.Command, err)
	}

	var entries []activityEntry

	if strings.TrimSpace(stored) != "" {
		if err := json.Unmarshal([]byte(stored), &entries); err != nil {
			return proto.Result{
				Command: request.Command,
				Error:   "the activity log is unreadable; clear it to continue",
			}
		}
	}

	total := len(entries)

	if len(entries) > limit {
		entries = entries[:limit]
	}

	view := proto.ActivityView{Total: total}

	lines := []string{strconv.Itoa(total) + " entr(y/ies)"}

	for _, entry := range entries {
		view.Entries = append(view.Entries, proto.ActivityEntry{
			At: entry.At, Command: entry.Command, Swap: entry.Swap,
			OK: entry.OK, Summary: entry.Summary, Error: entry.Error,
		})

		detail := entry.Summary

		if !entry.OK {
			detail = entry.Error
		}

		lines = append(lines, entry.At+"  "+entry.Command+"  "+entry.Swap+"  "+detail)
	}

	if len(entries) == 0 {
		lines = append(lines, "no entries")
	}

	return withView(proto.Result{
		Command: request.Command, OK: true, Text: strings.Join(lines, "\n"),
	}, view)
}

// clearActivity removes the log, which always works.
//
// It is the one command that must succeed on a log that will not open, because that is what
// the refusal tells a person to run.
func (d *vaultDomain) clearActivity(ctx context.Context, request proto.Request) proto.Result {
	if d.store == nil {
		return proto.Result{Command: request.Command, Error: "the vault is locked"}
	}

	if !strings.EqualFold(strings.TrimSpace(request.Args["confirm"]), "clear") {
		return proto.Result{
			Command: request.Command,
			Error:   "clearing the activity log needs confirm=clear",
		}
	}

	stored, err := d.readSecret(ctx, activityName)

	if err != nil {
		return failed(request.Command, err)
	}

	removed := 0

	if strings.TrimSpace(stored) != "" {
		var entries []activityEntry

		if json.Unmarshal([]byte(stored), &entries) == nil {
			removed = len(entries)
		} else {
			// An unreadable log is cleared rather than measured, and the reply says so by
			// reporting the one entry it cannot count.
			removed = 0
		}
	}

	if err := d.store.DeleteSecret(ctx, activityName); err != nil {
		return failed(request.Command, err)
	}

	return withView(proto.Result{
		Command: request.Command,
		OK:      true,
		Text:    "activity log cleared: " + strconv.Itoa(removed) + " entries removed",
	}, proto.ActivityView{Total: 0})
}
