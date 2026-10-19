//go:build js && wasm

package page

import (
	"fmt"
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

// UICmd is the command a DOM control sends. The string crosses from
// JavaScript, which is the realm boundary, so keep the vocabulary tiny and
// named rather than accepting arbitrary text.
type UICmd = string

// UICmdPing asks the domain to send another hello.
const UICmdPing UICmd = "ping"

// UICmdVaultTest asks the worker to run its encrypted-state self-test.
//
// It exists as a UI command rather than as a hidden boot step so the same button a
// person can press is the one the headless check presses: a test-only code path would
// prove something nobody runs.
const UICmdVaultTest UICmd = "vault-selftest"

// The rest of the vocabulary: the vault's two dependencies and the read-only engine
// commands. Each name is a button in index.html and a case in uiRequest.
const (
	UICmdIdentityStatus  UICmd = "identity-status"
	UICmdIdentitySet     UICmd = "identity-set"
	UICmdProxyStatus     UICmd = "proxy-status"
	UICmdProxySet        UICmd = "proxy-set"
	UICmdEngineStatus    UICmd = "engine-status"
	UICmdEngineList      UICmd = "engine-list"
	UICmdEngineDashboard UICmd = "engine-dashboard"
	UICmdEngineShow      UICmd = "engine-show"

	// The settlement transport's controls: a record to coordinate about, the seven
	// message types, and the confidentiality mode. They are the same commands the
	// CLI's announce and receive subcommands run.
	UICmdCoordOpen    UICmd = "coord-open"
	UICmdCoordSend    UICmd = "coord-send"
	UICmdCoordReceive UICmd = "coord-receive"
	UICmdCoordMode    UICmd = "coord-mode"
	UICmdCoordSetMode UICmd = "coord-set-mode"
)

// The market's controls: the relay list, a read, and the two acts that change
// the marketplace. They are the same commands the CLI's market subcommands run.
const (
	UICmdMarketRelays    UICmd = "market-relays"
	UICmdMarketSetRelays UICmd = "market-set-relays"
	UICmdMarketList      UICmd = "market-list"
	UICmdMarketPublish   UICmd = "market-publish"
	UICmdMarketTake      UICmd = "market-take"
)

// The vault is the page's front door: nothing else can store a record or read a
// secret until it is open, so creating and unlocking it are controls like any other.
const (
	UICmdVaultCreate UICmd = "vault-create"
	UICmdVaultUnlock UICmd = "vault-unlock"
	UICmdVaultLock   UICmd = "vault-lock"
)

// The element ids are part of the page contract: index.html renders them and
// the headless check in scripts/verify-browser.py reads them. Keeping them in
// one place stops the three from drifting.
const (
	ElementPageStatus   = "page-status"
	ElementWorkerStatus = "worker-status"
	ElementHello        = "hello"
	ElementRoundTrip    = "round-trip"

	// VerifiedAttribute marks the document once the worker's greeting has
	// been rendered, so a test can assert the round trip without scraping
	// presentation text.
	VerifiedAttribute = "data-hello-verified"

	// ElementVault is where the vault's own state is rendered, and ElementVaultTest
	// is where the encrypted self-test reports. They are separate panels because they
	// answer different questions: one is "is it open", the other "does sealing work".
	ElementVault     = "vault"
	ElementVaultTest = "vaulttest"

	// VaultVerifiedAttribute marks the document once the worker's encrypted
	// round trip has succeeded in this browser.
	VaultVerifiedAttribute = "data-vault-verified"

	// The panels the engine and its two dependencies report into, and the market's.
	ElementIdentity = "identity"
	ElementProxy    = "proxy"
	ElementEngine   = "engine"
	ElementMarket   = "market"

	// The inputs a command's arguments are read from. A control carries the id of
	// the input it feeds in data-uidata, so one click handler serves all of them.
	InputIdentitySecret = "identity-secret"
	InputProxyURL       = "proxy-url"
	InputSwapID         = "swap-id"
	InputPassphrase     = "passphrase"

	// The coordination inputs: the record's terms, the pinned counterparty, the
	// message type and the confidentiality mode.
	InputCoordBlake = "coord-blake"
	InputCoordSats  = "coord-sats"
	InputCoordPeer  = "coord-peer"
	InputCoordKind  = "coord-kind"
	InputCoordMode  = "coord-mode"
	ElementCoord    = "coord"

	// The market's inputs: where to read from, what to advertise, and what to take.
	InputRelays       = "relays"
	InputMarketBlake  = "market-blake"
	InputMarketSats   = "market-sats"
	InputMarketMinute = "market-minutes"
	InputMarketOffer  = "market-offer-id"

	// Markers set once a command of that family has succeeded here. They are what
	// the headless check asserts, so it never has to read presentation text.
	CoordVerifiedAttribute    = "data-coord-verified"
	IdentityVerifiedAttribute = "data-identity-set"
	ProxyVerifiedAttribute    = "data-proxy-set"
	EngineVerifiedAttribute   = "data-engine-verified"
	MarketVerifiedAttribute   = "data-market-verified"

	// VaultOpenAttribute tracks whether the vault is unlocked. Unlike the others it is
	// cleared again on a lock, because a marker that only ever turns on would claim the
	// vault is open after it was shut.
	VaultOpenAttribute = "data-vault-open"
)

// panel is where one command's output goes and which document marker its success
// sets. An empty marker means the command is informational and proves nothing on its
// own, so it sets nothing.
type panel struct {
	element string
	marker  string
	// clears is the marker a successful run removes, for the commands whose truth can
	// stop being true. Only locking does that today.
	clears string
}

// settlementKinds is the message types the page may ask the worker to send or receive.
//
// It is written out rather than derived from the worker's table: a page that could
// name an arbitrary command prefix would be a second dispatcher, and the worker would
// have no way to tell a typo from an attempt.
var settlementKinds = map[string]bool{
	"offer":         true,
	"accept":        true,
	"blake-funded":  true,
	"btc-funded":    true,
	"btc-claimed":   true,
	"blake-claimed": true,
	"btc-refunded":  true,
}

// panels routes a result by the command that produced it.
//
// Every command the page can send appears here. A command that does not is rendered
// into the worker panel and logged, which is how a missing case shows up as itself
// rather than as output in the wrong place.
var panels = map[string]panel{
	"vault.selftest": {element: ElementVaultTest, marker: VaultVerifiedAttribute},
	"vault.status":   {element: ElementVault},
	"vault.crypto":   {element: ElementVault},
	"vault.create":   {element: ElementVault, marker: VaultOpenAttribute},
	"vault.unlock":   {element: ElementVault, marker: VaultOpenAttribute},
	"vault.lock":     {element: ElementVault, clears: VaultOpenAttribute},

	"identity.set":    {element: ElementIdentity, marker: IdentityVerifiedAttribute},
	"identity.status": {element: ElementIdentity},

	"proxy.set":    {element: ElementProxy, marker: ProxyVerifiedAttribute},
	"proxy.status": {element: ElementProxy},

	"engine.status":    {element: ElementEngine, marker: EngineVerifiedAttribute},
	"engine.list":      {element: ElementEngine},
	"engine.show":      {element: ElementEngine},
	"engine.dashboard": {element: ElementEngine},

	// The settlement transport: opening a record, sending one message, receiving one,
	// and the mode the two of them use. A receive that validated sets the marker,
	// because that is the half of the conversation this client cannot fake.
	"coord.open":          {element: ElementCoord},
	"coord.mode":          {element: ElementCoord},
	"coord.set-mode":      {element: ElementCoord},
	"coord.offer.send":    {element: ElementCoord},
	"coord.offer.receive": {element: ElementCoord, marker: CoordVerifiedAttribute},

	"coord.accept.send":    {element: ElementCoord},
	"coord.accept.receive": {element: ElementCoord, marker: CoordVerifiedAttribute},

	"coord.blake-funded.send":    {element: ElementCoord},
	"coord.blake-funded.receive": {element: ElementCoord, marker: CoordVerifiedAttribute},

	"coord.btc-funded.send":    {element: ElementCoord},
	"coord.btc-funded.receive": {element: ElementCoord, marker: CoordVerifiedAttribute},

	"coord.btc-claimed.send":    {element: ElementCoord},
	"coord.btc-claimed.receive": {element: ElementCoord, marker: CoordVerifiedAttribute},

	"coord.blake-claimed.send":    {element: ElementCoord},
	"coord.blake-claimed.receive": {element: ElementCoord, marker: CoordVerifiedAttribute},

	"coord.btc-refunded.send":    {element: ElementCoord},
	"coord.btc-refunded.receive": {element: ElementCoord, marker: CoordVerifiedAttribute},

	// The market's read proves the relay connection works, so it sets the marker;
	// publishing and taking report into the same panel because they are the same
	// conversation.
	"market.relays":     {element: ElementMarket},
	"market.set-relays": {element: ElementMarket},
	"market.list":       {element: ElementMarket, marker: MarketVerifiedAttribute},
	"market.publish":    {element: ElementMarket},
	"market.take":       {element: ElementMarket},
}

// Domain is the page's event loop. It runs until Stop is closed.
//
// The state it owns is the outbound sequence number. That is deliberately the
// only mutable state on this side of the link.
func Domain(_ struct{}, ch Chans) {
	defer setPageStatus("stopped", "bad")

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
	sendHello()

	for {
		select {
		case e := <-ch.Link.Recv():
			switch e.Kind {
			case proto.WorkerReady:
				//
				// The worker's loop is up. Say so without claiming it
				// has answered: that is what the ack is for.
				//
				markWorkerReady()
				setWorkerStatus("connected", "wait")

			case proto.HelloAck:
				renderAck(e)

			case proto.CommandResult:
				renderResult(e)

			default:
				consoleLog("page: ignoring unexpected kind " + string(e.Kind))
			}

		case cmd := <-ch.UI:
			if cmd == UICmdPing {
				setPageStatus("pinging the worker", "wait")
				sendHello()

				continue
			}

			request, ok := uiRequest(cmd)

			if !ok {
				consoleLog("page: ignoring unknown UI command " + cmd)

				continue
			}

			sendCommand(ch.Link, &seq, request)

		case <-ch.Stop.Done():
			return
		}
	}
}

// uiRequest turns a control's command into the worker request it stands for.
//
// Arguments are read from the document here, at send time, so the page domain is the
// one place that knows which input feeds which command, and a value the person just
// typed cannot be stale in a closure.
func uiRequest(cmd UICmd) (proto.Request, bool) {
	switch cmd {
	case UICmdVaultTest:
		return proto.Request{Command: "vault.selftest"}, true

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

	case UICmdProxyStatus:
		return proto.Request{Command: "proxy.status"}, true

	case UICmdProxySet:
		return proto.Request{
			Command: "proxy.set",
			Args:    map[string]string{"url": inputValue(InputProxyURL)},
		}, true

	case UICmdEngineStatus:
		return proto.Request{Command: "engine.status"}, true

	case UICmdEngineList:
		return proto.Request{Command: "engine.list"}, true

	case UICmdEngineDashboard:
		return proto.Request{Command: "engine.dashboard"}, true

	case UICmdEngineShow:
		return proto.Request{
			Command: "engine.show",
			Args:    map[string]string{"swap_id": inputValue(InputSwapID)},
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

	case UICmdCoordSend, UICmdCoordReceive:
		kind := strings.ToLower(strings.TrimSpace(inputValue(InputCoordKind)))

		if !settlementKinds[kind] {
			return proto.Request{}, false
		}

		direction := "send"

		if cmd == UICmdCoordReceive {
			direction = "receive"
		}

		return proto.Request{
			Command: "coord." + kind + "." + direction,
			Args:    map[string]string{"swap_id": inputValue(InputSwapID)},
		}, true

	case UICmdCoordMode:
		return proto.Request{Command: "coord.mode"}, true

	case UICmdCoordSetMode:
		return proto.Request{
			Command: "coord.set-mode",
			Args:    map[string]string{"mode": inputValue(InputCoordMode)},
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

	case UICmdMarketPublish:
		return proto.Request{
			Command: "market.publish",
			Args: map[string]string{
				"blake":   inputValue(InputMarketBlake),
				"sats":    inputValue(InputMarketSats),
				"minutes": inputValue(InputMarketMinute),
			},
		}, true

	case UICmdMarketTake:
		return proto.Request{
			Command: "market.take",
			Args:    map[string]string{"offer_id": inputValue(InputMarketOffer)},
		}, true
	}

	return proto.Request{}, false
}

// sendCommand encodes one request and puts it on the link.
//
// The panel is cleared to a pending line first, so a command that never answers is
// visible as a command that never answered rather than as the previous output left
// sitting there.
func sendCommand(link tinyspawn.Duplex[proto.Envelope], seq *uint32, request proto.Request) {
	body, err := proto.EncodeRequest(request)

	if err != nil {
		renderPanel(request.Command, "could not encode the request: "+err.Error(), "bad")

		return
	}

	renderPanel(request.Command, "running "+request.Command+"…", "wait")

	*seq = *seq + 1

	link.Send(proto.Envelope{
		Kind: proto.Command,
		Seq:  *seq,
		Body: body,
		AtMS: time.Now().UnixMilli(),
	})
}

// renderResult displays one command's result in that command's panel.
//
// A failed command renders as text and sets no marker: a marker means the thing
// actually worked in this browser, which is the only reason to have one.
func renderResult(e proto.Envelope) {
	result, err := proto.DecodeResult(e.Body)

	if err != nil {
		renderPanel("", "the result could not be decoded: "+err.Error(), "bad")

		return
	}

	if !result.OK {
		renderPanel(result.Command, result.Command+" failed: "+result.Error, "bad")

		return
	}

	renderPanel(result.Command, result.Text, "ok")
}

// renderPanel writes text into the panel that owns a command and sets its state.
//
// The marker is set only for a success, and only for a command whose marker means
// something; an unknown command is reported as unknown and lands in the worker panel.
func renderPanel(command, text, state string) {
	if command == "" {
		setText(ElementVault, text)
		setStateClass(ElementVault, state)

		return
	}

	route, known := panels[command]

	if !known {
		consoleLog("page: no panel for " + command)
		route = panel{element: ElementVault}
	}

	setText(route.element, text)
	setStateClass(route.element, state)

	if state != "ok" {
		return
	}

	if route.marker != "" {
		mark(route.marker)
	}

	if route.clears != "" {
		unmark(route.clears)
	}
}

// renderAck displays the worker's greeting. This is the visible proof the
// skeleton exists for: the text comes from a different wasm instance, over the
// channel shim, and lands in the document here.
func renderAck(e proto.Envelope) {
	if strings.TrimSpace(e.Body) == "" {
		setWorkerStatus("empty answer", "bad")
		return
	}

	setHello(e.Body)

	micros := time.Now().UnixMilli() - e.AtMS

	if e.AtMS > 0 {
		setRoundTrip(fmt.Sprintf("%d ms, seq %d", micros, e.Seq))
	} else {
		setRoundTrip(fmt.Sprintf("seq %d", e.Seq))
	}

	setWorkerStatus("verified", "ok")
	setPageStatus("running", "ok")
	markVerified()
}
