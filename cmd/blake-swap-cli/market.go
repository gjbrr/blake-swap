package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
)

// The market read is rendered here rather than in the engine because it is
// presentation, and because the reference's output is a contract: the suites compare
// it, so the exact spacing and the exact places a blank line appears are part of what
// is being ported.

// renderMarket renders the human `market` listing: open offers only.
//
// The reference prints a header, a dashed rule, one row per offer, a count, and a
// sentence explaining what has been hidden. The column widths are the reference's,
// including the two that differ between the header and the rule.
func renderMarket(entries []marketflow.Entry, now int64) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("AVAILABLE BLAKE/BTC OFFERS\n")
	out.WriteString("==========================\n")
	out.WriteString("\n")

	if len(entries) == 0 {
		out.WriteString("No valid open offers found.\n")

		return out.String()
	}

	fmt.Fprintf(&out, "%-14s %-14s %-18s %-18s %s\n", "Offer", "Maker", "Giving", "Wanting", "Expires")
	fmt.Fprintf(&out, "%-14s %-14s %-16s %-16s %s\n",
		strings.Repeat("-", 12), strings.Repeat("-", 12),
		strings.Repeat("-", 14), strings.Repeat("-", 14),
		strings.Repeat("-", 8),
	)

	for _, entry := range entries {
		offer := entry.Offer

		give, want, err := offer.Direction.GiveWant(offer.BlakeAmount, amount.Sats(offer.BTCSats))

		if err != nil {
			continue
		}

		fmt.Fprintf(&out, "%-14s %-14s %-18s %-18s %s\n",
			truncate(offer.EventID, 12),
			truncate(offer.Maker, 12),
			give,
			want,
			formatTTL(offer.ExpiresAt-now),
		)
	}

	fmt.Fprintf(&out, "\n%d open offer(s)\n", len(entries))
	out.WriteString("\n")
	out.WriteString("Expired offers and valid maker cancellations are hidden.\n")

	return out.String()
}

// renderMarketState renders the canonical state listing: every offer the evidence
// describes, whatever state it is in.
func renderMarketState(entries []marketflow.Entry) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("BLAKE/BTC MARKET STATE\n")
	out.WriteString("======================\n")
	out.WriteString("\n")

	if len(entries) == 0 {
		out.WriteString("No valid market offers found.\n")

		return out.String()
	}

	for _, entry := range entries {
		offer := entry.Offer

		fmt.Fprintf(&out, "%s %s %s %s BLAKE %s sats\n",
			truncate(offer.EventID, 12),
			string(entry.State),
			string(offer.Direction),
			pythonFloat(blakeFloat(offer.BlakeAmount)),
			thousands(offer.BTCSats),
		)
	}

	return out.String()
}

// marketStateJSON renders the machine-readable boundary, with the reference's key set.
//
// A map is used rather than a struct for two reasons: the reference prints with
// sort_keys, which is what a map marshals to, and the field set is a contract with
// other people's tools rather than an internal type.
func marketStateJSON(entries []marketflow.Entry) (string, error) {
	offers := make([]map[string]any, 0, len(entries))

	for _, entry := range entries {
		offer := entry.Offer

		row := map[string]any{
			"event_id":   offer.EventID,
			"maker":      offer.Maker,
			"created_at": offer.CreatedAt,
			"expires_at": offer.ExpiresAt,
			// The reference emits the amount as a JSON number, and Python writes an
			// integral float as "1.0"; json.RawMessage keeps that spelling.
			"blake_amount": json.RawMessage(pythonFloat(blakeFloat(offer.BlakeAmount))),
			"btc_sats":     offer.BTCSats,
			"direction":    string(offer.Direction),
			"maker_role":   offer.MakerRole,
			"taker_role":   offer.TakerRole,
			"state":        string(entry.State),
		}

		row["cancel_event_id"] = nilIfEmpty(entry.CancelID)
		row["match_event_id"] = nilIfEmpty(entry.MatchID)

		offers = append(offers, row)
	}

	encoded, err := json.MarshalIndent(map[string]any{"offers": offers}, "", "  ")

	if err != nil {
		return "", err
	}

	return string(encoded) + "\n", nil
}

// nilIfEmpty renders an absent event id as JSON null, which is what the reference's
// dict.get(...) produces.
func nilIfEmpty(value string) any {
	if value == "" {
		return nil
	}

	return value
}

// truncate shortens a string to at most n bytes.
func truncate(value string, n int) string {
	if len(value) <= n {
		return value
	}

	return value[:n]
}

// formatTTL renders a remaining lifetime the way the reference does: the largest whole
// unit that fits, and zero seconds rather than a negative time.
func formatTTL(remaining int64) string {
	if remaining < 0 {
		remaining = 0
	}

	switch {
	case remaining < 60:
		return strconv.FormatInt(remaining, 10) + "s"
	case remaining < 3600:
		return strconv.FormatInt(remaining/60, 10) + "m"
	case remaining < 86400:
		return strconv.FormatInt(remaining/3600, 10) + "h"
	default:
		return strconv.FormatInt(remaining/86400, 10) + "d"
	}
}

// pythonFloat renders a float the way Python's str() does, which differs from Go's
// shortest form for whole numbers: Python writes 1.0, Go writes 1.
func pythonFloat(value float64) string {
	text := strconv.FormatFloat(value, 'g', -1, 64)

	if strings.ContainsAny(text, ".eE") {
		return text
	}

	return text + ".0"
}

// blakeFloat is the coin value of a BLAKE amount, as a float64.
//
// The amount is stored in base units and rendered through the exact decimal, so the
// float is only for the two places the reference's output is a float.
func blakeFloat(value amount.Blake) float64 {
	parsed, err := strconv.ParseFloat(value.Format(), 64)

	if err != nil {
		return 0
	}

	return parsed
}

// thousands groups an integer the way Python's `:,` does.
func thousands(n int64) string {
	negative := n < 0

	if negative {
		n = -n
	}

	digits := strconv.FormatInt(n, 10)

	var groups []string

	for len(digits) > 3 {
		groups = append([]string{digits[len(digits)-3:]}, groups...)
		digits = digits[:len(digits)-3]
	}

	groups = append([]string{digits}, groups...)
	text := strings.Join(groups, ",")

	if negative {
		return "-" + text
	}

	return text
}
