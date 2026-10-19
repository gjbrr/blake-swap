package worker

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/coord"
	"github.com/gjbrr/blake-swap/pkg/decimal"
	"github.com/gjbrr/blake-swap/pkg/fund"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/proto"
	"github.com/gjbrr/blake-swap/pkg/reconcile"
	"github.com/gjbrr/blake-swap/pkg/store"
	"github.com/gjbrr/blake-swap/pkg/swap"
	"github.com/gjbrr/blake-swap/pkg/view"
)

// Names the vault uses for the values the engine needs.
const (
	// SecretIdentity is the participant's Nostr secret key.
	SecretIdentity = "identity"
	// SecretProxy is the app server's proxy base URL, such as http://127.0.0.1:8787.
	SecretProxy = "proxy"
)

// The vault values holding this participant's wallet names.
//
// A participant has one signing wallet and one watch-only wallet per chain. It is not
// Alice's BLAKE wallet and Bob's Bitcoin wallet: that mapping belongs to a two-participant
// test rig on one node, and a real participant on one node needs their own name for each
// chain, in both roles.
const (
	SecretWalletBlake      = "wallet.blake"
	SecretWalletBTC        = "wallet.btc"
	SecretWalletBlakeWatch = "wallet.blake_watch"
	SecretWalletBTCWatch   = "wallet.btc_watch"
)

// Default wallet names, used until the participant sets their own.
const (
	defaultWalletBlake      = "blake"
	defaultWalletBTC        = "btc"
	defaultWalletBlakeWatch = "blake-watch"
	defaultWalletBTCWatch   = "btc-watch"
)

// engineDomain runs the engine's commands in the browser.
//
// It is the same engine the CLI drives, over the same store interface and the same node
// surface — the only difference is that its node connections point at the app server's
// proxy, and its records are sealed by the vault.
//
// Every command here is a case in one switch, and each returns the engine's answer
// unchanged: the browser may do less than the CLI, but it must never decide something the
// engine would have decided differently.
type engineDomain struct {
	vault *vaultDomain
	// dialer is the relay seam the engine's coordination transport is built over. It
	// is the browser's own WebSocket, injectable so the domain can be tested.
	dialer nostr.Dialer
	// nodes are built per call from the vault's proxy address, so changing the proxy
	// takes effect without restarting the worker.
	btc, blake *chain.Node
}

// newEngineDomain builds the engine domain over a vault and a relay dialer.
func newEngineDomain(vault *vaultDomain, dialer nostr.Dialer) *engineDomain {
	return &engineDomain{vault: vault, dialer: dialer}
}

// Handle runs one engine command.
func (d *engineDomain) Handle(ctx context.Context, request proto.Request) proto.Result {
	//
	// The wallet status is the one engine command that does not need the engine: it asks
	// the node which of this participant's wallets it can see, which is the question an
	// operator asks before any of the others can work.
	//
	if request.Command == "wallets.status" {
		return d.walletStatus(ctx, request)
	}

	if request.Command == "engine.status" {
		return d.status(ctx, request)
	}

	if request.Command == "engine.dashboard" {
		return d.dashboard(ctx, request)
	}

	if request.Command == "engine.resume-all" {
		return d.resumeAll(ctx, request)
	}

	//
	// The command and its arguments are checked before the vault is opened: a typo or a
	// missing argument is an operator's mistake, and reporting it as "swap not found" (or
	// as a locked vault) would send them looking in the wrong place.
	//
	if err := validateEngineArgs(request); err != nil {
		return failed(request.Command, err)
	}

	swapID := strings.TrimSpace(request.Args["swap_id"])

	engine, err := d.buildFor(ctx, swapID)

	if err != nil {
		return failed(request.Command, err)
	}

	switch request.Command {
	case "engine.new":
		return d.newSwap(ctx, engine, request)

	case "engine.list":
		records, err := engine.List(ctx)

		if err != nil {
			return failed(request.Command, err)
		}

		if len(records) == 0 {
			return withView(proto.Result{Command: request.Command, OK: true, Text: "No swaps."},
				proto.SwapList{})
		}

		var (
			lines []string
			swaps []proto.SwapSummary
		)

		for _, record := range records {
			lines = append(lines, record.SwapID+"\t"+string(record.State))

			// A record's own report is not built here: list is a cheap read, and six
			// chain reads per record is not.
			swaps = append(swaps, view.Summary(record, roleFor(record, engine), nil))
		}

		return withView(proto.Result{
			Command: request.Command, OK: true, Text: strings.Join(lines, "\n"),
		}, proto.SwapList{Swaps: swaps})

	case "engine.show":
		record, err := engine.Show(ctx, swapID)

		if err != nil {
			return failed(request.Command, err)
		}

		return withView(proto.Result{
			Command: request.Command, OK: true, Text: describeSwap(record),
		}, view.Summary(record, roleFor(record, engine), nil))

	case "engine.reconcile":
		report, err := engine.Reconcile(ctx, swapID)

		if err != nil {
			return failed(request.Command, err)
		}

		record, err := engine.Show(ctx, swapID)

		if err != nil {
			return failed(request.Command, err)
		}

		return withView(proto.Result{
			Command: request.Command, OK: true, Text: describeReport(report),
		}, view.Summary(record, string(report.ProtocolRole), report))

	case "engine.next":
		action, class, reason, err := engine.Next(ctx, swapID)

		if err != nil {
			return failed(request.Command, err)
		}

		return withView(proto.Result{
			Command: request.Command,
			OK:      true,
			Text:    string(action) + " (" + string(class) + "): " + reason,
		}, proto.ActionView{
			Command: request.Command, SwapID: swapID, Kind: string(action),
			DecisionClass: string(class), Reason: reason,
		})

	case "engine.publish-offer":
		result, err := engine.PublishOffer(ctx, swapID)

		if err != nil {
			return failed(request.Command, err)
		}

		return actionResult(request, view.Message(
			view.Action(request.Command, swapID, "publish-offer"), result))

	case "engine.accept-offer":
		return d.acceptOffer(ctx, engine, request)

	case "engine.receive-accept":
		result, err := engine.ReceiveAccept(ctx, swapID)

		if err != nil {
			return failed(request.Command, err)
		}

		return actionResult(request, view.Message(
			view.Action(request.Command, swapID, "receive-accept"), result))

	case "engine.fund-blake":
		result, err := engine.FundBlake(ctx, swapID)

		if err != nil {
			return failed(request.Command, err)
		}

		return actionResult(request, view.Funding(
			view.Action(request.Command, swapID, "fund-blake"), result))

	case "engine.fund-btc":
		result, err := engine.FundBTC(ctx, swapID)

		if err != nil {
			return failed(request.Command, err)
		}

		return actionResult(request, view.Funding(
			view.Action(request.Command, swapID, "fund-btc"), result))

	case "engine.verify-blake":
		if err := engine.VerifyBlake(ctx, swapID); err != nil {
			return failed(request.Command, err)
		}

		return actionResult(request, view.Action(request.Command, swapID, "verify-blake"))

	case "engine.verify-btc":
		if err := engine.VerifyBTC(ctx, swapID); err != nil {
			return failed(request.Command, err)
		}

		return actionResult(request, view.Action(request.Command, swapID, "verify-btc"))

	case "engine.claim-btc":
		return d.settlement(ctx, engine, request, swapID, "claim-btc", engine.ClaimBTC)

	case "engine.claim-blake":
		return d.settlement(ctx, engine, request, swapID, "claim-blake", engine.ClaimBlake)

	case "engine.refund-btc":
		return d.settlement(ctx, engine, request, swapID, "refund-btc", engine.RefundBTC)

	case "engine.refund-blake":
		return d.settlement(ctx, engine, request, swapID, "refund-blake", engine.RefundBlake)

	case "engine.extract-secret":
		//
		// The preimage is deliberately not returned. The engine records it in the
		// record, which is sealed, and that is where a key belongs: a value printed into
		// a document would be a secret in the DOM, and one that a screenshot would keep.
		//
		if _, err := engine.ExtractSecret(ctx, swapID); err != nil {
			return failed(request.Command, err)
		}

		action := view.Action(request.Command, swapID, "extract-secret")
		action.Details = []proto.ActionFact{
			{Label: "secret", Value: "extracted into the sealed record"},
		}

		return actionResult(request, action)

	case "engine.complete":
		if err := engine.Complete(ctx, swapID); err != nil {
			return failed(request.Command, err)
		}

		action := view.Action(request.Command, swapID, "complete")
		action.State = string(stateComplete)

		return actionResult(request, action)

	case "engine.sync-funding":
		result, err := engine.SyncFunding(ctx, swapID)

		if err != nil {
			return failed(request.Command, err)
		}

		return actionResult(request, view.Sync(
			view.Action(request.Command, swapID, "sync-funding"), result))

	case "engine.sync-settlement":
		result, err := engine.SyncSettlement(ctx, swapID)

		if err != nil {
			return failed(request.Command, err)
		}

		return actionResult(request, view.Sync(
			view.Action(request.Command, swapID, "sync-settlement"), result))

	case "engine.resume":
		result, err := engine.Resume(ctx, swapID)

		if err != nil {
			return failed(request.Command, err)
		}

		return actionResult(request, view.Resume(
			view.Action(request.Command, swapID, "resume"), result))

	case "engine.recovery-plan":
		plan, err := engine.RecoveryPlan(ctx, swapID)

		if err != nil {
			return failed(request.Command, err)
		}

		return withView(proto.Result{
			Command: request.Command, OK: true, Text: describeRecovery(plan),
		}, recoveryView(plan))

	case "engine.watch-htlc":
		status, err := engine.WatchHTLC(ctx, swapID)

		if err != nil {
			return failed(request.Command, err)
		}

		action := view.Action(request.Command, swapID, "watch-htlc")
		action.Details = []proto.ActionFact{
			{Label: "chain", Value: string(status.Chain)},
			{Label: "wallet", Value: status.Wallet},
			{Label: "address", Value: status.Address},
			{Label: "imported", Value: strconv.FormatBool(status.Imported)},
		}

		if status.Reason != "" {
			action.Reason = status.Reason
		}

		return actionResult(request, action)

	case "engine.refresh-timeouts":
		cleared := strings.EqualFold(strings.TrimSpace(request.Args["clear_stale_intent"]), "true")

		refresh, err := engine.RefreshTimeouts(ctx, swapID, cleared)

		if err != nil {
			return failed(request.Command, err)
		}

		action := view.Action(request.Command, swapID, "refresh-timeouts")
		action.Details = []proto.ActionFact{
			{Label: "BTC timeout", Value: heights(refresh.OldBTCTimeout, refresh.NewBTCTimeout)},
			{Label: "BLAKE timeout", Value: heights(refresh.OldBlakeTimeout, refresh.NewBlakeTimeout)},
			{Label: "BTC height", Value: strconv.FormatInt(refresh.BTCHeight, 10)},
			{Label: "BLAKE height", Value: strconv.FormatInt(refresh.BlakeHeight, 10)},
		}

		if len(refresh.ClearedIntents) > 0 {
			action.Details = append(action.Details, proto.ActionFact{
				Label: "cleared intents", Value: strings.Join(refresh.ClearedIntents, ", "),
			})
		}

		return actionResult(request, action)
	}

	return proto.Result{
		Command: request.Command,
		Error:   "unsupported engine command: " + request.Command,
	}
}

// stateComplete is the terminal checkpoint, named once so a typo is a compile error in
// the one place it is used.
const stateComplete = "COMPLETE"

// engineCommands is the client's engine surface.
//
// It is a table rather than a default case so an unknown command is refused before the
// vault is opened, and so the test-only `advance` cannot be reached by name at all.
var engineCommands = map[string]bool{
	"engine.new": true, "engine.list": true, "engine.show": true,
	"engine.reconcile": true, "engine.next": true, "engine.dashboard": true,
	"engine.status": true, "engine.publish-offer": true, "engine.accept-offer": true,
	"engine.receive-accept": true, "engine.fund-blake": true, "engine.fund-btc": true,
	"engine.verify-blake": true, "engine.verify-btc": true, "engine.claim-btc": true,
	"engine.claim-blake": true, "engine.refund-btc": true, "engine.refund-blake": true,
	"engine.extract-secret": true, "engine.complete": true, "engine.sync-funding": true,
	"engine.sync-settlement": true, "engine.resume": true, "engine.resume-all": true,
	"engine.recovery-plan": true, "engine.watch-htlc": true, "engine.refresh-timeouts": true,
	"wallets.status": true,
}

// commandsWithoutSwap are the ones that name no record.
var commandsWithoutSwap = map[string]bool{
	"engine.new": true, "engine.list": true, "engine.dashboard": true,
	"engine.status": true, "engine.resume-all": true, "wallets.status": true,
}

// validateEngineArgs checks a command's shape before any work is done.
func validateEngineArgs(request proto.Request) error {
	if !engineCommands[request.Command] {
		return fmt.Errorf("unsupported engine command: %s", request.Command)
	}

	if commandsWithoutSwap[request.Command] {
		return nil
	}

	if strings.TrimSpace(request.Args["swap_id"]) == "" {
		return fmt.Errorf("%s requires a swap id", strings.TrimPrefix(request.Command, "engine."))
	}

	if raw := strings.TrimSpace(request.Args["fee_rate"]); raw != "" {
		rate, err := decimal.Parse(raw)

		if err != nil {
			return fmt.Errorf("fee_rate must be a number of sat/vB")
		}

		if rate.IsNegative() || rate.IsZero() {
			return fmt.Errorf("fee_rate must be a positive number of sat/vB")
		}
	}

	return nil
}

// status renders both chain tips.
func (d *engineDomain) status(ctx context.Context, request proto.Request) proto.Result {
	engine, err := d.build(ctx)

	if err != nil {
		return failed(request.Command, err)
	}

	status, err := engine.Status(ctx)

	if err != nil {
		return failed(request.Command, err)
	}

	return withView(proto.Result{
		Command: request.Command,
		OK:      true,
		Text: "BLAKE height: " + strconv.FormatInt(status.Blake, 10) +
			"\nBitcoin height: " + strconv.FormatInt(status.BTC, 10),
	}, proto.SetupView{
		Heights:      proto.HeightView{Blake: status.Blake, BTC: status.BTC},
		ProxyHealthy: true,
	})
}

// dashboard renders every record, classified.
func (d *engineDomain) dashboard(ctx context.Context, request proto.Request) proto.Result {
	engine, err := d.build(ctx)

	if err != nil {
		return failed(request.Command, err)
	}

	entries, err := engine.Dashboard(ctx)

	if err != nil {
		return failed(request.Command, err)
	}

	summary := swap.Classify(entries)
	summary.Archived = engine.Archive(ctx)
	summary.Unsolvable = engine.UnsolvableHTLCs(ctx)

	// A dashboard whose heights are unknown still lists the records; the heights are
	// the one part that may legitimately be missing.
	heights := proto.HeightView{}

	if status, err := engine.Status(ctx); err == nil {
		heights = proto.HeightView{Blake: status.Blake, BTC: status.BTC}
	}

	return withView(proto.Result{
		Command: request.Command, OK: true, Text: describeDashboard(summary),
	}, view.Dashboard(summary, summary.Archived, summary.Unsolvable, heights))
}

// resumeAll resumes every swap that has work.
func (d *engineDomain) resumeAll(ctx context.Context, request proto.Request) proto.Result {
	engine, err := d.build(ctx)

	if err != nil {
		return failed(request.Command, err)
	}

	results, err := engine.ResumeAll(ctx)

	if err != nil {
		return failed(request.Command, err)
	}

	actions := make([]proto.ActionView, 0, len(results))

	var lines []string

	for _, result := range results {
		actions = append(actions, view.Resume(
			view.Action(request.Command, result.SwapID, "resume-all"), result))

		lines = append(lines, result.SwapID+"\t"+string(result.Action)+"\t"+
			strconv.FormatBool(result.Performed))
	}

	if len(lines) == 0 {
		lines = []string{"nothing to resume"}
	}

	return withView(proto.Result{
		Command: request.Command, OK: true, Text: strings.Join(lines, "\n"),
	}, actions)
}

// newSwap creates a swap as protocol Alice.
func (d *engineDomain) newSwap(ctx context.Context, engine *swap.Engine, request proto.Request) proto.Result {
	blake, err := amount.ParseBlake(request.Args["blake"])

	if err != nil {
		return proto.Result{Command: request.Command, Error: "BLAKE amount: " + err.Error()}
	}

	sats, err := positiveSats(request.Args["sats"])

	if err != nil {
		return proto.Result{Command: request.Command, Error: err.Error()}
	}

	blakeTimeout, err := optionalHeight(request.Args["blake_timeout"])

	if err != nil {
		return failure(request.Command, err)
	}

	btcTimeout, err := optionalHeight(request.Args["btc_timeout"])

	if err != nil {
		return failure(request.Command, err)
	}

	//
	// Both deadlines or neither, checked here as well as in the engine: an operator who
	// typed one should hear about it before a chain read, and the engine's own refusal
	// stays the authority.
	//
	if (blakeTimeout > 0) != (btcTimeout > 0) {
		return proto.Result{
			Command: request.Command,
			Error:   "either supply both blake_timeout and btc_timeout, or supply neither",
		}
	}

	// A swap is created by Alice, so the engine's local role is Alice's for this call.
	engine.Role = store.Alice

	result, err := engine.New(ctx, swap.NewRequest{
		BlakeAmount:  blake,
		BTCSats:      sats,
		BlakeTimeout: blakeTimeout,
		BTCTimeout:   btcTimeout,
		NostrBobPub:  strings.TrimSpace(request.Args["peer"]),
		Random:       randomReader{},
	})

	if err != nil {
		return failed(request.Command, err)
	}

	return actionResult(request, view.NewSwap(
		view.Action(request.Command, result.Swap.SwapID, "new"), result))
}

// acceptOffer accepts an offer as protocol Bob.
func (d *engineDomain) acceptOffer(ctx context.Context, engine *swap.Engine, request proto.Request) proto.Result {
	swapID := strings.TrimSpace(request.Args["swap_id"])

	if swapID == "" {
		return proto.Result{Command: request.Command, Error: "accept-offer requires a swap id"}
	}

	accept := swap.AcceptRequest{
		SwapID:              swapID,
		ExpectedAlicePub:    strings.TrimSpace(request.Args["expected_alice"]),
		AllowUnpinnedAuthor: strings.EqualFold(strings.TrimSpace(request.Args["allow_unpinned"]), "true"),
	}

	if raw := strings.TrimSpace(request.Args["expected_blake"]); raw != "" {
		parsed, err := amount.ParseBlake(raw)

		if err != nil {
			return proto.Result{Command: request.Command, Error: "expected BLAKE amount: " + err.Error()}
		}

		accept.ExpectedBlakeAmount = &parsed
	}

	if raw := strings.TrimSpace(request.Args["expected_sats"]); raw != "" {
		parsed, err := positiveSats(raw)

		if err != nil {
			return proto.Result{Command: request.Command, Error: "expected BTC amount: " + err.Error()}
		}

		accept.ExpectedBTCSats = &parsed
	}

	// Accepting an offer is Bob's act, so the record will be Bob's.
	engine.Role = store.Bob

	result, err := engine.AcceptOffer(ctx, accept)

	if err != nil {
		return failed(request.Command, err)
	}

	return actionResult(request, view.Accept(
		view.Action(request.Command, swapID, "accept-offer"), result))
}

// settlement runs one claim or refund with the optional fee override.
func (d *engineDomain) settlement(
	ctx context.Context,
	engine *swap.Engine,
	request proto.Request,
	swapID, kind string,
	run func(context.Context, string) (*swap.SettlementResult, error),
) proto.Result {
	if raw := strings.TrimSpace(request.Args["fee_rate"]); raw != "" {
		rate, err := decimal.Parse(raw)

		if err != nil {
			return proto.Result{
				Command: request.Command,
				Error:   "fee_rate must be a number of sat/vB",
			}
		}

		engine.Settlement.FeeRate = &rate
	}

	result, err := run(ctx, swapID)

	if err != nil {
		return failed(request.Command, err)
	}

	return actionResult(request, view.Settlement(
		view.Action(request.Command, swapID, kind), result))
}

// walletStatus reports the four wallet names and whether the node can see them.
//
// It is the question a person asks before a funding command: a wallet name that the node
// does not have is the single most common reason a funding refuses, and the refusal on
// its own does not say which of the four is wrong.
func (d *engineDomain) walletStatus(ctx context.Context, request proto.Request) proto.Result {
	names, err := d.wallets(ctx)

	if err != nil {
		return failed(request.Command, err)
	}

	views := proto.WalletView{
		Blake: names[SecretWalletBlake], BTC: names[SecretWalletBTC],
		BlakeWatch: names[SecretWalletBlakeWatch], BTCWatch: names[SecretWalletBTCWatch],
		Visible: map[string]bool{},
	}

	lines := []string{
		"BLAKE wallet:       " + views.Blake,
		"BTC wallet:         " + views.BTC,
		"BLAKE watch wallet: " + views.BlakeWatch,
		"BTC watch wallet:   " + views.BTCWatch,
	}

	if _, err := d.build(ctx); err != nil {
		// The names are still worth reporting when the node is unreachable: the
		// configuration is a fact, the visibility is an observation.
		lines = append(lines, "node: "+err.Error())

		return withView(proto.Result{
			Command: request.Command, OK: true, Text: strings.Join(lines, "\n"),
		}, views)
	}

	for chainName, wallet := range map[string]string{
		"blake": views.Blake, "btc": views.BTC,
		"blake-watch": views.BlakeWatch, "btc-watch": views.BTCWatch,
	} {
		visible := d.walletVisible(ctx, chainName, wallet)

		views.Visible[wallet] = visible

		state := "not visible"

		if visible {
			state = "visible"
		}

		lines = append(lines, wallet+": "+state)
	}

	return withView(proto.Result{
		Command: request.Command, OK: true, Text: strings.Join(lines, "\n"),
	}, views)
}

// walletVisible asks the node whether it has a wallet with this name.
//
// The answer is a fact about the node, not about the engine: a wallet the node does not
// have is the most common reason a funding refuses, and the refusal on its own does not
// say which of the four names is the wrong one.
func (d *engineDomain) walletVisible(ctx context.Context, chainName, wallet string) bool {
	node := d.btc

	if chainName == "blake" || chainName == "blake-watch" {
		node = d.blake
	}

	if node == nil || wallet == "" {
		return false
	}

	loaded, err := node.WalletLoaded(ctx, wallet)

	return err == nil && loaded
}

// wallets reads the four names from the vault, filling in defaults.
func (d *engineDomain) wallets(ctx context.Context) (map[string]string, error) {
	names := map[string]string{
		SecretWalletBlake:      defaultWalletBlake,
		SecretWalletBTC:        defaultWalletBTC,
		SecretWalletBlakeWatch: defaultWalletBlakeWatch,
		SecretWalletBTCWatch:   defaultWalletBTCWatch,
	}

	if d.vault == nil || d.vault.store == nil {
		return nil, fmt.Errorf("the vault is locked")
	}

	for key := range names {
		value, err := d.vault.readSecret(ctx, key)

		if err != nil {
			return nil, err
		}

		if strings.TrimSpace(value) != "" {
			names[key] = strings.TrimSpace(value)
		}
	}

	return names, nil
}

// build assembles the engine from the vault.
func (d *engineDomain) build(ctx context.Context) (*swap.Engine, error) {
	if d.vault == nil || d.vault.store == nil {
		return nil, fmt.Errorf("the vault is locked")
	}

	proxy, err := d.vault.proxyAddress(ctx)

	if err != nil {
		return nil, err
	}

	if proxy == "" {
		return nil, fmt.Errorf("no proxy address: set it with proxy.set")
	}

	identity, err := d.identity(ctx)

	if err != nil {
		return nil, err
	}

	d.btc, err = chain.New(chain.BTC, chain.Config{URL: proxy + "/rpc/btc"})

	if err != nil {
		return nil, err
	}

	d.blake, err = chain.New(chain.Blake, chain.Config{URL: proxy + "/rpc/blake"})

	if err != nil {
		return nil, err
	}

	engine := swap.NewEngine(d.vault.store, d.vault.intents(), d.blake, d.btc)

	//
	// The engine's coordination transport is the same relay transport the
	// coordination domain publishes with, so `Announce*` and `Receive*` work
	// unchanged in the browser: the only difference from the CLI is where the
	// socket and the identity come from.
	//
	relays, err := readRelayList(ctx, d.vault)

	if err != nil {
		return nil, err
	}

	mode, err := d.mode(ctx)

	if err != nil {
		return nil, err
	}

	engine.Transport = &coord.NostrTransport{
		Client:   &nostr.Client{Dialer: d.dialer, Relays: relays, Cache: nostr.NewMemoryCache()},
		Identity: identity,
	}

	names, err := d.wallets(ctx)

	if err != nil {
		return nil, err
	}

	engine.Identity = identity
	engine.Mode = mode
	engine.BlakeWallet = names[SecretWalletBlake]
	engine.BTCWallet = names[SecretWalletBTC]
	engine.BlakeWatchWallet = names[SecretWalletBlakeWatch]
	engine.BTCWatchWallet = names[SecretWalletBTCWatch]

	//
	// The protocol role is not a setting. Every command that has a swap derives it from
	// that record's identity pins before it runs, because a person is Alice in one swap
	// and Bob in another and a client that made them choose would be offering them
	// actions the engine then refuses.
	//
	engine.Role = store.Alice

	return engine, nil
}

// buildFor assembles the engine and, when the command names a swap, adopts that record's
// protocol role.
func (d *engineDomain) buildFor(ctx context.Context, swapID string) (*swap.Engine, error) {
	engine, err := d.build(ctx)

	if err != nil {
		return nil, err
	}

	if swapID == "" {
		return engine, nil
	}

	record, err := engine.Show(ctx, swapID)

	if err != nil {
		return nil, err
	}

	role, _, err := swap.LocalRole(record, engine.Identity.PublicKey())

	if err != nil {
		//
		// A record with no coordination pins at all was written by this client before
		// the counterparty was known — a FILE-transport handshake, or a record seeded by a
		// test. There is nothing to derive from, so the role the vault recorded for it is
		// the evidence, and Alice is the fallback because a record is created by Alice.
		//
		// A record that does pin somebody, and not us, keeps the engine's refusal: that
		// record belongs to another participant.
		//
		if record.NostrAlicePub != nil || record.NostrBobPub != nil {
			return nil, err
		}

		role = store.Alice

		if stored, storedErr := d.recordedRole(ctx, swapID); storedErr == nil && stored != "" {
			role = stored
		}
	}

	engine.Role = role

	return engine, nil
}

// recordedRole is the role the vault recorded when this swap was first saved.
//
// It is empty for a vault written before the role tag existed, and for a store that does
// not keep one at all; the caller falls back to the record's pins, which are the authority.
func (d *engineDomain) recordedRole(ctx context.Context, swapID string) (string, error) {
	if d.vault == nil || d.vault.store == nil {
		return "", fmt.Errorf("the vault is locked")
	}

	return d.vault.store.Role(ctx, swapID)
}

// roleFor is a record's role for a display path that must not fail.
func roleFor(record *model.Swap, engine *swap.Engine) string {
	if record == nil || engine == nil || engine.Identity == nil {
		return ""
	}

	role, _, err := swap.LocalRole(record, engine.Identity.PublicKey())

	if err != nil {
		return ""
	}

	return role
}

// mode is the configured coordination confidentiality mode.
func (d *engineDomain) mode(ctx context.Context) (nostr.Mode, error) {
	stored, err := d.vault.readSecret(ctx, SecretEncryption)

	if err != nil {
		return "", err
	}

	return nostr.ParseMode(stored)
}

// identity reads the participant's Nostr key from the vault.
func (d *engineDomain) identity(ctx context.Context) (*nostr.Key, error) {
	secret, err := d.vault.readSecret(ctx, SecretIdentity)

	if err != nil {
		return nil, err
	}

	if secret == "" {
		return nil, fmt.Errorf("no participant identity: set it with identity.set")
	}

	return nostr.NewKey(secret)
}

// withView attaches a view to a result, reporting an encoding failure as the command's
// failure: a command whose answer the page cannot read did not do its job.
func withView(result proto.Result, v any) proto.Result {
	withData, err := proto.WithData(result, v)

	if err != nil {
		return proto.Result{Command: result.Command, Error: err.Error()}
	}

	return withData
}

// actionResult renders an action and its view.
func actionResult(request proto.Request, action proto.ActionView) proto.Result {
	return withView(proto.Result{
		Command: request.Command, OK: true, Text: describeAction(action),
	}, action)
}

// describeAction is the one-line answer a terminal or a log wants.
func describeAction(action proto.ActionView) string {
	parts := []string{action.Kind}

	if action.SwapID != "" {
		parts = append(parts, action.SwapID)
	}

	if action.TxID != "" {
		parts = append(parts, action.TxID)
	}

	if action.State != "" {
		parts = append(parts, action.State)
	}

	if action.Message != "" {
		parts = append(parts, action.Message)
	}

	return strings.Join(parts, " ")
}

// failure renders a validation error.
func failure(command string, err error) proto.Result {
	return proto.Result{Command: command, Error: err.Error()}
}

// positiveSats parses a whole, positive satoshi count.
func positiveSats(raw string) (int64, error) {
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)

	if err != nil || value <= 0 {
		return 0, fmt.Errorf("must be a positive whole number of satoshis")
	}

	return value, nil
}

// optionalHeight parses an optional block height.
func optionalHeight(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)

	if raw == "" {
		return 0, nil
	}

	value, err := strconv.ParseInt(raw, 10, 64)

	if err != nil || value < 0 {
		return 0, fmt.Errorf("a timeout must be a non-negative block height")
	}

	return value, nil
}

// heights renders a replaced height pair.
func heights(old, updated int64) string {
	if old == updated {
		return strconv.FormatInt(updated, 10) + " (unchanged)"
	}

	return strconv.FormatInt(old, 10) + " -> " + strconv.FormatInt(updated, 10)
}

// recoveryView renders a recovery plan.
func recoveryView(plan *swap.RecoveryPlan) proto.RecoveryView {
	if plan == nil {
		return proto.RecoveryView{}
	}

	out := proto.RecoveryView{
		SwapID: plan.SwapID, State: string(plan.State), Role: plan.Role,
	}

	for _, leg := range plan.Legs {
		rendered := proto.RecoveryLegView{
			Chain:          string(leg.Chain),
			Descriptor:     leg.Descriptor,
			Address:        leg.Address,
			Amount:         strconv.FormatInt(leg.Amount, 10),
			FundingTxID:    leg.FundingTxID,
			Timeout:        leg.Timeout,
			OurClaimKey:    leg.OurClaimKey,
			OurRefundKey:   leg.OurRefundKey,
			SolvableWallet: leg.SolvableWallet,
			Reason:         leg.Reason,
		}

		if leg.FundingVout != nil {
			rendered.FundingVout = int64(*leg.FundingVout)
		}

		if leg.Solvable != nil {
			rendered.Solvable = *leg.Solvable

			rendered.Known = true
		}

		out.Legs = append(out.Legs, rendered)
	}

	return out
}

// describeRecovery renders a plan as text.
func describeRecovery(plan *swap.RecoveryPlan) string {
	if plan == nil {
		return "no plan"
	}

	var lines []string

	lines = append(lines, "swap id: "+plan.SwapID, "state: "+string(plan.State), "role: "+plan.Role)

	for _, leg := range plan.Legs {
		line := string(leg.Chain) + ": " + leg.Address

		if leg.Solvable != nil {
			if *leg.Solvable {
				line += " (solvable)"
			} else {
				line += " (not solvable)"
			}
		}

		lines = append(lines, line)
	}

	return strings.Join(lines, "\n")
}

// describeDashboard renders the dashboard as text.
func describeDashboard(state swap.Dashboard) string {
	groups := []struct {
		name    string
		entries []swap.DashboardEntry
	}{
		{"active", state.Active},
		{"attention", state.Attention},
		{"settled", state.Settled},
	}

	var lines []string

	for _, group := range groups {
		for _, entry := range group.entries {
			lines = append(lines, group.name+"\t"+entry.SwapID+"\t"+string(entry.State)+"\t"+
				entry.Outcome+"\t"+string(entry.Action))
		}
	}

	if len(lines) == 0 {
		return "No swaps."
	}

	return strings.Join(lines, "\n")
}

// describeSwap renders a record's facts as text, for the page's log.
func describeSwap(record *model.Swap) string {
	if record == nil {
		return ""
	}

	lines := []string{
		"swap id: " + record.SwapID,
		"state: " + string(record.State),
		"BLAKE: " + amount.Blake(record.BlakeAmountBaseUnits).Format(),
		"BTC sats: " + strconv.FormatInt(record.BTCSats, 10),
		"secret hash: " + record.SecretHash,
		"BTC timeout: " + strconv.FormatInt(record.BTCTimeout, 10),
		"BLAKE timeout: " + strconv.FormatInt(record.BlakeTimeout, 10),
	}

	return strings.Join(lines, "\n")
}

// describeReport renders a reconciliation report as text.
func describeReport(report *reconcile.Report) string {
	if report == nil {
		return ""
	}

	var builder strings.Builder

	builder.WriteString("swap id: " + report.SwapID + "\n")
	builder.WriteString("role: " + string(report.ProtocolRole) + "\n")
	builder.WriteString("state: " + string(report.CheckpointState) + "\n")
	builder.WriteString("BLAKE: " + string(report.Blake.Status))

	if report.Blake.Branch != reconcile.BranchNone {
		builder.WriteString(" / " + string(report.Blake.Branch))
	}

	builder.WriteString("\nBTC: " + string(report.BTC.Status))

	if report.BTC.Branch != reconcile.BranchNone {
		builder.WriteString(" / " + string(report.BTC.Branch))
	}

	builder.WriteString("\nclass: " + string(report.DecisionClass))
	builder.WriteString("\nnext action: " + string(report.NextAction))
	builder.WriteString("\nreason: " + report.Reason)

	return builder.String()
}

// intents returns the funding-intent store the engine needs.
//
// Funding intents are short-lived and belong to a funding operation, which is a
// host-side action in this client; an in-memory store keeps the read-only commands
// working without inventing a second durable format.
func (d *vaultDomain) intents() fund.Store { return fund.NewMemStore() }
