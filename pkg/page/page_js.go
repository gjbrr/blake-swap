//go:build js && wasm

package page

import (
	"strconv"
	"strings"
	"time"

	"git.smesh.lol/tiny-spawn"

	"github.com/gjbrr/blake-swap/pkg/proto"
)

// Chans is the page domain's fixed channel set. Per the tiny-spawn rules it is
// bound at spawn and never changes: the link carries worker messages and UI
// carries commands from the document's controls.
type Chans struct {
	Link tinyspawn.Duplex[proto.Envelope]
	UI   <-chan string
	Stop tinyspawn.Breaker
}

// UICmd is the command a DOM control sends. The string crosses from JavaScript,
// which is the realm boundary, so keep the vocabulary tiny and named rather than
// accepting arbitrary text.
//
// A command may carry one argument after a colon — a swap id, an offer id, a market
// action — because a list row's button is created from data and there is no input field
// to read the value from. The command is still this page's own vocabulary; nothing
// outside can inject one.
type UICmd = string

// The views. A name is both the body's data-view value and the button that opens it.
const (
	ViewSetup        = "setup"
	ViewMarket       = "market"
	ViewSwaps        = "swaps"
	ViewSwap         = "swap"
	ViewActivity     = "activity"
	ViewSettings     = "settings"
	ViewCoordination = "coordination"
	ViewDiagnostics  = "diagnostics"
)

// The rest of the vocabulary. Each name is a control in index.html and a case in
// handleUI; a control with no case is logged and ignored rather than guessing.
const (
	UICmdPing         UICmd = "ping"
	UICmdVaultTest    UICmd = "vault-selftest"
	UICmdDismissError UICmd = "dismiss-error"

	UICmdVaultCreate UICmd = "vault-create"
	UICmdVaultUnlock UICmd = "vault-unlock"
	UICmdVaultLock   UICmd = "vault-lock"
	UICmdVaultExport UICmd = "vault-export"
	UICmdVaultImport UICmd = "vault-import"

	UICmdIdentityStatus   UICmd = "identity-status"
	UICmdIdentitySet      UICmd = "identity-set"
	UICmdIdentityGenerate UICmd = "identity-generate"

	UICmdProxyStatus UICmd = "proxy-status"
	UICmdProxySet    UICmd = "proxy-set"

	UICmdWalletsStatus UICmd = "wallets-status"
	UICmdWalletsSet    UICmd = "wallets-set"

	UICmdEngineStatus    UICmd = "engine-status"
	UICmdEngineList      UICmd = "engine-list"
	UICmdEngineShow      UICmd = "engine-show"
	UICmdEngineReconcile UICmd = "engine-reconcile"
	UICmdEngineNext      UICmd = "engine-next"
	UICmdEngineDashboard UICmd = "engine-dashboard"
	UICmdEngineResumeAll UICmd = "engine-resume-all"
	UICmdEngineNew       UICmd = "engine-new"
	UICmdEngineAccept    UICmd = "engine-accept-offer"

	UICmdMarketRelays    UICmd = "market-relays"
	UICmdMarketSetRelays UICmd = "market-set-relays"
	UICmdMarketList      UICmd = "market-list"
	UICmdMarketMatches   UICmd = "market-matches"
	UICmdMarketPublish   UICmd = "market-publish"

	UICmdCoordOpen    UICmd = "coord-open"
	UICmdCoordSend    UICmd = "coord-send"
	UICmdCoordReceive UICmd = "coord-receive"
	UICmdCoordMode    UICmd = "coord-mode"
	UICmdCoordSetMode UICmd = "coord-set-mode"

	UICmdSwapOpen        UICmd = "swap-open"
	UICmdSwapRefresh     UICmd = "swap-refresh"
	UICmdSwapClose       UICmd = "swap-close"
	UICmdSwapRecovery    UICmd = "swap-recovery-plan"
	UICmdSettingsRefresh UICmd = "settings-refresh"
	UICmdActivityRefresh UICmd = "activity-refresh"
	UICmdActivityClear   UICmd = "activity-clear"

	UICmdConfirmOK     UICmd = "confirm-ok"
	UICmdConfirmCancel UICmd = "confirm-cancel"
)

// The prefixes a data-driven control may carry. The suffix is an id or an action name
// this page itself wrote into the document.
const (
	cmdViewPrefix        = "view-"
	cmdSwapAction        = "swap-action:"
	cmdMarketTake        = "market-take:"
	cmdMarketCancel      = "market-cancel:"
	cmdMarketAcceptTake  = "market-accept-take:"
	cmdMarketAcceptMatch = "market-accept-match:"
	cmdMarketAcceptOffer = "market-accept-reverse-offer:"
	cmdMarketIgnoreMatch = "market-ignore-match:"
	cmdWalletCreate      = "wallet-create:"
	cmdMarketTakes       = "market-takes:"
	cmdOpenSwap          = "open-swap:"
	cmdCoordSend         = "coord-send:"
	cmdCoordReceive      = "coord-receive:"
)

// The element ids are part of the page contract: index.html renders them and the
// headless check in scripts/verify-browser.py reads them.
const (
	ElementPageStatus      = "page-status"
	ElementWorkerStatus    = "worker-status"
	ElementHello           = "hello"
	ElementRoundTrip       = "round-trip"
	ElementVault           = "vault"
	ElementVaultBackup     = "vault-backup-out"
	ElementVaultTest       = "vaulttest"
	ElementVaultChip       = "vault-chip"
	ElementIdentity        = "identity"
	ElementProxy           = "proxy"
	ElementEngine          = "engine"
	ElementMarket          = "market"
	ElementCoord           = "coord"
	ElementWallets         = "wallets"
	ElementWalletSlots     = "wallet-slots"
	ElementWalletSlotEmpty = "wallet-slots-empty"
	ElementWalletAction    = "wallets-action-out"
	ElementActivityRows    = "activity-rows"
	ElementActivityEmpty   = "activity-empty"
	ElementActivityOut     = "activity-out"
	ElementEngineStatus    = "engine-status-out"
	ElementMarketRelays    = "market-relays-out"
	ElementAlert           = "alert"
	ElementAlertCommand    = "alert-command"
	ElementAlertText       = "alert-text"
	ElementBusy            = "busy-line"

	ElementIdentityOnce  = "identity-secret-once"
	ElementIdentityValue = "identity-secret-value"

	ElementDashboardHeights    = "dashboard-heights"
	ElementDashboardUnsolvable = "dashboard-unsolvable"
	ElementListActive          = "list-active"
	ElementListAttention       = "list-attention"
	ElementListSettled         = "list-settled"
	ElementListArchived        = "list-archived"
	ElementEngineNewOut        = "engine-new-out"

	ElementSwapTitle     = "swap-title"
	ElementSwapState     = "swap-state-badge"
	ElementSwapRole      = "swap-role"
	ElementSwapNext      = "swap-next"
	ElementSwapTerms     = "swap-terms"
	ElementSwapChains    = "swap-chains"
	ElementSwapReason    = "swap-reason"
	ElementSwapTimeline  = "swap-timeline"
	ElementSwapTxs       = "swap-txs"
	ElementSwapActions   = "swap-actions"
	ElementSwapActionOut = "swap-action-out"
	ElementSwapRecovery  = "swap-recovery-out"

	ElementMarketRead       = "market-read"
	ElementMarketOpen       = "market-open-rows"
	ElementMarketMine       = "market-mine-rows"
	ElementMarketTakes      = "market-takes-rows"
	ElementMarketMatches    = "market-matches-rows"
	ElementMarketOpenEmpty  = "market-open-empty"
	ElementMarketMineEmpty  = "market-mine-empty"
	ElementMarketTakesEmpty = "market-takes-empty"
	ElementMarketMatchEmpty = "market-matches-empty"
	ElementSettings         = "settings-summary"
	ElementCoordModeOut     = "coord-mode-out"

	DialogConfirm = "confirm-dialog"
	DialogTitle   = "confirm-title"
	DialogBody    = "confirm-body"
)

// The inputs a command's arguments are read from.
const (
	InputIdentitySecret    = "identity-secret"
	InputConfirmPassphrase = "confirm-passphrase"
	InputImportFile        = "import-vault-file"
	InputProxyURL          = "proxy-url"
	InputSwapID            = "swap-id"
	InputPassphrase        = "passphrase"
	InputWalletBlake       = "wallet-blake"
	InputWalletBTC         = "wallet-btc"
	InputWalletBlakeWatch  = "wallet-blake-watch"
	InputWalletBTCWatch    = "wallet-btc-watch"

	InputCoordBlake = "coord-blake"
	InputCoordSats  = "coord-sats"
	InputCoordPeer  = "coord-peer"
	InputCoordKind  = "coord-kind"
	InputCoordMode  = "coord-mode-setting"

	InputRelays          = "relays"
	InputMarketBlake     = "market-blake"
	InputMarketSats      = "market-sats"
	InputMarketMinute    = "market-minutes"
	InputMarketDirection = "market-direction"

	InputAcceptSwap  = "accept-swap-id"
	InputAcceptAlice = "accept-alice-pub"
	ElementAcceptOut = "engine-accept-out"

	InputNewBlake = "new-blake"
	InputNewSats  = "new-sats"
	InputNewPeer  = "new-peer"
)

// Document markers. A marker means the thing actually happened in this browser, which is
// the only reason to have one; the headless check asserts markers rather than prose.
const (
	VerifiedAttribute         = "data-hello-verified"
	VaultVerifiedAttribute    = "data-vault-verified"
	VaultOpenAttribute        = "data-vault-open"
	CoordVerifiedAttribute    = "data-coord-verified"
	IdentityVerifiedAttribute = "data-identity-set"
	ProxyVerifiedAttribute    = "data-proxy-set"
	EngineVerifiedAttribute   = "data-engine-verified"
	MarketVerifiedAttribute   = "data-market-verified"

	// The client's own markers.
	BusyAttribute        = "data-busy"
	ActivityCountAttr    = "data-activity-count"
	ActivityEmptyAttr    = "data-activity-empty"
	ActivityEntryAttr    = "data-activity-entry"
	ActivityCommandAttr  = "data-activity-command"
	ActivityOKAttr       = "data-activity-ok"
	ViewAttribute   = "data-view"
	SwapStateAttr   = "data-swap-state"
	NextActionAttr  = "data-next-action"
	MarketCountAttr = "data-market-count"
	LastErrorAttr   = "data-last-error"
)

// actionSpec is one act the UI may perform, and what the confirmation says about it.
//
// The page does not decide whether an action is allowed — reconcile does — but it does
// decide what the person is told before it runs, and that is the difference between a
// button and a trap.
type actionSpec struct {
	// command is the worker command.
	command string
	// label is the button and dialog title.
	label string
	// what says what will happen, in one sentence.
	what string
	// economic marks an action that moves money, which always needs a second gesture.
	economic bool
	// swaps says the command acts on the selected swap.
	selected bool
}

// swapActions is every action the detail view can offer, keyed by the name its button
// carries.
var swapActions = map[string]actionSpec{
	"publish-offer":         {"engine.publish-offer", "Publish offer", "Publish the signed offer so the counterparty can accept it.", false, true},
	"receive-accept":        {"engine.receive-accept", "Receive acceptance", "Read the counterparty's signed acceptance and pin their keys.", false, true},
	"fund-blake":            {"engine.fund-blake", "Fund BLAKE", "Build, sign and broadcast the BLAKE HTLC funding transaction.", true, true},
	"fund-btc":              {"engine.fund-btc", "Fund BTC", "Build, sign and broadcast the Bitcoin HTLC funding transaction.", true, true},
	"verify-blake":          {"engine.verify-blake", "Verify BLAKE funding", "Prove the BLAKE HTLC on chain matches this swap's terms.", false, true},
	"verify-btc":            {"engine.verify-btc", "Verify BTC funding", "Prove the Bitcoin HTLC on chain matches this swap's terms.", false, true},
	"claim-btc":             {"engine.claim-btc", "Claim BTC", "Spend Bitcoin with the secret. This reveals the secret on Bitcoin.", true, true},
	"claim-blake":           {"engine.claim-blake", "Claim BLAKE", "Spend BLAKE with the revealed secret.", true, true},
	"refund-btc":            {"engine.refund-btc", "Refund BTC", "Spend Bitcoin's timelock branch after it matures.", true, true},
	"refund-blake":          {"engine.refund-blake", "Refund BLAKE", "Spend BLAKE's timelock branch after it matures.", true, true},
	"extract-secret":        {"engine.extract-secret", "Extract secret", "Read the preimage out of the Bitcoin claim into the sealed record.", false, true},
	"complete":              {"engine.complete", "Complete", "Close the swap once the BLAKE claim is confirmed and still active.", false, true},
	"sync-funding":          {"engine.sync-funding", "Sync funding", "Recover funding state from confirmed chain history.", false, true},
	"sync-settlement":       {"engine.sync-settlement", "Sync settlement", "Recover settlement state from confirmed chain history.", false, true},
	"resume":                {"engine.resume", "Resume", "Do the one action the reconciliation recommends.", true, true},
	"watch-htlc":            {"engine.watch-htlc", "Watch HTLC", "Import the HTLC descriptor into the watch-only wallet.", false, true},
	"refresh-timeouts":      {"engine.refresh-timeouts", "Refresh timeouts", "Recompute the deadlines of an unaccepted offer.", false, true},
	"publish-blake-funded":  {"engine.publish-blake-funded", "Announce BLAKE funded", "Publish the signed BLAKE funding announcement.", false, true},
	"receive-blake-funded":  {"engine.receive-blake-funded", "Receive BLAKE funded", "Read and apply the BLAKE funding announcement.", false, true},
	"publish-btc-funded":    {"engine.publish-btc-funded", "Announce BTC funded", "Publish the signed Bitcoin funding announcement.", false, true},
	"receive-btc-funded":    {"engine.receive-btc-funded", "Receive BTC funded", "Read and apply the Bitcoin funding announcement.", false, true},
	"publish-btc-claimed":   {"engine.publish-btc-claimed", "Announce BTC claimed", "Publish the signed Bitcoin claim announcement.", false, true},
	"receive-btc-claimed":   {"engine.receive-btc-claimed", "Receive BTC claimed", "Read and apply the Bitcoin claim announcement.", false, true},
	"publish-blake-claimed": {"engine.publish-blake-claimed", "Announce BLAKE claimed", "Publish the signed BLAKE claim announcement.", false, true},
	"receive-blake-claimed": {"engine.receive-blake-claimed", "Receive BLAKE claimed", "Read and apply the BLAKE claim announcement.", false, true},
	"publish-btc-refunded":  {"engine.publish-btc-refunded", "Announce BTC refunded", "Publish the signed Bitcoin refund announcement.", false, true},
	"receive-btc-refunded":  {"engine.receive-btc-refunded", "Receive BTC refunded", "Read and apply the Bitcoin refund announcement.", false, true},
}

// nextActionButton is the action reconcile recommends for each of its actions.
//
// It exists so the detail view has one obvious primary button, and so a person who has
// never read the protocol can do the safe thing without choosing. The action still comes
// from reconcile; this only names the button that performs it.
var nextActionButton = map[string]string{
	"publish-offer":    "publish-offer",
	"receive-accept":   "receive-accept",
	"fund-blake":       "fund-blake",
	"fund-btc":         "fund-btc",
	"claim-btc":        "claim-btc",
	"claim-blake":      "claim-blake",
	"refund-btc":       "refund-btc",
	"refund-blake":     "refund-blake",
	"complete":         "complete",
	"sync-funding":     "sync-funding",
	"sync-settlement":  "sync-settlement",
	"watch-htlc":       "watch-htlc",
	"refresh-timeouts": "refresh-timeouts",
}

// app is the page's whole state.
//
// It is one value owned by the domain loop and passed by pointer to the renderers. There
// is no package-level state: a second domain in the same instance (a test, a future split)
// must not see this one's.
type app struct {
	// view is the visible section.
	view string
	// selected is the swap the detail view is showing.
	selected string
	// dashboard and market are the last snapshots.
	dashboard proto.DashboardView
	market    proto.MarketView
	// summary is the last reconcile of the selected swap.
	summary proto.SwapSummary
	// haveSummary reports that summary describes something.
	haveSummary bool
	// pending is the action the dialog is asking about.
	pending *pendingAction
	// focusBeforeDialog is the element id focus returns to.
	focusBeforeDialog string
	// inflight is the sequence number of the command in flight, zero when idle.
	inflight uint32
	// busy reports whether a command is in flight.
	busy bool
	// lastError is the last refusal, shown inline until dismissed.
	lastError string
	// unlocked mirrors the vault's state, so the page can start its own sequence.
	unlocked bool
	// identity is the public key, empty when none is configured.
	identity string
	// restore is the swap id read back from the sealed selection.
	restore string
	// commandLog is the last commands this page sent, for diagnosis.
	commandLog []string
	// ticker drives the light dashboard refresh; tick is its channel, nil when off.
	ticker *time.Ticker
	tick   <-chan time.Time
	// marketOfferID is the offer whose takes are on screen, so an acceptance can name it.
	marketOfferID string
	// relays, proxy and walletNames are the configuration the Settings view shows.
	relays      []string
	proxy       string
	walletNames map[string]string
	// walletSlots is the four rows the Settings view shows, with the node's answer about
	// each. It comes from wallets.status, so the page never guesses which wallet exists.
	walletSlots []proto.WalletSlotView
	// activity is the last page of the sealed log.
	activity proto.ActivityView
}

// pendingAction is one act waiting for its second gesture.
type pendingAction struct {
	spec    actionSpec
	request proto.Request
	swapID  string
	// detail is what the confirmation shows, beyond the spec's sentence.
	detail [][2]string
	// prompt names the extra controls the dialog shows. An empty prompt is the ordinary
	// yes or no.
	prompt string
}

// The prompts a confirmation can add to its body. They are strings rather than values
// because the dialog is built by the renderer from this marker.
const (
	// promptImportFile asks for the backup's own passphrase and the file it belongs to.
	promptImportFile = "import-file"
)

// Domain is the page's event loop. It runs until Stop is closed.
func Domain(_ struct{}, ch Chans) {
	defer setPageStatus("stopped", "bad")

	state := &app{view: ViewSetup}

	seq := uint32(0)

	sendHello := func() {
		seq++

		ch.Link.Send(proto.Envelope{
			Kind: proto.Hello,
			Seq:  seq,
			Body: "hello from the page",
			AtMS: time.Now().UnixMilli(),
		})
	}

	setPageStatus("waiting for the worker", "wait")
	setView(state, ViewSetup)
	sendHello()

	// refresh is the sequence the page runs after unlocking: the configuration, the
	// chains, the market and the dashboard. One at a time, because the worker runs one
	// command per message.
	refresh := newQueue()

	for {
		select {
		case e := <-ch.Link.Recv():
			switch e.Kind {
			case proto.WorkerReady:
				markWorkerReady()
				setWorkerStatus("connected", "wait")

			case proto.HelloAck:
				renderAck(e)

			case proto.CommandResult:
				finishCommand(state, ch.Link, &seq, e, refresh)

			default:
				consoleLog("page: ignoring unexpected kind " + string(e.Kind))
			}

		case cmd := <-ch.UI:
			handleUI(state, ch.Link, &seq, cmd, refresh)

		case <-state.tick:
			//
			// A light refresh while a swap view is open. It is a read, so it is queued
			// behind whatever the operator is doing rather than interrupting it.
			//
			queueCommand(refresh, state, ch.Link, &seq, proto.Request{Command: "engine.dashboard"})

		case <-ch.Stop.Done():
			return
		}
	}
}

// handleUI turns one control's command into work.
//
// It is the page's only dispatcher: navigation changes the view, everything else becomes a
// worker request, and the actions that move money are held until the dialog confirms them.
func handleUI(state *app, link tinyspawn.Duplex[proto.Envelope], seq *uint32, cmd string, refresh *queue) {
	cmd = strings.TrimSpace(cmd)

	if cmd == "" {
		return
	}

	// The dialog's own two buttons are answered whatever the busy state: a cancel must
	// always work.
	switch cmd {
	case UICmdConfirmCancel:
		cancelConfirmation(state)

		return

	case UICmdConfirmOK:
		confirmPending(state, link, seq, refresh)

		return

	case UICmdDismissError:
		clearError(state)

		return
	}

	// One command at a time. A second tap while one is in flight is ignored rather than
	// queued: the operator asked twice for the same thing, and doing it twice is worse
	// than doing it once.
	if state.busy {
		consoleLog("page: ignoring " + cmd + " while another command is in flight")

		return
	}

	if cmd == UICmdSwapClose {
		setView(state, ViewSwaps)
		queueCommand(refresh, state, link, seq, proto.Request{Command: "engine.dashboard"})

		return
	}

	if strings.HasPrefix(cmd, cmdViewPrefix) {
		setView(state, strings.TrimPrefix(cmd, cmdViewPrefix))
		afterView(state, link, seq, refresh)

		return
	}

	if cmd == UICmdPing {
		state.busy = true
		state.inflight = 0
		setBusy(state, "pinging the worker")

		seq0 := *seq + 1
		*seq = seq0

		link.Send(proto.Envelope{
			Kind: proto.Hello, Seq: seq0, Body: "hello from the page",
			AtMS: time.Now().UnixMilli(),
		})

		return
	}

	if cmd == UICmdVaultTest {
		startCommand(state, link, seq, proto.Request{Command: "vault.selftest"})

		return
	}

	//
	// The diagnostics panel sends coord-send / coord-receive and names the message type in a
	// field, because that is the form a hand-driven handshake takes. The swap view sends the
	// same commands with the type in the command, because its buttons are made from the
	// record's own message list.
	//
	if cmd == UICmdCoordSend || cmd == UICmdCoordReceive {
		kind := strings.ToLower(strings.TrimSpace(inputValue(InputCoordKind)))

		direction := "send"

		if cmd == UICmdCoordReceive {
			direction = "receive"
		}

		beginCoord(state, kind, direction)

		return
	}

	// A swap action from the detail view: look it up, then ask for confirmation.
	if name, found := strings.CutPrefix(cmd, cmdSwapAction); found {
		beginSwapAction(state, name)

		return
	}

	if cmd == UICmdSwapOpen {
		openSwap(state, link, seq, strings.TrimSpace(inputValue(InputSwapID)), refresh)

		return
	}

	if swapID, found := strings.CutPrefix(cmd, cmdOpenSwap); found {
		openSwap(state, link, seq, swapID, refresh)

		return
	}

	if offerID, found := strings.CutPrefix(cmd, cmdMarketTakes); found {
		state.marketOfferID = offerID

		startCommand(state, link, seq, proto.Request{
			Command: "market.takes",
			Args:    map[string]string{"offer_id": offerID},
		})

		return
	}

	if offerID, found := strings.CutPrefix(cmd, cmdMarketTake); found {
		beginMarketTake(state, offerID)

		return
	}

	if offerID, found := strings.CutPrefix(cmd, cmdMarketCancel); found {
		beginMarketCancel(state, offerID)

		return
	}

	if takeID, found := strings.CutPrefix(cmd, cmdMarketAcceptTake); found {
		beginMarketAcceptTake(state, takeID)

		return
	}

	if matchID, found := strings.CutPrefix(cmd, cmdMarketAcceptMatch); found {
		beginMarketAcceptMatch(state, matchID)

		return
	}

	if matchID, found := strings.CutPrefix(cmd, cmdMarketAcceptOffer); found {
		beginMarketAcceptReverseOffer(state, matchID)

		return
	}

	if matchID, found := strings.CutPrefix(cmd, cmdMarketIgnoreMatch); found {
		beginMarketIgnoreMatch(state, matchID)

		return
	}

	if slot, found := strings.CutPrefix(cmd, cmdWalletCreate); found {
		beginWalletCreate(state, slot)

		return
	}

	if cmd == UICmdActivityClear {
		beginActivityClear(state)

		return
	}

	if cmd == UICmdVaultExport {
		beginVaultExport(state)

		return
	}

	if cmd == UICmdVaultImport {
		beginVaultImport(state)

		return
	}

	if cmd == UICmdEngineAccept {
		beginAcceptOffer(state)

		return
	}

	if cmd == UICmdMarketPublish {
		beginMarketPublish(state)

		return
	}

	if kind, found := strings.CutPrefix(cmd, cmdCoordSend); found {
		beginCoord(state, kind, "send")

		return
	}

	if kind, found := strings.CutPrefix(cmd, cmdCoordReceive); found {
		beginCoord(state, kind, "receive")

		return
	}

	if request, ok := uiRequest(state, cmd); ok {
		startCommand(state, link, seq, request)

		return
	}

	consoleLog("page: ignoring unknown UI command " + cmd)
}

// afterView runs the reads a view needs when it opens.
func afterView(state *app, link tinyspawn.Duplex[proto.Envelope], seq *uint32, refresh *queue) {
	switch state.view {
	case ViewSwaps:
		queueCommand(refresh, state, link, seq, proto.Request{Command: "engine.dashboard"})

	case ViewMarket:
		queueCommand(refresh, state, link, seq, proto.Request{Command: "market.list"})

	case ViewSettings:
		queueCommand(refresh, state, link, seq, proto.Request{Command: "wallets.status"})

	case ViewActivity:
		queueCommand(refresh, state, link, seq, proto.Request{Command: "activity.list"})
	}
}

// bootstrap is the sequence the page runs once the vault is open.
//
// The origin is offered first, so an app served by the app server needs no proxy setting;
// then the configuration, the chains, the market and the dashboard. Each is one command,
// and the queue sends them one at a time.
func bootstrap(state *app, link tinyspawn.Duplex[proto.Envelope], seq *uint32, refresh *queue) {
	if origin := documentOrigin(); origin != "" {
		queueCommand(refresh, state, link, seq, proto.Request{
			Command: "proxy.default",
			Args:    map[string]string{"url": origin},
		})
	}

	for _, request := range []proto.Request{
		{Command: "identity.status"},
		{Command: "proxy.status"},
		{Command: "wallets.status"},
		{Command: "engine.status"},
		{Command: "engine.dashboard"},
		{Command: "market.relays"},
		{Command: "ui.get"},
	} {
		queueCommand(refresh, state, link, seq, request)
	}
}

// uiRequest maps a simple control to its worker request.
//
// Arguments are read from the document at send time, so a value the person just typed
// cannot be stale in a closure.
func uiRequest(state *app, cmd UICmd) (proto.Request, bool) {
	switch cmd {
	case UICmdVaultCreate:
		return proto.Request{
			Command: "vault.create",
			Args:    map[string]string{"passphrase": inputValue(InputPassphrase)},
		}, true

	case UICmdVaultUnlock:
		return proto.Request{
			Command: "vault.unlock",
			Args:    map[string]string{"passphrase": inputValue(InputPassphrase)},
		}, true

	case UICmdVaultLock:
		return proto.Request{Command: "vault.lock"}, true

	case UICmdIdentityStatus:
		return proto.Request{Command: "identity.status"}, true

	case UICmdIdentitySet:
		return proto.Request{
			Command: "identity.set",
			Args:    map[string]string{"secret": inputValue(InputIdentitySecret)},
		}, true

	case UICmdIdentityGenerate:
		return proto.Request{Command: "identity.generate"}, true

	case UICmdProxyStatus:
		return proto.Request{Command: "proxy.status"}, true

	case UICmdProxySet:
		return proto.Request{
			Command: "proxy.set",
			Args:    map[string]string{"url": proxyInput()},
		}, true

	case UICmdWalletsStatus:
		return proto.Request{Command: "wallets.status"}, true

	case UICmdWalletsSet:
		return proto.Request{
			Command: "wallets.set",
			Args: map[string]string{
				"blake":       inputValue(InputWalletBlake),
				"btc":         inputValue(InputWalletBTC),
				"blake_watch": inputValue(InputWalletBlakeWatch),
				"btc_watch":   inputValue(InputWalletBTCWatch),
			},
		}, true

	case UICmdEngineStatus:
		return proto.Request{Command: "engine.status"}, true

	case UICmdEngineList:
		return proto.Request{Command: "engine.list"}, true

	case UICmdEngineShow:
		return proto.Request{
			Command: "engine.show", Args: map[string]string{"swap_id": inputValue(InputSwapID)},
		}, true

	case UICmdEngineReconcile:
		return proto.Request{
			Command: "engine.reconcile", Args: map[string]string{"swap_id": inputValue(InputSwapID)},
		}, true

	case UICmdEngineNext:
		return proto.Request{
			Command: "engine.next", Args: map[string]string{"swap_id": inputValue(InputSwapID)},
		}, true

	case UICmdEngineDashboard:
		return proto.Request{Command: "engine.dashboard"}, true

	case UICmdEngineResumeAll:
		return proto.Request{Command: "engine.resume-all"}, true

	case UICmdEngineNew:
		return proto.Request{
			Command: "engine.new",
			Args: map[string]string{
				"blake": inputValue(InputNewBlake),
				"sats":  inputValue(InputNewSats),
				"peer":  inputValue(InputNewPeer),
			},
		}, true

	case UICmdMarketRelays:
		return proto.Request{Command: "market.relays"}, true

	case UICmdMarketSetRelays:
		return proto.Request{
			Command: "market.set-relays",
			Args:    map[string]string{"relays": inputValue(InputRelays)},
		}, true

	case UICmdMarketList:
		return proto.Request{Command: "market.list"}, true

	case UICmdMarketMatches:
		// The matches of a negotiation are published by the maker, so the other side has to
		// ask for them; the page refreshes automatically after its own actions, and this is
		// the button for everything a peer did.
		return proto.Request{Command: "market.matches"}, true

	case UICmdCoordMode:
		return proto.Request{Command: "coord.mode"}, true

	case UICmdCoordSetMode:
		return proto.Request{
			Command: "coord.set-mode",
			Args:    map[string]string{"mode": inputValue(InputCoordMode)},
		}, true

	case UICmdCoordOpen:
		return proto.Request{
			Command: "coord.open",
			Args: map[string]string{
				"blake":         inputValue(InputCoordBlake),
				"sats":          inputValue(InputCoordSats),
				"nostr_bob_pub": inputValue(InputCoordPeer),
			},
		}, true

	case UICmdActivityRefresh:
		return proto.Request{Command: "activity.list"}, true

	case UICmdActivityClear:
		return proto.Request{
			Command: "activity.clear",
			Args:    map[string]string{"confirm": "clear"},
		}, true

	case UICmdSwapRefresh, UICmdSettingsRefresh:
		if state.selected != "" {
			return proto.Request{
				Command: "engine.reconcile",
				Args:    map[string]string{"swap_id": state.selected},
			}, true
		}

		return proto.Request{Command: "engine.dashboard"}, true

	case UICmdSwapClose:
		return proto.Request{Command: "engine.dashboard"}, true

	case UICmdSwapRecovery:
		return proto.Request{
			Command: "engine.recovery-plan",
			Args:    map[string]string{"swap_id": state.selected},
		}, true
	}

	return proto.Request{}, false
}

// startCommand sends one request and marks the page busy.
func startCommand(state *app, link tinyspawn.Duplex[proto.Envelope], seq *uint32, request proto.Request) {
	body, err := proto.EncodeRequest(request)

	if err != nil {
		showError(state, request.Command, "could not encode the request: "+err.Error())

		return
	}

	*seq = *seq + 1

	state.busy = true
	state.inflight = *seq

	setBusy(state, "running "+request.Command)
	jsSetBodyAttr("data-last-command", request.Command)

	// A bounded log of what this page has sent, for a check that needs to know which
	// command produced a result. It carries command names, never arguments.
	state.commandLog = append(state.commandLog, request.Command)

	if len(state.commandLog) > 24 {
		state.commandLog = state.commandLog[len(state.commandLog)-24:]
	}

	jsSetBodyAttr("data-command-log", strings.Join(state.commandLog, ","))

	//
	// The count of commands actually sent is on the document so a check can prove that a
	// double click sent one command rather than two. It is incremented here, which is the
	// only place a command leaves the page.
	//
	jsSetBodyAttr("data-commands-sent", strconv.Itoa(commandsSent+1))
	commandsSent++

	link.Send(proto.Envelope{
		Kind: proto.Command,
		Seq:  *seq,
		Body: body,
		AtMS: time.Now().UnixMilli(),
	})
}

// commandsSent counts the commands this page has sent. It is the domain's own evidence that
// one tap is one command; it never changes a decision.
var commandsSent int

// finishCommand handles one result: the busy state clears, the view updates, and a
// refusal is shown verbatim.
func finishCommand(
	state *app, link tinyspawn.Duplex[proto.Envelope], seq *uint32,
	e proto.Envelope, refresh *queue,
) {
	result, err := proto.DecodeResult(e.Body)

	if err != nil {
		state.busy = false
		state.inflight = 0
		clearBusy(state)
		showError(state, "", "the result could not be decoded: "+err.Error())

		return
	}

	state.busy = false
	state.inflight = 0
	clearBusy(state)

	if !result.OK {
		//
		// A refusal changes nothing, so nothing is refreshed. Refreshing after a failure
		// would be worse than wasted work: a failed read of a swap that does not exist would
		// queue another read of it, and the page would loop for as long as it stayed open.
		//
		showError(state, result.Command, result.Error)
		routeText(panelFor(result.Command), "ERROR: "+result.Error, "bad")

		return
	}

	//
	// A refusal stays on screen until it is dismissed or the command is retried. The
	// refresh that follows every action must not wipe it: a check, and a person, need to
	// read the sentence the engine wrote, and the automatic read that succeeds a second
	// later says nothing about the action that failed.
	//
	if !isAutomaticRead(result.Command) {
		clearError(state)
	}

	applyResult(state, result)

	// Creating a swap opens it: the next thing an operator does is fund it, and making them
	// find it in a list first would be a step that exists only for the page's convenience.
	if (result.Command == "engine.new" || result.Command == "engine.accept-offer") &&
		state.selected != "" {
		setView(state, ViewSwap)

		queueSelection(state, link, seq, refresh)
	}

	// The swap the person last opened is restored after a reload, which is the one thing
	// that makes continuing after a reload easy.
	if result.Command == "ui.get" && state.restore != "" && state.selected == "" {
		state.selected = state.restore
		state.restore = ""

		setText(ElementSwapTitle, state.selected)

		queueCommand(refresh, state, link, seq, proto.Request{
			Command: "engine.reconcile",
			Args:    map[string]string{"swap_id": state.selected},
		})
	}

	if result.Command == "vault.unlock" || result.Command == "vault.create" {
		bootstrap(state, link, seq, refresh)
	}

	// The wallet rows are the node's answer, so they are re-read after anything that could
	// have changed one: a different name, or a wallet that did not exist a moment ago.
	if result.Command == "wallets.set" || result.Command == "wallets.create" {
		queueCommand(refresh, state, link, seq, proto.Request{Command: "wallets.status"})
	}

	// An identity is what makes the engine usable: without one, every chain read is refused
	// with "no participant identity". Setting or generating one therefore refreshes the
	// reads that depend on it, rather than leaving a page that says it cannot reach a chain.
	if result.Command == "identity.set" || result.Command == "identity.generate" {
		queueCommand(refresh, state, link, seq, proto.Request{Command: "identity.status"})
		queueCommand(refresh, state, link, seq, proto.Request{Command: "engine.status"})
		queueCommand(refresh, state, link, seq, proto.Request{Command: "wallets.status"})
	}

	// Every action refreshes what it changed: the selected swap's reconciliation and the
	// dashboard. It is one command at a time, so they are queued rather than sent at once.
	if isAction(result.Command) {
		refresh.selected(state, link, seq)
		queueCommand(refresh, state, link, seq, proto.Request{Command: "engine.dashboard"})
	}

	// A market act changes the book, the takes or the matches, so all of them are re-read:
	// a Market screen that did not show the offer it just advertised, or the take it just
	// accepted, would look like a failed command.
	switch result.Command {
	case "market.publish", "market.take", "market.cancel":
		queueCommand(refresh, state, link, seq, proto.Request{Command: "market.list"})

	case "market.accept-take", "market.accept-match", "market.accept-reverse-match",
		"market.accept-reverse-offer", "market.ignore-match":
		// The matches list changes on every one of these, and an ignore only changes the
		// matches: the book and this participant's own offers are untouched, so re-reading
		// them would be two reads for a fact that did not change.
		queueCommand(refresh, state, link, seq, proto.Request{Command: "market.matches"})

		if result.Command == "market.ignore-match" {
			break
		}

		queueCommand(refresh, state, link, seq, proto.Request{Command: "market.list"})
	}

	refresh.next(state, link, seq)
}

// queue is the page's own small command queue.
//
// It exists because the rule is one command in flight: a refresh after an action is two
// reads, and a page that sent both would have two results racing to describe the same
// swap.
type queue struct {
	pending []proto.Request
}

// newQueue returns an empty queue.
func newQueue() *queue { return &queue{} }

// queueCommand adds a request to the queue and starts it if nothing is running.
func queueCommand(q *queue, state *app, link tinyspawn.Duplex[proto.Envelope], seq *uint32, request proto.Request) {
	q.pending = append(q.pending, request)

	q.next(state, link, seq)
}

// next starts the next queued request, if the page is idle.
func (q *queue) next(state *app, link tinyspawn.Duplex[proto.Envelope], seq *uint32) {
	if state.busy {
		return
	}

	if len(q.pending) == 0 {
		return
	}

	request := q.pending[0]
	q.pending = q.pending[1:]

	startCommand(state, link, seq, request)
}

// selected queues a reconcile of the selected swap, when there is one.
func (q *queue) selected(state *app, link tinyspawn.Duplex[proto.Envelope], seq *uint32) {
	if state.selected == "" {
		return
	}

	queueCommand(q, state, link, seq, proto.Request{
		Command: "engine.reconcile",
		Args:    map[string]string{"swap_id": state.selected},
	})
}

// isAutomaticRead reports whether a command is one the page runs by itself.
func isAutomaticRead(command string) bool {
	switch command {
	case "engine.status", "engine.list", "engine.show", "engine.reconcile",
		"engine.next", "engine.dashboard", "wallets.status", "identity.status",
		"proxy.status", "market.list", "market.relays", "ui.get", "activity.list":
		return true
	}

	return false
}

// isAction reports whether a command changed something, which is what decides whether the
// page refreshes afterwards.
func isAction(command string) bool {
	switch command {
	case "engine.status", "engine.list", "engine.show", "engine.reconcile",
		"engine.next", "engine.dashboard", "engine.recovery-plan", "wallets.status",
		"vault.status", "vault.selftest", "identity.status", "proxy.status",
		"market.list", "market.relays", "market.set-relays", "coord.mode", "ui.get", "ui.set",
		"activity.list":
		return false
	}

	return strings.HasPrefix(command, "engine.") || strings.HasPrefix(command, "market.")
}

// queueSelection records the opened swap in the sealed vault.
func queueSelection(state *app, link tinyspawn.Duplex[proto.Envelope], seq *uint32, refresh *queue) {
	if state.selected == "" {
		return
	}

	queueCommand(refresh, state, link, seq, proto.Request{
		Command: "ui.set",
		Args:    map[string]string{"selected": state.selected},
	})
}

// applyResult routes a successful result to the view it belongs to.
func applyResult(state *app, result proto.Result) {
	applyMarker(result.Command)

	switch result.Command {
	case "vault.selftest":
		routeText(ElementVaultTest, result.Text, "ok")
		mark(VaultVerifiedAttribute)

	case "ui.get":
		state.restore = strings.TrimSpace(strings.TrimPrefix(result.Text, "selected:"))

	case "ui.set":
		// A selection that could not be stored is not a reason to stop working; it is a
		// convenience, and the swap itself is in the vault already.

	case "vault.status", "vault.crypto":
		routeText(ElementVault, result.Text, "ok")

	case "vault.export":
		applyVaultExport(state, result)

	case "vault.import":
		applyVaultImport(state, result)

	case "vault.create", "vault.unlock":
		setInputValue(InputPassphrase, "")
		state.unlocked = true
		markVaultOpen(true)
		setVaultChip("unlocked", "ok")
		routeText(panelFor(result.Command), result.Text, "ok")

	case "vault.lock":
		setText(ElementBusy, "")
		state.unlocked = false
		state.selected = ""
		state.haveSummary = false
		markVaultOpen(false)
		setVaultChip("locked", "wait")
		routeText(panelFor(result.Command), result.Text, "ok")

	case "identity.set":
		routeText(panelFor(result.Command), result.Text, "ok")

		//
		// The secret has been sealed by the worker, so the field it was typed into is
		// cleared: a value left in an input is a secret on screen, in the accessibility
		// tree and in whatever a screenshot keeps.
		//
		setInputValue(InputIdentitySecret, "")
		setIdentitySecretOnce("")

	case "identity.generate":
		var view proto.IdentityView

		if err := proto.DecodeData(result, &view); err != nil {
			routeText(panelFor(result.Command), result.Text, "ok")
		} else {
			state.identity = view.Public
			routeText(result.Command, "identity generated: "+view.Public, "ok")
			setIdentitySecretOnce(view.Secret)
		}

	case "identity.status":
		state.identity = strings.TrimSpace(strings.TrimPrefix(result.Text, "identity:"))
		routeText(panelFor(result.Command), result.Text, "ok")
		renderSettings(state)

	case "proxy.status":
		state.proxy = proxyValue(result.Text)
		routeText(panelFor(result.Command), result.Text, "ok")
		fillInput(InputProxyURL, state.proxy)
		renderSettings(state)

	case "proxy.set":
		state.proxy = proxyValue(result.Text)
		routeText(panelFor(result.Command), result.Text, "ok")
		renderSettings(state)

	case "proxy.default":
		renderSettings(state)

	case "wallets.status":
		renderWallets(state, result)
		renderSettings(state)

	case "wallets.create":
		// Its own panel: the status read that follows replaces the wallets panel, and what
		// a create did ("created" or "already exists") is the one thing that read cannot
		// say.
		routeText(ElementWalletAction, result.Text, "ok")

	case "wallets.set":
		routeText(ElementWallets, result.Text, "ok")
		renderSettings(state)

	case "engine.status":
		renderHeights(result)
		routeText(ElementEngineStatus, result.Text, "ok")
		routeText("engine", result.Text, "ok")

	case "engine.list":
		routeText(ElementEngine, result.Text, "ok")

	case "engine.show":
		routeText(ElementEngine, result.Text, "ok")
		applySummary(state, result)

	case "engine.reconcile":
		applySummary(state, result)
		routeSwapSummary(state, result)

	case "engine.next":
		applyNext(state, result)

	case "engine.dashboard":
		applyDashboard(state, result)

	case "engine.resume-all":
		routeText("engine", result.Text, "ok")

	case "engine.new":
		applyAction(state, result, ElementEngineNewOut)

	case "engine.accept-offer":
		applyAction(state, result, ElementAcceptOut)

	case "engine.recovery-plan":
		routeText(ElementSwapRecovery, result.Text, "ok")

	case "market.relays":
		routeText(ElementMarketRelays, result.Text, "ok")
		routeText(ElementMarket, result.Text, "ok")
		state.relays = splitList(relayValue(result.Text))
		fillInput(InputRelays, strings.Join(state.relays, ", "))
		renderSettings(state)

	case "market.set-relays":
		state.relays = splitList(relayValue(result.Text))
		routeText(ElementMarketRelays, result.Text, "ok")
		renderSettings(state)

	case "market.publish", "market.take", "market.cancel", "market.accept-take",
		"market.accept-match", "market.accept-reverse-match", "market.accept-reverse-offer",
		"market.ignore-match":
		applyAction(state, result, ElementMarket)

	case "market.list":
		applyMarket(state, result)

	case "market.takes":
		applyTakes(state, result)

	case "market.matches":
		applyMatches(state, result)

	case "activity.list":
		applyActivity(state, result)

	case "activity.clear":
		applyActivityCleared(state, result)

	case "coord.mode":
		routeText(ElementCoordModeOut, result.Text, "ok")
		routeText(ElementCoord, result.Text, "ok")

	case "coord.set-mode":
		routeText(ElementCoordModeOut, result.Text, "ok")

	case "coord.open":
		routeText(ElementCoord, result.Text, "ok")

		// Opening a record from the coordination panel makes it the record the panel's
		// announce and receive buttons act on, which is what a person expects from the
		// command that just printed its id.
		if swapID := fieldValue(result.Text, "swap id:"); swapID != "" {
			state.selected = swapID
			setInputValue(InputSwapID, swapID)
		}

	default:
		if strings.HasPrefix(result.Command, "coord.") {
			routeText(ElementCoord, result.Text, "ok")

			return
		}

		applyAction(state, result, ElementSwapActionOut)
	}
}

// applyMarker records the document markers a successful command proves.
//
// A marker means the thing actually worked in this browser, which is the only reason to
// have one; it is set here, once, rather than by each panel's renderer.
func applyMarker(command string) {
	switch command {
	case "identity.set", "identity.generate":
		mark(IdentityVerifiedAttribute)

	case "proxy.set":
		mark(ProxyVerifiedAttribute)

	case "engine.status":
		mark(EngineVerifiedAttribute)

	case "market.list":
		mark(MarketVerifiedAttribute)
	}

	if strings.HasPrefix(command, "coord.") && strings.HasSuffix(command, ".receive") {
		mark(CoordVerifiedAttribute)
	}
}

// applySummary records the selected swap's summary and marks the view.
func applySummary(state *app, result proto.Result) {
	var summary proto.SwapSummary

	if err := proto.DecodeData(result, &summary); err != nil {
		consoleLog("page: " + result.Command + " returned no swap view: " + err.Error())

		return
	}

	state.summary = summary
	state.haveSummary = true
	state.selected = summary.SwapID

	renderSwap(state, summary)
}

// applyNext records the reconciliation's recommendation.
func applyNext(state *app, result proto.Result) {
	var action proto.ActionView

	if err := proto.DecodeData(result, &action); err != nil {
		return
	}

	if action.SwapID != "" && action.SwapID == state.selected {
		state.summary.NextAction = action.Kind
		state.summary.DecisionClass = action.DecisionClass
		state.summary.Reason = action.Reason

		renderSwapActions(state)
	}
}

// applyDashboard renders the Swaps screen.
func applyDashboard(state *app, result proto.Result) {
	var dashboard proto.DashboardView

	if err := proto.DecodeData(result, &dashboard); err != nil {
		consoleLog("page: dashboard returned no view: " + err.Error())

		return
	}

	state.dashboard = dashboard

	renderDashboard(state)
	renderHeightsText(dashboard.Heights)
}

// applyMarket renders the Market screen.
func applyMarket(state *app, result proto.Result) {
	var market proto.MarketView

	if err := proto.DecodeData(result, &market); err != nil {
		consoleLog("page: market returned no view: " + err.Error())

		return
	}

	state.market = market

	renderMarket(state)
	routeText(ElementMarket, result.Text, "ok")
}

// applyTakes records the takes of one offer and renders them.
func applyTakes(state *app, result proto.Result) {
	var market proto.MarketView

	if err := proto.DecodeData(result, &market); err != nil {
		return
	}

	state.market.Takes = market.Takes

	renderTakes(state.market)
	routeText(ElementMarket, result.Text, "ok")
}

// applyMatches records the matches naming this participant and renders them.
func applyMatches(state *app, result proto.Result) {
	var market proto.MarketView

	if err := proto.DecodeData(result, &market); err != nil {
		return
	}

	state.market.Matches = market.Matches

	renderMatches(state.market)
	routeText(ElementMarket, result.Text, "ok")
}

// applyAction renders what an economic or coordination command did.
func applyAction(state *app, result proto.Result, panelID string) {
	var action proto.ActionView

	if err := proto.DecodeData(result, &action); err != nil {
		routeText(panelID, result.Text, "ok")
		consoleLog("page: " + result.Command + " returned no action view: " + err.Error())

		return
	}

	routeText(panelID, describeActionText(action), "ok")

	// The id a published event got is a fact the document carries, so a check can read it
	// without parsing the panel's prose.
	if action.Message != "" && strings.HasPrefix(result.Command, "market.") {
		jsSetBodyAttr("data-last-event-id", action.Message)
	}

	if action.SwapID != "" {
		state.selected = action.SwapID

		// The record's state moved, so the detail view is refreshed by the caller.
		state.haveSummary = false
	}
}

// actionHeading is the human name of an action.
//
// It is a table in Go rather than a heading in the page's JavaScript: the label is part of
// what the client says, and the page's job is to place text, not to invent it.
func actionHeading(kind string) string {
	switch kind {
	case "publish-market-offer":
		return "MARKET OFFER PUBLISHED"
	case "take-market-offer":
		return "MARKET OFFER TAKEN"
	case "publish-offer":
		return "OFFER PUBLISHED"
	case "accept-offer":
		return "OFFER ACCEPTED"
	case "receive-accept":
		return "ACCEPT RECEIVED"
	case "fund-blake":
		return "BLAKE FUNDED"
	case "fund-btc":
		return "BTC FUNDED"
	case "claim-btc":
		return "BTC CLAIMED"
	case "claim-blake":
		return "BLAKE CLAIMED"
	case "refund-btc":
		return "BTC REFUNDED"
	case "refund-blake":
		return "BLAKE REFUNDED"
	case "extract-secret":
		return "SECRET EXTRACTED"
	case "complete":
		return "SWAP COMPLETE"
	case "sync-funding", "sync-settlement":
		return "SYNCED"
	case "resume":
		return "RESUMED"
	case "recovery-plan":
		return "RECOVERY PLAN"
	case "watch-htlc":
		return "HTLC WATCHED"
	case "refresh-timeouts":
		return "TIMEOUTS REFRESHED"
	}

	return strings.ToUpper(strings.ReplaceAll(kind, "-", " "))
}

// fieldValue reads one "label: value" line out of a rendered result.
//
// It is used only for the coordination panel's own record id, which the panel printed from a
// command the operator typed; every view the UI renders comes from a structured result.
func fieldValue(text, label string) string {
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, label) {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, label))
		}
	}

	return ""
}

// describeActionText is the panel rendering of an action, from the view.
//
// It is the same information the CLI prints, built from fields rather than from a string
// the page had to parse.
func describeActionText(action proto.ActionView) string {
	var builder strings.Builder

	heading := actionHeading(action.Kind)

	builder.WriteString(heading)
	builder.WriteString("\n" + strings.Repeat("=", len(heading)))

	if action.SwapID != "" {
		builder.WriteString("\nswap id:  " + action.SwapID)
	}

	if action.State != "" {
		builder.WriteString("\nstate:    " + action.State)
	}

	if action.TxID != "" {
		builder.WriteString("\ntxid:     " + action.TxID)
	}

	if action.BlockHash != "" {
		builder.WriteString("\nblock:    " + action.BlockHash)
	}

	if action.Pending {
		builder.WriteString("\nstatus:   pending (in the mempool)")
	}

	if action.Confirmed {
		builder.WriteString("\nstatus:   confirmed")
	}

	if action.Recovered {
		builder.WriteString("\nnote:     adopted from the record rather than signed now")
	}

	if action.FeeSats != "" && action.FeeSats != "0" {
		builder.WriteString("\nfee:      " + action.FeeSats + " sats")
	}

	if action.FeeRate != "" && action.FeeRate != "0" {
		builder.WriteString(" (" + action.FeeRate + " sat/vB)")
	}

	if action.Message != "" {
		builder.WriteString("\nmessage:  " + action.Message)
	}

	if action.Sender != "" {
		builder.WriteString("\nsender:   " + action.Sender)
	}

	if action.Start != "" || action.End != "" {
		builder.WriteString("\nstate:    " + action.Start + " -> " + action.End)
	}

	if action.Reason != "" {
		builder.WriteString("\nreason:   " + action.Reason)
	}

	for _, fact := range action.Details {
		builder.WriteString("\n" + fact.Label + ": " + fact.Value)
	}

	if action.Performed {
		builder.WriteString("\nperformed: yes")
	}

	return builder.String()
}
