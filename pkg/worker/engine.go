package worker

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/fund"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/proto"
	"github.com/gjbrr/blake-swap/pkg/reconcile"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// Names the vault uses for the two values the engine needs.
const (
	// SecretIdentity is the participant's Nostr secret key.
	SecretIdentity = "identity"
	// SecretProxy is the app server's proxy base URL, such as http://127.0.0.1:8787.
	SecretProxy = "proxy"
)

// engineDomain runs the engine's commands in the browser.
//
// It is the same engine the CLI drives, over the same store interface and the same
// node surface — the only difference is that its node connections point at the app
// server's proxy, and its records are sealed by the vault.
//
// It is deliberately read-only for now: status, list, show, reconcile, next, dashboard
// and resume are the commands that need no key material beyond the identity, and they are
// the ones a browser UI must be able to trust before it is allowed to move funds.
type engineDomain struct {
	vault *vaultDomain
	// nodes are built per call from the vault's proxy address, so changing the proxy
	// takes effect without restarting the worker.
	btc, blake *chain.Node
}

// newEngineDomain builds the engine domain over a vault.
func newEngineDomain(vault *vaultDomain) *engineDomain {
	return &engineDomain{vault: vault}
}

// Handle runs one engine command.
func (d *engineDomain) Handle(ctx context.Context, request proto.Request) proto.Result {
	engine, err := d.build(ctx)

	if err != nil {
		return failed(request.Command, err)
	}

	switch request.Command {
	case "engine.status":
		status, err := engine.Status(ctx)

		if err != nil {
			return failed(request.Command, err)
		}

		return proto.Result{
			Command: request.Command,
			OK:      true,
			Text: "BLAKE height: " + strconv.FormatInt(status.Blake, 10) +
				"\nBitcoin height: " + strconv.FormatInt(status.BTC, 10),
		}

	case "engine.list":
		records, err := engine.List(ctx)

		if err != nil {
			return failed(request.Command, err)
		}

		if len(records) == 0 {
			return proto.Result{Command: request.Command, OK: true, Text: "No swaps."}
		}

		var lines []string

		for _, record := range records {
			lines = append(lines, record.SwapID+"\t"+string(record.State))
		}

		return proto.Result{
			Command: request.Command, OK: true, Text: strings.Join(lines, "\n"),
		}

	case "engine.show":
		record, err := engine.Show(ctx, request.Args["swap_id"])

		if err != nil {
			return failed(request.Command, err)
		}

		return proto.Result{
			Command: request.Command, OK: true, Text: describeSwap(record),
		}

	case "engine.reconcile":
		report, err := engine.Reconcile(ctx, request.Args["swap_id"])

		if err != nil {
			return failed(request.Command, err)
		}

		return proto.Result{
			Command: request.Command, OK: true, Text: describeReport(report),
		}

	case "engine.next":
		action, class, reason, err := engine.Next(ctx, request.Args["swap_id"])

		if err != nil {
			return failed(request.Command, err)
		}

		return proto.Result{
			Command: request.Command,
			OK:      true,
			Text:    string(action) + " (" + string(class) + "): " + reason,
		}

	case "engine.dashboard":
		entries, err := engine.Dashboard(ctx)

		if err != nil {
			return failed(request.Command, err)
		}

		if len(entries) == 0 {
			return proto.Result{Command: request.Command, OK: true, Text: "No swaps."}
		}

		var lines []string

		for _, entry := range entries {
			lines = append(lines, entry.SwapID+"\t"+string(entry.State)+"\t"+
				entry.Outcome+"\t"+string(entry.Action))
		}

		return proto.Result{
			Command: request.Command, OK: true, Text: strings.Join(lines, "\n"),
		}
	}

	return proto.Result{
		Command: request.Command,
		Error:   "unsupported engine command: " + request.Command,
	}
}

// build assembles the engine from the vault.
func (d *engineDomain) build(ctx context.Context) (*swap.Engine, error) {
	if d.vault == nil || d.vault.store == nil {
		return nil, fmt.Errorf("the vault is locked")
	}

	proxy, err := d.vault.readSecret(ctx, SecretProxy)

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

	engine.Identity = identity
	engine.Role = "alice"
	engine.BlakeWallet = "alice"
	engine.BTCWallet = "bob"
	engine.BlakeWatchWallet = "alice-watch"
	engine.BTCWatchWallet = "bob-watch"

	return engine, nil
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

// describeSwap renders a record's facts as text, for the page's panel.
func describeSwap(record *model.Swap) string {
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
