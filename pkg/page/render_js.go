//go:build js && wasm

package page

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"git.smesh.lol/tiny-spawn"

	"github.com/gjbrr/blake-swap/pkg/proto"
)

//
// This file renders. Every function here reads the page's state and the worker's views and
// writes the document; none of them decides anything about the protocol. What may be done
// next comes from reconcile, what an amount is comes from the worker as a string, and what
// went wrong comes from the engine's own sentence.
//

// setView makes one section visible.
func setView(state *app, view string) {
	for _, name := range []string{
		ViewSetup, ViewMarket, ViewSwaps, ViewSwap, ViewSettings, ViewCoordination, ViewDiagnostics,
	} {
		toggle("view-"+name, name == view)
	}

	state.view = view

	jsSetBodyAttr(ViewAttribute, view)
	setAttr("app", "data-active-view", view)

	for _, name := range []string{
		ViewSetup, ViewMarket, ViewSwaps, ViewSwap, ViewSettings, ViewCoordination, ViewDiagnostics,
	} {
		setAttr("nav-"+name, "aria-current", boolText(name == view))
	}

	// The Marketplace timer only runs while the Swaps view is open: a page that polled
	// the chains while sitting on the settings screen would be spending an operator's
	// node capacity on a question nobody asked.
	stopDashboardTimer(state)

	if view == ViewSwaps || view == ViewSwap {
		startDashboardTimer(state)
	}

	focusCommand("view-" + view)
}

// renderAck displays the worker's greeting. This is the visible proof the skeleton exists
// for: the text comes from a different wasm instance, over the channel shim, and lands in
// the document here.
func renderAck(e proto.Envelope) {
	if strings.TrimSpace(e.Body) == "" {
		setWorkerStatus("empty answer", "bad")

		return
	}

	setHello(e.Body)

	if e.AtMS > 0 {
		setRoundTrip(fmt.Sprintf("%d ms, seq %d", time.Now().UnixMilli()-e.AtMS, e.Seq))
	} else {
		setRoundTrip(fmt.Sprintf("seq %d", e.Seq))
	}

	setWorkerStatus("verified", "ok")
	setPageStatus("running", "ok")
	markVerified()
}

// setBusy names the command in flight.
func setBusy(state *app, text string) {
	jsSetBodyAttr(BusyAttribute, "true")
	setText(ElementBusy, text)
	toggle(ElementBusy, true)
}

// clearBusy returns the page to idle.
func clearBusy(state *app) {
	jsSetBodyAttr(BusyAttribute, "false")
	setText(ElementBusy, "")
	toggle(ElementBusy, false)
}

// showError displays a refusal inline, where it stays until dismissed or retried.
func showError(state *app, command, message string) {
	state.lastError = message

	label := command

	if label == "" {
		label = "error"
	}

	setText(ElementAlertCommand, label)
	setText(ElementAlertText, message)
	toggle(ElementAlert, true)

	jsSetBodyAttr(LastErrorAttr, message)
}

// clearError hides the inline error.
func clearError(state *app) {
	state.lastError = ""

	setText(ElementAlertText, "")
	toggle(ElementAlert, false)

	jsSetBodyAttr(LastErrorAttr, "")
}

// panelFor is where a command's text is rendered.
//
// It is a table rather than a guess at the command's prefix, because the two halves of a
// name are not always the panel's name: the vault's self-test reports into its own panel so
// "is the vault open" and "does sealing work" stay separate answers.
func panelFor(command string) string {
	switch {
	case command == "vault.selftest":
		return ElementVaultTest
	case strings.HasPrefix(command, "vault."), strings.HasPrefix(command, "state."):
		return ElementVault
	case strings.HasPrefix(command, "identity."):
		return ElementIdentity
	case strings.HasPrefix(command, "proxy."):
		return ElementProxy
	case strings.HasPrefix(command, "wallets."):
		return ElementWallets
	case command == "engine.status":
		return ElementEngineStatus
	case strings.HasPrefix(command, "engine."):
		return ElementEngine
	case strings.HasPrefix(command, "market."):
		return ElementMarket
	case strings.HasPrefix(command, "coord."):
		return ElementCoord
	}

	return ElementVault
}

// routeText writes a command's text into a panel and sets its state class.
func routeText(id, text, status string) {
	if id == "" {
		return
	}

	setText(id, text)
	setStateClass(id, status)
}

// routeSwapSummary shows a reconcile's text where the operator is looking.
func routeSwapSummary(state *app, result proto.Result) {
	routeText(ElementSwapActionOut, result.Text, "ok")
	routeText(ElementEngine, result.Text, "ok")
}

// markVaultOpen records the vault's state on the document.
func markVaultOpen(open bool) {
	if open {
		mark(VaultOpenAttribute)
	} else {
		unmark(VaultOpenAttribute)
	}
}

// setVaultChip renders the header's vault pill.
func setVaultChip(text, status string) {
	setText(ElementVaultChip, text)
	setStateClass(ElementVaultChip, status)
}

// setIdentitySecretOnce shows a generated secret, and only a generated one.
//
// An empty value clears the box, which is what every other operation does: the secret is
// displayed long enough to be written down, and no longer.
func setIdentitySecretOnce(secret string) {
	if secret == "" {
		setText(ElementIdentityValue, "")
		toggle(ElementIdentityOnce, false)

		return
	}

	setText(ElementIdentityValue, secret)
	toggle(ElementIdentityOnce, true)
}

// proxyInput is the proxy control's value, defaulting to this origin.
func proxyInput() string {
	if value := strings.TrimSpace(inputValue(InputProxyURL)); value != "" {
		return value
	}

	return documentOrigin()
}

// renderHeights renders the chain heights from a status result.
func renderHeights(result proto.Result) {
	var view proto.SetupView

	if err := proto.DecodeData(result, &view); err != nil {
		return
	}

	renderHeightsText(view.Heights)
}

// renderHeightsText renders two heights.
func renderHeightsText(heights proto.HeightView) {
	text := "BLAKE height " + strconv.FormatInt(heights.Blake, 10) +
		" · Bitcoin height " + strconv.FormatInt(heights.BTC, 10)

	setText(ElementDashboardHeights, text)
}

// ------------------------------------------------------------------ dashboard ---

// renderDashboard renders the Swaps screen.
func renderDashboard(state *app) {
	renderRows(ElementListActive, state.dashboard.Active, state.selected)
	renderRows(ElementListAttention, state.dashboard.Attention, state.selected)
	renderRows(ElementListSettled, state.dashboard.Settled, state.selected)
	renderRows(ElementListArchived, state.dashboard.Archived, state.selected)

	if len(state.dashboard.Unsolvable) > 0 {
		var lines []string

		for _, item := range state.dashboard.Unsolvable {
			lines = append(lines, item.SwapID+" "+item.Chain+": "+item.Reason)
		}

		setText(ElementDashboardUnsolvable, "HTLCs NOT SOLVABLE BY A LOADED WALLET\n"+strings.Join(lines, "\n"))
		toggle(ElementDashboardUnsolvable, true)
	} else {
		toggle(ElementDashboardUnsolvable, false)
	}

	renderHeightsText(state.dashboard.Heights)
}

// renderRows renders one dashboard group.
func renderRows(containerID string, rows []proto.DashboardRow, selected string) {
	clear(containerID)

	if len(rows) == 0 {
		put(containerID, elem("li").class("empty").text("Nothing here."))

		return
	}

	for _, row := range rows {
		item := elem("li")

		if row.SwapID == selected {
			item = item.attr("aria-current", "true")
		}

		badge := elem("span").class("badge").text(row.State)
		badge = badge.attr("data-tone", toneForState(row.State))

		amounts := elem("span").class("amounts").text(row.BlakeAmount + " BLAKE / " + row.BTCSats + " sats")
		next := elem("span").class("next").text(row.Outcome + " · " + row.NextAction)

		open := elem("button").attr("type", "button").
			onCmd(cmdOpenSwap + row.SwapID).text("Open")

		item = item.child(elem("span").class("id").text(row.SwapID)).
			child(badge).
			child(amounts).
			child(next).
			child(open)

		appendTo(containerID, item)
	}
}

// toneForState maps a checkpoint to a colour, so a state is never colour alone: the badge
// always carries the state's own name as its text.
func toneForState(value string) string {
	switch value {
	case "COMPLETE":
		return "ok"
	case "BLAKE_REFUNDED", "BTC_REFUNDED":
		return "dim"
	case "BLAKE_CLAIMED", "BTC_CLAIMED":
		return "wait"
	}

	return "wait"
}

// --------------------------------------------------------------------- market ---

// renderMarket renders the Market screen.
func renderMarket(state *app) {
	renderMarketRead(state.market)
	renderOpenOffers(state.market)
	renderMine(state.market)
	renderTakes(state.market)
	renderMatches(state.market)
}

// renderMarketRead renders the scan line and the offer count marker.
func renderMarketRead(market proto.MarketView) {
	text := market.Read

	if market.Err != "" {
		text = "relay error: " + market.Err
	}

	setText(ElementMarketRead, text)

	count := strconv.Itoa(len(market.Open))

	jsSetBodyAttr(MarketCountAttr, count)
}

// renderOpenOffers renders the public book.
func renderOpenOffers(market proto.MarketView) {
	clear(ElementMarketOpen)

	if len(market.Open) == 0 {
		toggle(ElementMarketOpenEmpty, true)

		return
	}

	toggle(ElementMarketOpenEmpty, false)

	for _, offer := range market.Open {
		row := elem("tr").
			child(elem("td").text(short(offer.EventID))).
			child(elem("td").text(short(offer.Maker))).
			child(elem("td").text(offer.Direction)).
			child(elem("td").class("num").text(offer.BlakeAmount)).
			child(elem("td").class("num").text(offer.BTCSats)).
			child(elem("td").text(ageText(offer.CreatedAt)))

		// Taking is a coordination act, and the maker's accept path that turns a take into
		// a swap is implemented here, so the button can finish what it starts.
		action := elem("td").child(elem("button").
			attr("type", "button").
			onCmd(cmdMarketTake + offer.EventID).
			text("Take"))

		row = row.child(action)

		appendTo(ElementMarketOpen, row)
	}
}

// renderMine renders this participant's own offers.
func renderMine(market proto.MarketView) {
	clear(ElementMarketMine)

	if len(market.Mine) == 0 {
		toggle(ElementMarketMineEmpty, true)

		return
	}

	toggle(ElementMarketMineEmpty, false)

	for _, offer := range market.Mine {
		actions := elem("td")

		if offer.State == "OPEN" {
			actions = actions.child(elem("button").attr("type", "button").
				onCmd(cmdMarketTakes + offer.EventID).text("Takes"))
			actions = actions.child(elem("button").attr("type", "button").
				onCmd(cmdMarketCancel + offer.EventID).text("Cancel"))
		} else {
			actions = actions.child(elem("button").attr("type", "button").
				onCmd(cmdMarketTakes + offer.EventID).text("Takes"))
		}

		appendTo(ElementMarketMine, elem("tr").
			child(elem("td").text(short(offer.EventID))).
			child(elem("td").text(offer.State)).
			child(elem("td").class("num").text(offer.BlakeAmount)).
			child(elem("td").class("num").text(offer.BTCSats)).
			child(elem("td").text(expiryText(offer.ExpiresAt))).
			child(actions))
	}
}

// renderTakes renders the valid takes on this participant's offers.
func renderTakes(market proto.MarketView) {
	clear(ElementMarketTakes)

	if len(market.Takes) == 0 {
		toggle(ElementMarketTakesEmpty, true)

		return
	}

	toggle(ElementMarketTakesEmpty, false)

	for _, take := range market.Takes {
		appendTo(ElementMarketTakes, elem("tr").
			child(elem("td").text(short(take.EventID))).
			child(elem("td").text(short(take.Taker))).
			child(elem("td").class("num").text(take.BlakeAmount)).
			child(elem("td").class("num").text(take.BTCSats)).
			child(elem("td").text(ageText(take.CreatedAt))).
			child(elem("td").child(elem("button").attr("type", "button").
				onCmd(cmdMarketAcceptTake+take.EventID).text("Accept take"))))
	}
}

// renderMatches renders the verified matches naming this participant.
func renderMatches(market proto.MarketView) {
	clear(ElementMarketMatches)

	if len(market.Matches) == 0 {
		toggle(ElementMarketMatchEmpty, true)

		return
	}

	toggle(ElementMarketMatchEmpty, false)

	for _, match := range market.Matches {
		trace := short(match.OfferID) + " -> " + short(match.TakeID)

		action := elem("td")

		if !match.Imported {
			action = action.child(elem("button").attr("type", "button").
				onCmd(cmdMarketAcceptMatch + match.EventID).text("Import match"))
		}

		appendTo(ElementMarketMatches, elem("tr").
			child(elem("td").text(short(match.EventID))).
			child(elem("td").text(match.SwapID)).
			child(elem("td").text(trace)).
			child(elem("td").text(ageText(match.CreatedAt))).
			child(action))
	}
}

// ---------------------------------------------------------------- settings ---

// renderSettings renders the settings summary.
//
// It shows only public facts: the identity's public key, the proxy address, the four wallet
// names and where the market reads from. No secret has a field here.
func renderSettings(state *app) {
	facts := elem("dl").class("facts")

	add := func(label, value string) {
		facts = facts.child(elem("dt").text(label)).child(elem("dd").text(orDash(value)))
	}

	add("identity", state.identity)
	add("relays", strings.Join(state.relays, ", "))
	add("proxy", state.proxy)
	add("BLAKE wallet", state.walletNames["blake"])
	add("BTC wallet", state.walletNames["btc"])
	add("BLAKE watch-only", state.walletNames["blake_watch"])
	add("BTC watch-only", state.walletNames["btc_watch"])

	put(ElementSettings, facts)
}

// renderWallets records and renders the wallet status.
func renderWallets(state *app, result proto.Result) {
	var wallets proto.WalletView

	if err := proto.DecodeData(result, &wallets); err != nil {
		routeText(ElementWallets, result.Text, "ok")

		return
	}

	state.walletNames = map[string]string{
		"blake":       wallets.Blake,
		"btc":         wallets.BTC,
		"blake_watch": wallets.BlakeWatch,
		"btc_watch":   wallets.BTCWatch,
	}

	fillInput(InputWalletBlake, wallets.Blake)
	fillInput(InputWalletBTC, wallets.BTC)
	fillInput(InputWalletBlakeWatch, wallets.BlakeWatch)
	fillInput(InputWalletBTCWatch, wallets.BTCWatch)

	routeText(ElementWallets, result.Text, "ok")
}

// fillInput puts a stored value into a field without overwriting what is being typed.
//
// The fields start empty, and the value a person typed is theirs: a refresh that replaced it
// mid-edit would be worse than a stale helper. An empty field is filled, and a focused field
// is never touched.
func fillInput(id, value string) {
	if value == "" || id == activeElementID() {
		return
	}

	if strings.TrimSpace(inputValue(id)) != "" {
		return
	}

	setInputValue(id, value)
}

// proxyValue reads the proxy address out of a status line.
func proxyValue(text string) string {
	value := strings.TrimSpace(strings.TrimPrefix(text, "proxy:"))
	value = strings.TrimSpace(strings.TrimPrefix(value, "default:"))

	if value == "absent" {
		return ""
	}

	return value
}

// relayValue reads the relay list out of a status line.
func relayValue(text string) string {
	value := strings.TrimSpace(strings.TrimPrefix(text, "relays:"))

	if value == "absent" {
		return ""
	}

	return value
}

// ---------------------------------------------------------------- swap detail ---

// renderSwap renders the selected swap.
func renderSwap(state *app, summary proto.SwapSummary) {
	setText(ElementSwapTitle, summary.SwapID)
	setText(ElementSwapRole, summary.Role)

	next := summary.NextAction

	if next == "" {
		next = "unknown"
	}

	setText(ElementSwapNext, next)

	setText(ElementSwapState, summary.State)
	setAttr(ElementSwapState, SwapStateAttr, summary.State)
	jsSetBodyAttr(SwapStateAttr, summary.State)
	jsSetBodyAttr(NextActionAttr, summary.NextAction)
	jsSetBodyAttr("data-selected-swap", summary.SwapID)
	setAttr(ElementSwapState, "data-tone", toneForState(summary.State))
	setAttr(ElementSwapNext, NextActionAttr, summary.NextAction)

	renderTerms(summary)
	renderChains(summary)
	renderTimeline(summary)
	renderTransactions(summary)
	renderSwapActions(state)

	setText(ElementSwapReason, summary.Reason)
}

// renderTerms renders the swap's trade and deadlines.
func renderTerms(summary proto.SwapSummary) {
	facts := elem("dl").class("facts")

	add := func(label, value string) {
		facts = facts.child(elem("dt").text(label)).child(elem("dd").text(value))
	}

	add("BLAKE", summary.BlakeAmount)
	add("BTC sats", summary.BTCSats)
	add("secret hash", summary.SecretHash)
	add("counterparty", orDash(summary.Counterparty))
	add("created", timeText(summary.CreatedAt))
	add("BTC timeout", strconv.FormatInt(summary.BTCTimeout, 10))
	add("BLAKE timeout", strconv.FormatInt(summary.BlakeTimeout, 10))
	add("funding (BLAKE)", outpointText(summary.Funding.BlakeTxID, summary.Funding.BlakeVout))
	add("funding (BTC)", outpointText(summary.Funding.BTCTxID, summary.Funding.BTCVout))
	add("negotiation", orDash(summary.Negotiation))

	put(ElementSwapTerms, facts)
}

// renderChains renders the reconciliation's per-chain observations.
func renderChains(summary proto.SwapSummary) {
	facts := elem("dl").class("facts")

	add := func(label, value string) {
		facts = facts.child(elem("dt").text(label)).child(elem("dd").text(value))
	}

	add("BLAKE", observationText(summary.Chains.Blake))
	add("BTC", observationText(summary.Chains.BTC))
	add("class", orDash(summary.DecisionClass))
	add("next", orDash(summary.NextAction))

	put(ElementSwapChains, facts)
}

// observationText renders one chain observation.
func observationText(observed proto.ObservationView) string {
	text := observed.Status

	if observed.Branch != "" {
		text += " / " + observed.Branch
	}

	if observed.TxID != "" {
		text += " " + short(observed.TxID)
	}

	return text
}

// renderTimeline renders the protocol's steps.
func renderTimeline(summary proto.SwapSummary) {
	clear(ElementSwapTimeline)

	for _, step := range summary.Timeline {
		item := elem("li").text(step.State).
			attr("data-reached", boolText(step.Reached)).
			attr("data-current", boolText(step.Current)).
			attr("data-refund", boolText(step.Refund))

		appendTo(ElementSwapTimeline, item)
	}
}

// renderTransactions renders the six settlement anchors.
func renderTransactions(summary proto.SwapSummary) {
	facts := elem("dl").class("facts")

	add := func(label string, tx proto.TxView) {
		facts = facts.child(elem("dt").text(label)).child(elem("dd").text(txText(tx)))
	}

	add("BTC claim", summary.Settlement.BTCClaim)
	add("BTC refund", summary.Settlement.BTCRefund)
	add("BLAKE claim", summary.Settlement.BlakeClaim)
	add("BLAKE refund", summary.Settlement.BlakeRefund)

	if summary.Settlement.ReorgClaimTxID != "" || summary.Settlement.ReorgRefundTxID != "" {
		facts = facts.child(elem("dt").text("rolled back")).child(elem("dd").
			text(orDash(summary.Settlement.ReorgClaimTxID) + " / " + orDash(summary.Settlement.ReorgRefundTxID)))
	}

	put(ElementSwapTxs, facts)
}

// txText renders one transaction and whether it is confirmed.
func txText(tx proto.TxView) string {
	if tx.TxID == "" {
		return "—"
	}

	if tx.Confirmed {
		return tx.TxID + " (confirmed)"
	}

	return tx.TxID + " (pending)"
}

// alwaysAvailable is the actions that are safe to offer at any checkpoint: they prove,
// read or announce, and the engine refuses them if they do not apply.
var alwaysAvailable = map[string]bool{
	//
	// The coordination and verification actions the engine validates for itself. A
	// reconciliation recommends one *economic* step; it cannot recommend "publish the
	// offer", because whether an offer has been published is not a protocol fact the
	// record holds, and an unpublished offer would then have no button at all.
	//
	"publish-offer": true, "accept-offer": true, "receive-accept": true,
	"refresh-timeouts": true,
	"verify-blake":     true, "verify-btc": true, "watch-htlc": true,
	"sync-funding": true, "sync-settlement": true, "extract-secret": true,
	"publish-blake-funded": true, "receive-blake-funded": true,
	"publish-btc-funded": true, "receive-btc-funded": true,
	"publish-btc-claimed": true, "receive-btc-claimed": true,
	"publish-blake-claimed": true, "receive-blake-claimed": true,
	"publish-btc-refunded": true, "receive-btc-refunded": true,
	"recovery-plan": true,
}

// actionOrder is the order the buttons appear in.
var actionOrder = []string{
	"publish-offer", "accept-offer", "receive-accept",
	"fund-blake", "fund-btc", "verify-blake", "verify-btc",
	"claim-btc", "claim-blake", "refund-btc", "refund-blake",
	"extract-secret", "complete", "sync-funding", "sync-settlement",
	"resume", "watch-htlc", "refresh-timeouts",
	"publish-blake-funded", "receive-blake-funded",
	"publish-btc-funded", "receive-btc-funded",
	"publish-btc-claimed", "receive-btc-claimed",
	"publish-blake-claimed", "receive-blake-claimed",
	"publish-btc-refunded", "receive-btc-refunded",
}

// renderSwapActions renders the action buttons.
//
// Only the reconciliation's recommendation is enabled among the actions that move money or
// advance the protocol. The engine would refuse a wrong one anyway; the point of gating
// here is that a stale view must not invite an action the engine is about to refuse, and an
// operator should not have to know which of thirty buttons is the safe one.
func renderSwapActions(state *app) {
	clear(ElementSwapActions)

	if !state.haveSummary {
		appendTo(ElementSwapActions, elem("span").class("muted").text("Open a swap to see its actions."))

		return
	}

	recommended := nextActionButton[state.summary.NextAction]

	for _, name := range actionOrder {
		spec, known := swapActions[name]

		if !known {
			continue
		}

		enabled := alwaysAvailable[name] || name == recommended

		button := elem("button").attr("type", "button").text(spec.label)

		if enabled {
			button = button.onCmd(cmdSwapAction + name)

			if name == recommended && spec.economic {
				button = button.class("primary")
			}
		} else {
			button = button.attr("disabled", "disabled").
				attr("title", "not the recommended next step; reconcile recommends "+
					orDash(state.summary.NextAction))
		}

		appendTo(ElementSwapActions, button)
	}
}

// --------------------------------------------------------------- confirmation ---

// beginSwapAction opens the confirmation for one swap action.
func beginSwapAction(state *app, name string) {
	spec, known := swapActions[name]

	if !known {
		consoleLog("page: unknown swap action " + name)

		return
	}

	if spec.selected && state.selected == "" {
		showError(state, spec.command, "open a swap first")

		return
	}

	request := proto.Request{Command: spec.command}

	if spec.selected {
		request.Args = map[string]string{"swap_id": state.selected}
	}

	openConfirmation(state, spec, request, state.selected, confirmationFacts(state))
}

// beginMarketPublish opens the confirmation for advertising an offer.
//
// Publishing is coordination rather than economics, but it is signed and public, so the
// dialog shows the exact terms that will be advertised.
func beginMarketPublish(state *app) {
	blake := strings.TrimSpace(inputValue(InputMarketBlake))
	sats := strings.TrimSpace(inputValue(InputMarketSats))
	minutes := strings.TrimSpace(inputValue(InputMarketMinute))
	direction := strings.TrimSpace(inputValue(InputMarketDirection))

	spec := actionSpec{
		command: "market.publish",
		label:   "Publish offer",
		what: "Sign and publish a public market advertisement with these terms." +
			" It contains no secret hash and moves no funds.",
	}

	request := proto.Request{
		Command: "market.publish",
		Args: map[string]string{
			"blake": blake, "sats": sats, "minutes": minutes, "direction": direction,
		},
	}

	facts := [][2]string{
		{"BLAKE", blake},
		{"BTC sats", sats},
		{"lifetime", orDash(minutes) + " minutes"},
		{"direction", orDash(direction)},
	}

	openConfirmation(state, spec, request, "", facts)
}

// beginAcceptOffer opens the confirmation for accepting an offer by id.
//
// This is the path a Bob takes when Alice sends him a swap id out of band: there is no
// record yet, so the acceptance is the command that creates one.
func beginAcceptOffer(state *app) {
	swapID := strings.TrimSpace(inputValue(InputAcceptSwap))
	alice := strings.TrimSpace(inputValue(InputAcceptAlice))

	if swapID == "" {
		showError(state, "engine.accept-offer", "enter the swap id Alice sent you")

		return
	}

	spec := actionSpec{
		command: "engine.accept-offer",
		label:   "Accept offer",
		what: "Accept this offer as Bob: pin the maker, adopt the terms, and publish a signed" +
			" acceptance. No funds move.",
	}

	facts := [][2]string{
		{"swap", swapID},
		{"maker", orDash(alice)},
	}

	if alice == "" {
		facts = append(facts, [2]string{"warning", "without the maker's key nothing is pinned"})
	}

	request := proto.Request{
		Command: "engine.accept-offer",
		Args:    map[string]string{"swap_id": swapID, "expected_alice": alice},
	}

	openConfirmation(state, spec, request, swapID, facts)
}

// beginCoord opens the confirmation for one coordination message.
func beginCoord(state *app, kind, direction string) {
	if state.selected == "" {
		showError(state, "coord."+kind+"."+direction, "open a swap first")

		return
	}

	label := "Announce " + kind

	if direction == "receive" {
		label = "Receive " + kind
	}

	spec := actionSpec{
		command: "coord." + kind + "." + direction,
		label:   label,
		what: "Publish the signed " + kind + " message for this swap." +
			" Its body is built by the worker from the sealed record; this page supplies none of it.",
	}

	if direction == "receive" {
		spec.what = "Read and validate the " + kind + " message from the counterparty this swap pins."
	}

	request := proto.Request{
		Command: spec.command,
		Args:    map[string]string{"swap_id": state.selected},
	}

	openConfirmation(state, spec, request, state.selected, confirmationFacts(state))
}

// confirmationFacts is what the dialog shows about the swap.
func confirmationFacts(state *app) [][2]string {
	if !state.haveSummary {
		return nil
	}

	return [][2]string{
		{"swap", state.summary.SwapID},
		{"BLAKE", state.summary.BlakeAmount},
		{"BTC sats", state.summary.BTCSats},
		{"counterparty", orDash(state.summary.Counterparty)},
		{"state", state.summary.State},
		{"role", orDash(state.summary.Role)},
	}
}

// openConfirmation shows the dialog for one pending action.
func openConfirmation(state *app, spec actionSpec, request proto.Request, swapID string, facts [][2]string) {
	state.focusBeforeDialog = activeElementID()

	state.pending = &pendingAction{spec: spec, request: request, swapID: swapID, detail: facts}

	body := elem("div")

	body = body.child(elem("p").class("what").text(spec.what))

	factList := elem("dl").class("facts")

	for _, fact := range facts {
		factList = factList.child(elem("dt").text(fact[0])).child(elem("dd").text(fact[1]))
	}

	body = body.child(factList)

	if spec.economic {
		body = body.child(elem("p").class("consequence").text(
			"This moves funds. Nothing can undo a broadcast transaction."))
	}

	logo := spec.label

	if request.Args["swap_id"] != "" {
		logo += " · " + request.Args["swap_id"]
	}

	dialogOpen(logo, body)
}

// cancelConfirmation closes the dialog and returns focus to the button that opened it.
func cancelConfirmation(state *app) {
	state.pending = nil

	dialogClose()

	if state.focusBeforeDialog != "" {
		focusElement(state.focusBeforeDialog)
		state.focusBeforeDialog = ""
	}
}

// confirmPending performs the action the dialog is asking about.
func confirmPending(state *app, link tinyspawn.Duplex[proto.Envelope], seq *uint32, refresh *queue) {
	pending := state.pending

	state.pending = nil

	dialogClose()

	if pending == nil {
		return
	}

	if pending.spec.selected && pending.request.Args["swap_id"] == "" {
		pending.request.Args = map[string]string{"swap_id": pending.swapID}
	}

	startCommand(state, link, seq, pending.request)

	refresh.next(state, link, seq)
}

// ------------------------------------------------------------------- opening ---

// openSwap selects a swap and asks for its reconciliation.
func openSwap(state *app, link tinyspawn.Duplex[proto.Envelope], seq *uint32, swapID string, refresh *queue) {
	if swapID == "" {
		swapID = strings.TrimSpace(inputValue(InputSwapID))
	}

	if swapID == "" {
		showError(state, "engine.show", "enter a swap id")

		return
	}

	state.selected = swapID
	state.haveSummary = false

	setText(ElementSwapTitle, swapID)
	setText(ElementSwapState, "loading")
	setAttr(ElementSwapState, SwapStateAttr, "")
	setText(ElementSwapNext, "loading")
	setAttr(ElementSwapNext, NextActionAttr, "")

	queueSelection(state, link, seq, refresh)

	queueCommand(refresh, state, link, seq, proto.Request{
		Command: "engine.reconcile",
		Args:    map[string]string{"swap_id": swapID},
	})

	setView(state, ViewSwap)

	if refresh != nil {
		refresh.next(state, link, seq)
	}
}

// ------------------------------------------------------------------- helpers ---

// splitList splits a comma or space separated list.
func splitList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t'
	})

	out := make([]string, 0, len(fields))

	for _, field := range fields {
		if trimmed := strings.TrimSpace(field); trimmed != "" {
			out = append(out, trimmed)
		}
	}

	return out
}

// short truncates an id for a table cell.
func short(id string) string {
	if len(id) <= 12 {
		return id
	}

	return id[:12]
}

// orDash renders an empty value as a dash.
func orDash(value string) string {
	if value == "" {
		return "—"
	}

	return value
}

// outpointText renders a recorded outpoint.
func outpointText(txid string, vout int32) string {
	if txid == "" {
		return "—"
	}

	return txid + ":" + strconv.FormatInt(int64(vout), 10)
}

// timeText renders a Unix timestamp as an age and a clock time.
func timeText(seconds int64) string {
	if seconds <= 0 {
		return "—"
	}

	at := time.Unix(seconds, 0).UTC()

	return at.Format("2006-01-02 15:04") + " UTC"
}

// ageText renders how long ago an event was created.
func ageText(seconds int64) string {
	if seconds <= 0 {
		return "—"
	}

	elapsed := time.Now().Unix() - seconds

	if elapsed < 0 {
		return "in the future"
	}

	return durationText(elapsed) + " ago"
}

// expiryText renders when an offer stops being open.
func expiryText(expires int64) string {
	if expires <= 0 {
		return "—"
	}

	remaining := expires - time.Now().Unix()

	if remaining <= 0 {
		return "expired"
	}

	return "in " + durationText(remaining)
}

// durationText renders a duration in the largest unit that fits.
func durationText(seconds int64) string {
	switch {
	case seconds < 60:
		return strconv.FormatInt(seconds, 10) + "s"
	case seconds < 3600:
		return strconv.FormatInt(seconds/60, 10) + "m"
	case seconds < 86400:
		return strconv.FormatInt(seconds/3600, 10) + "h"
	}

	return strconv.FormatInt(seconds/86400, 10) + "d"
}

// boolText renders a boolean as an attribute value.
func boolText(value bool) string {
	if value {
		return "true"
	}

	return "false"
}

// startDashboardTimer refreshes the dashboard every 25 seconds while a swap view is open.
//
// The ticker lives on the app value and the domain loop selects on its channel, so there is
// no goroutine and no package-level state: the refresh is a case in the one loop that owns
// the page. A nil channel blocks forever, which is exactly "not ticking".
func startDashboardTimer(state *app) {
	if state.ticker != nil {
		return
	}

	state.ticker = time.NewTicker(25 * time.Second)
	state.tick = state.ticker.C
}

// stopDashboardTimer stops the refresh.
func stopDashboardTimer(state *app) {
	if state.ticker == nil {
		return
	}

	state.ticker.Stop()
	state.ticker = nil
	state.tick = nil
}

// ------------------------------------------------------------------ market acts ---

// beginMarketTake opens the confirmation for taking an offer.
func beginMarketTake(state *app, offerID string) {
	offer := findOffer(state.market.Open, offerID)

	spec := actionSpec{
		command: "market.take",
		label:   "Take offer",
		what: "Publish a signed take for this offer. It is a public statement of interest" +
			" and moves no funds; the maker then accepts or ignores it.",
	}

	facts := [][2]string{
		{"offer", offerID},
		{"maker", orDash(offer.Maker)},
		{"BLAKE", orDash(offer.BlakeAmount)},
		{"BTC sats", orDash(offer.BTCSats)},
	}

	request := proto.Request{
		Command: "market.take",
		Args:    map[string]string{"offer_id": offerID},
	}

	openConfirmation(state, spec, request, "", facts)
}

// beginMarketCancel opens the confirmation for withdrawing one of this participant's offers.
func beginMarketCancel(state *app, offerID string) {
	offer := findOffer(state.market.Mine, offerID)

	spec := actionSpec{
		command: "market.cancel",
		label:   "Cancel offer",
		what: "Publish a signed cancellation of your own offer. It cannot be un-cancelled;" +
			" publish a new offer instead.",
	}

	facts := [][2]string{
		{"offer", offerID},
		{"BLAKE", orDash(offer.BlakeAmount)},
		{"BTC sats", orDash(offer.BTCSats)},
		{"state", orDash(offer.State)},
	}

	request := proto.Request{
		Command: "market.cancel",
		Args:    map[string]string{"offer_id": offerID},
	}

	openConfirmation(state, spec, request, "", facts)
}

// beginMarketAcceptTake opens the confirmation for turning a take into a swap.
func beginMarketAcceptTake(state *app, takeID string) {
	spec := actionSpec{
		command: "market.accept-take",
		label:   "Accept take",
		what: "Accept this take: create the swap, pin both identities, and publish the" +
			" protocol offer and the market match that binds them. It creates the swap but" +
			" moves no funds — funding stays a separate explicit action.",
	}

	facts := [][2]string{
		{"take", takeID},
		{"offer", orDash(state.marketOfferID)},
	}

	request := proto.Request{
		Command: "market.accept-take",
		Args:    map[string]string{"take_id": takeID},
	}

	openConfirmation(state, spec, request, "", facts)
}

// beginMarketAcceptMatch opens the confirmation for importing a match as the taker.
func beginMarketAcceptMatch(state *app, matchID string) {
	spec := actionSpec{
		command: "market.accept-match",
		label:   "Import match",
		what: "Validate this match against the offer and take it names, then create your" +
			" side of the swap and bind the negotiation to it. No funds move.",
	}

	request := proto.Request{
		Command: "market.accept-match",
		Args:    map[string]string{"match_id": matchID},
	}

	openConfirmation(state, spec, request, "", [][2]string{{"match", matchID}})
}

// findOffer reads one offer out of a list, zero when it is not there.
func findOffer(offers []proto.MarketOffer, eventID string) proto.MarketOffer {
	for _, offer := range offers {
		if offer.EventID == eventID {
			return offer
		}
	}

	return proto.MarketOffer{}
}
