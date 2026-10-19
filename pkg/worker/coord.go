package worker

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/coord"
	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/keys"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/platform"
	"github.com/gjbrr/blake-swap/pkg/proto"
	"github.com/gjbrr/blake-swap/pkg/store"
	"github.com/gjbrr/blake-swap/pkg/swap"
	"github.com/gjbrr/blake-swap/pkg/timeout"
	"github.com/gjbrr/blake-swap/pkg/wire"
)

// SecretEncryption is the vault value holding the coordination confidentiality mode.
//
// It lives beside the identity and the relay list because it is this participant's
// configuration, and it is read the same way the CLI reads
// BLAKE_SWAP_NOSTR_ENCRYPTION: an unrecognised value is refused where it is typed, so
// a participant who mistyped the mode cannot silently publish plaintext.
const SecretEncryption = "encryption"

// coordDomain is the browser's settlement-message transport.
//
// It is the same transport the CLI drives — `coord.NostrTransport` over `pkg/nostr`,
// the reference's sender pinning, its NIP-44 policy and its fail-closed decryption —
// with the two browser differences that matter: the socket is the page's own
// `platform.WebSocket`, and the identity and relay list come from the unlocked vault
// rather than from a key file and an environment variable.
//
// The message bodies are not built here. They come from `swap.MessageBody`, the same
// builder the engine's announce commands use, so the browser and the CLI cannot drift
// into two wire formats.
type coordDomain struct {
	vault *vaultDomain
	// dialer is the platform seam, injectable so the domain can be tested against a
	// relay stub without a browser.
	dialer nostr.Dialer
	// now is the clock the event time is pinned from.
	now func() time.Time
}

// newCoordDomain builds the coordination domain over a vault and a dialer.
func newCoordDomain(vault *vaultDomain, dialer nostr.Dialer) *coordDomain {
	return &coordDomain{vault: vault, dialer: dialer, now: time.Now}
}

// CoordinationCommands are the worker commands the coordination domain answers, in the
// order the docs list them.
//
// The shape is `<domain>.<type>.<direction>`: one send and one receive per settlement
// message type, because "receive an accept" and "send an accept" are different acts
// with different role rules, and a single command would have to guess which one was
// meant.
func CoordinationCommands() []string {
	commands := []string{"coord.mode", "coord.set-mode", "coord.open"}

	for _, kind := range settlementKinds() {
		commands = append(commands, "coord."+kind+".send", "coord."+kind+".receive")
	}

	return commands
}

// settlementKinds is the seven message types, in the wire schema's order.
//
// It is read from the schema rather than written out: a message type the wire module
// does not define is not one this transport can publish, and a second list here could
// disagree with it without anything failing.
func settlementKinds() []string {
	kinds := make([]string, 0, len(wire.SettlementTypes))

	for _, kind := range wire.SettlementTypes {
		kinds = append(kinds, string(kind))
	}

	return kinds
}

// Handle runs one coordination command.
//
// Every command here is one act of the same conversation, which is why they are one
// domain: a mode set here is what the next send uses, and the record opened here is
// what both directions build their bodies from.
func (d *coordDomain) Handle(ctx context.Context, request proto.Request) proto.Result {
	switch request.Command {
	case "coord.mode":
		return d.readMode(ctx, request)

	case "coord.set-mode":
		return d.setMode(ctx, request)

	case "coord.open":
		return d.open(ctx, request)
	}

	kind, direction, ok := parseCoordCommand(request.Command)

	if !ok {
		return proto.Result{
			Command: request.Command,
			Error:   "unsupported coordination command: " + request.Command,
		}
	}

	if direction == "send" {
		return d.send(ctx, request, kind)
	}

	return d.receive(ctx, request, kind)
}

// parseCoordCommand splits `coord.<kind>.<direction>`.
func parseCoordCommand(command string) (kind, direction string, ok bool) {
	rest, found := strings.CutPrefix(command, "coord.")

	if !found {
		return "", "", false
	}

	for _, candidate := range []string{"send", "receive"} {
		prefix, trimmed := strings.CutSuffix(rest, "."+candidate)

		if !trimmed {
			continue
		}

		for _, known := range settlementKinds() {
			if prefix == known {
				return prefix, candidate, true
			}
		}
	}

	return "", "", false
}

// readMode reports the configured confidentiality mode.
func (d *coordDomain) readMode(ctx context.Context, request proto.Request) proto.Result {
	mode, err := d.mode(ctx)

	if err != nil {
		return failed(request.Command, err)
	}

	return proto.Result{
		Command: request.Command, OK: true, Text: "encryption: " + string(mode),
	}
}

// setMode stores the confidentiality mode.
func (d *coordDomain) setMode(ctx context.Context, request proto.Request) proto.Result {
	mode, err := nostr.ParseMode(request.Args["mode"])

	if err != nil {
		return proto.Result{Command: request.Command, Error: err.Error()}
	}

	if err := d.vault.writeSecret(ctx, SecretEncryption, string(mode)); err != nil {
		return failed(request.Command, err)
	}

	return proto.Result{
		Command: request.Command, OK: true, Text: "encryption set: " + string(mode),
	}
}

// open creates a swap record this worker can coordinate about.
//
// The browser holds no wallet yet, so the record's contract keys are this
// participant's own Nostr key in its compressed form and the timeout plan is the
// policy's plan from height zero. That makes the record a well-formed protocol
// proposal, which is what the transport needs, and explicitly not a fundable
// contract: the funding flow replaces the contract keys from the wallet and anchors
// the heights to both chains. A record that cannot announce is refused later, by the
// builder, rather than filled with a placeholder here.
func (d *coordDomain) open(ctx context.Context, request proto.Request) proto.Result {
	key, err := d.identity(ctx)

	if err != nil {
		return failed(request.Command, err)
	}

	blake, err := amount.ParseBlake(request.Args["blake"])

	if err != nil {
		return proto.Result{Command: request.Command, Error: "BLAKE amount: " + err.Error()}
	}

	// The amount is a whole number of satoshis, exactly as `market.publish` treats it.
	sats, err := strconv.ParseInt(strings.TrimSpace(request.Args["sats"]), 10, 64)

	if err != nil || sats <= 0 {
		return proto.Result{
			Command: request.Command,
			Error:   "BTC amount: must be a positive whole number of satoshis",
		}
	}

	policy := timeout.DefaultPolicy()

	plan, err := timeout.BuildPlan(0, 0, policy)

	if err != nil {
		return failed(request.Command, err)
	}

	contractKey, err := identityCompressedKey(key)

	if err != nil {
		return failed(request.Command, err)
	}

	record, err := model.New(randomReader{}, model.NewParams{
		BlakeAmount:        blake,
		BTCSats:            sats,
		BlakeTimeout:       plan.BlakeTimeoutHeight,
		BTCTimeout:         plan.BTCTimeoutHeight,
		PolicyVersion:      "1.5",
		BTCTimeoutWindow:   &plan.BTCWindowSeconds,
		BlakeTimeoutWindow: &plan.BlakeWindowSeconds,
		BobRecovery:        &plan.BobRecoverySeconds,
	})

	if err != nil {
		return failed(request.Command, err)
	}

	alice := key.Public
	record.NostrAlicePub = &alice
	record.BlakeAlicePub = &contractKey
	record.BTCAlicePub = &contractKey

	if peer := strings.ToLower(strings.TrimSpace(request.Args["nostr_bob_pub"])); peer != "" {
		if _, err := hex.DecodeString(peer); err != nil || len(peer) != 64 {
			return proto.Result{
				Command: request.Command,
				Error:   "the counterparty key must be the full 64-character x-only Nostr public key",
			}
		}

		record.NostrBobPub = &peer
	}

	if d.vault.store == nil {
		return proto.Result{Command: request.Command, Error: "the vault is locked"}
	}

	if err := d.vault.store.Save(ctx, store.Alice, record); err != nil {
		return failed(request.Command, err)
	}

	text := "COORDINATION SWAP OPENED\nswap id: " + record.SwapID +
		"\nrole:    alice\nBLAKE:   " + amount.Blake(record.BlakeAmountBaseUnits).Format() +
		"\nsats:    " + strconv.FormatInt(record.BTCSats, 10)

	if record.NostrBobPub != nil {
		text += "\npeer:    " + *record.NostrBobPub
	}

	return proto.Result{Command: request.Command, OK: true, Text: text}
}

// identityCompressedKey is this participant's own Nostr key as a SEC1 compressed
// public key, which is the shape a contract key field holds.
func identityCompressedKey(key *nostr.Key) (string, error) {
	parsed, err := keys.FromSecretHex(key.Secret)

	if err != nil {
		return "", err
	}

	return parsed.CompressedPublicHex(), nil
}

// send publishes one settlement message signed by this participant.
//
// The body is built in Go from the record the worker holds — never from anything the
// page sent — and the transport refuses it if the local protocol role is not the role
// this message type belongs to.
func (d *coordDomain) send(ctx context.Context, request proto.Request, kind string) proto.Result {
	record, key, err := d.record(ctx, request.Args["swap_id"])

	if err != nil {
		return failed(request.Command, err)
	}

	role, peer, err := swap.LocalRole(record, key.Public)

	if err != nil {
		return failed(request.Command, err)
	}

	required, err := swap.SenderRole(kind)

	if err != nil {
		return failed(request.Command, err)
	}

	if role != required {
		return proto.Result{
			Command: request.Command,
			Error: "SECURITY FAILURE: refusing to sign " + kind + " as protocol " + role +
				"; protocol requires " + required,
		}
	}

	body, err := swap.MessageBody(record, kind)

	if err != nil {
		return failed(request.Command, err)
	}

	transport, err := d.transport(ctx, key)

	if err != nil {
		return failed(request.Command, err)
	}

	mode, err := d.mode(ctx)

	if err != nil {
		return failed(request.Command, err)
	}

	location, err := transport.Send(ctx, record.SwapID, kind, body, coord.SendOptions{
		LocalRole:    role,
		ExpectedRole: required,
		PeerPubkey:   peer,
		Mode:         mode,
		CreatedAt:    d.now().Unix(),
	})

	if err != nil {
		return failed(request.Command, err)
	}

	return proto.Result{
		Command: request.Command,
		OK:      true,
		Text: "COORDINATION MESSAGE PUBLISHED\ntype:       " + kind +
			"\nswap id:    " + record.SwapID +
			"\nevent id:   " + location +
			"\nsender:     " + key.Public +
			"\nmode:       " + modeLabel(mode) +
			"\n" + ciphertextLine(mode, kind),
	}
}

// receive reads and validates the counterparty's message for one type.
//
// Validation is the transport's: signature, pinned author, declared encryption and
// the canonical wire schema, all applied before this function sees a body. What is
// added here is the local half — this participant must be the role that receives
// this type, and the message must be about this record's swap and contract — and the
// refusal of a message from the wrong author by name instead of a "nothing found".
func (d *coordDomain) receive(ctx context.Context, request proto.Request, kind string) proto.Result {
	record, key, err := d.record(ctx, request.Args["swap_id"])

	if err != nil {
		return failed(request.Command, err)
	}

	role, peer, err := swap.LocalRole(record, key.Public)

	if err != nil {
		return failed(request.Command, err)
	}

	expected, err := swap.ReceiverRole(kind)

	if err != nil {
		return failed(request.Command, err)
	}

	if role != expected {
		return proto.Result{
			Command: request.Command,
			Error: "SECURITY FAILURE: this participant is protocol " + role +
				" and cannot receive " + kind + "; protocol delivers it to " + expected,
		}
	}

	if peer == "" {
		return proto.Result{
			Command: request.Command,
			Error: "SECURITY FAILURE: the swap record pins no counterparty to validate " +
				kind + " against",
		}
	}

	transport, err := d.transport(ctx, key)

	if err != nil {
		return failed(request.Command, err)
	}

	mode, err := d.mode(ctx)

	if err != nil {
		return failed(request.Command, err)
	}

	message, err := transport.Receive(ctx, record.SwapID, kind, coord.ReceiveOptions{
		ExpectedPubkey: peer,
		Mode:           mode,
	})

	if err != nil {
		// A message from the wrong author is refused, not skipped: the reason a
		// pinned read found nothing has to name the author it did find.
		return failed(request.Command, d.refuseForeign(ctx, key, record, kind, peer, mode, err))
	}

	//
	// The transport already refused every other author; this is the same fact
	// restated where it is used, because a pin that only exists inside the transport
	// is a pin one refactor away from being an optimisation.
	//
	if message.Sender != peer {
		return proto.Result{
			Command: request.Command,
			Error: "SECURITY FAILURE: " + kind + " was signed by " + message.Sender +
				", not by the pinned counterparty " + peer,
		}
	}

	if err := swap.CheckReceived(record, kind, message.Body); err != nil {
		return failed(request.Command, err)
	}

	return proto.Result{
		Command: request.Command,
		OK:      true,
		Text: "COORDINATION MESSAGE RECEIVED\ntype:       " + kind +
			"\nswap id:    " + record.SwapID +
			"\nsender:     " + message.Sender +
			"\nevent id:   " + message.Location +
			"\nmode:       " + modeLabel(mode) +
			"\n" + ciphertextLine(mode, kind) +
			"\nbody:       " + canonicalBody(message.Body),
	}
}

// refuseForeign is the diagnosis of a pinned receive that found nothing.
//
// The relay was asked only for the pinned author's events, and a relay is free to
// ignore that. When it does, the honest answer is not "no message": it is that a
// message for this swap and type exists under a different key, which is exactly the
// case the pin is there to catch.
func (d *coordDomain) refuseForeign(
	ctx context.Context,
	key *nostr.Key,
	record *model.Swap,
	kind, peer string,
	mode nostr.Mode,
	original error,
) error {
	//
	// A declared encryption we could not honour already names a security condition.
	// It must not be re-labelled as a wrong author, which would send an operator
	// looking at keys instead of at the downgrade.
	//
	if strings.Contains(original.Error(), "SECURITY FAILURE") {
		return original
	}

	client, err := d.client(ctx)

	if err != nil {
		return original
	}

	events, err := client.Fetch(ctx, nostr.Filter{
		Kinds: []int{event.KindTextNote},
		DTags: []string{record.SwapID},
		Limit: 20,
	})

	if err != nil {
		return original
	}

	for _, candidate := range events {
		body, decodeErr := nostr.DecodeContent(key, candidate, record.SwapID, kind, mode)

		if decodeErr != nil || body == nil {
			continue
		}

		if err := swap.CheckReceived(record, kind, body); err != nil {
			continue
		}

		return fmt.Errorf(
			"SECURITY FAILURE: %s was signed by %s, not by the pinned counterparty %s",
			kind, candidate.Pubkey, peer,
		)
	}

	return original
}

// record loads one swap record and the identity that must be pinned to it.
func (d *coordDomain) record(ctx context.Context, swapID string) (*model.Swap, *nostr.Key, error) {
	if d.vault.store == nil {
		return nil, nil, fmt.Errorf("the vault is locked")
	}

	swapID = strings.TrimSpace(swapID)

	if !model.IsSwapID(swapID) {
		return nil, nil, fmt.Errorf("swap_id must be the 12-character protocol swap id")
	}

	key, err := d.identity(ctx)

	if err != nil {
		return nil, nil, err
	}

	record, err := d.vault.store.Load(ctx, swapID)

	if err != nil {
		return nil, nil, err
	}

	return record, key, nil
}

// transport builds the relay transport from the vault's configuration.
//
// The relay list is the market's: a participant who configured where they read the
// market has configured where they coordinate, and a second list would be a second
// answer to the same question.
func (d *coordDomain) transport(ctx context.Context, key *nostr.Key) (*coord.NostrTransport, error) {
	client, err := d.client(ctx)

	if err != nil {
		return nil, err
	}

	return &coord.NostrTransport{Client: client, Identity: key}, nil
}

// client is a relay client over the configured relays.
func (d *coordDomain) client(ctx context.Context) (*nostr.Client, error) {
	relays, err := d.relayList(ctx)

	if err != nil {
		return nil, err
	}

	return &nostr.Client{
		Dialer: d.dialer,
		Relays: relays,
		Cache:  nostr.NewMemoryCache(),
	}, nil
}

// relayList reads the configured relays, or the default.
func (d *coordDomain) relayList(ctx context.Context) ([]string, error) {
	return readRelayList(ctx, d.vault)
}

// mode is the configured confidentiality mode.
func (d *coordDomain) mode(ctx context.Context) (nostr.Mode, error) {
	stored, err := d.vault.readSecret(ctx, SecretEncryption)

	if err != nil {
		return "", err
	}

	return nostr.ParseMode(stored)
}

// identity is this participant's signing key, from the unlocked vault.
func (d *coordDomain) identity(ctx context.Context) (*nostr.Key, error) {
	secret, err := d.vault.readSecret(ctx, SecretIdentity)

	if err != nil {
		return nil, err
	}

	if secret == "" {
		return nil, errMissingIdentity
	}

	return nostr.NewKey(secret)
}

// ciphertextLine says what the policy requires of one message type in one mode.
//
// It is derived from the reference's rule rather than from the event, because that is
// the question an operator is asking: an offer is exempt by policy (it is first contact,
// before the taker's key is known), and every other type must be ciphertext when the
// mode is on.
func ciphertextLine(mode nostr.Mode, kind string) string {
	if nostr.RequiresEncryption(mode, kind) {
		return "ciphertext: required"
	}

	if mode == nostr.ModeNIP44 {
		return "ciphertext: none (" + kind + " is exempt from encryption)"
	}

	return "ciphertext: none (encryption is off)"
}

// modeLabel renders a confidentiality mode for operator output.
func modeLabel(mode nostr.Mode) string {
	if mode == nostr.ModeNIP44 {
		return string(mode)
	}

	return "off"
}

// canonicalBody renders a validated message body as canonical JSON.
//
// The canonical form is the wire's, so a message that arrives reordered prints the
// same as the one that was sent, and a test can assert on it.
func canonicalBody(body map[string]any) string {
	encoded, err := event.CanonicalJSON(body)

	if err != nil {
		return "unrenderable: " + err.Error()
	}

	return string(encoded)
}

// relayDialer is the browser's socket, as a nostr.Dialer.
//
// It is a function rather than a method so the worker's loop can build both domains
// over the same seam and a test can replace it.
func relayDialer() nostr.Dialer {
	return func(ctx context.Context, url string) (nostr.Conn, error) {
		return platform.DialWebSocket(ctx, url)
	}
}
