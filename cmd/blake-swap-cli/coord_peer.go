package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/gjbrr/blake-swap/pkg/coord"
	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/nostr"
)

//
// The coordination peer: the node-free half of the settlement transport.
//
// A browser participant needs something on the other end of the relay that is not a
// browser, or "the message arrived" only proves that one client agrees with itself.
// These three commands are that other end. They are deliberately bare — a Nostr
// identity and a relay list, no node, no record, no funding — because the property
// under test is the transport: is the message signed, is it the pinned author's, is it
// encrypted the way the policy requires, and does the canonical schema accept it.
//
// They are not a second protocol implementation: every send goes through
// `coord.NostrTransport`, which is the same transport the engine and the browser worker
// use, so the schema and the encryption policy are enforced by the one implementation.
//

// The coordination peer's command names.
const (
	coordKeyCommand   = "coord-key"
	coordReadCommand  = "coord-read"
	coordReplyCommand = "coord-reply"
)

// runCoordPeer dispatches the node-free coordination commands.
//
// It is an if-chain rather than a second `switch command` on purpose: the dispatcher's
// switch is the command registry's contract, and a second one would be a second place
// to forget a case.
func runCoordPeer(ctx context.Context, command string, args []string) error {
	if command == coordKeyCommand {
		return runCoordKey()
	}

	if command == coordReadCommand {
		return runCoordRead(ctx, args)
	}

	if command == coordReplyCommand {
		return runCoordReply(ctx, args)
	}

	return fmt.Errorf("unsupported coordination command: %s", command)
}

// peerIdentity is this participant's signing key, as a concrete key rather than the
// engine's identity interface: the transport signs, so it needs the secret.
func peerIdentity() (*nostr.Key, error) {
	identity, err := loadIdentity(stateDirectory())

	if err != nil {
		return nil, err
	}

	key, ok := identity.(*nostr.Key)

	if !ok {
		return nil, fmt.Errorf("the participant key is not a Nostr key")
	}

	return key, nil
}

// peerTransport builds the relay transport and its mode from the environment.
func peerTransport(encryption string) (*coord.NostrTransport, nostr.Mode, error) {
	key, err := peerIdentity()

	if err != nil {
		return nil, "", err
	}

	mode, err := nostr.ParseMode(encryption)

	if err != nil {
		return nil, "", err
	}

	return &coord.NostrTransport{
		Client:   &nostr.Client{Dialer: dialRelay, Relays: relayList()},
		Identity: key,
	}, mode, nil
}

// runCoordKey prints this participant's x-only public key.
//
// It exists so a counterparty can pin this participant before the exchange starts,
// which is the difference between a pin and trust-on-first-use.
func runCoordKey() error {
	key, err := peerIdentity()

	if err != nil {
		return err
	}

	fmt.Printf("public key: %s\n", key.Public)

	return nil
}

// runCoordRead reads and validates one coordination message.
//
// Everything that makes a message acceptable is the transport's: the event signature,
// a declared encryption it can honour, and the canonical wire schema. The author pin is
// the caller's, because only the caller knows who the counterparty is supposed to be —
// and an author that does not match it is a refusal, not a miss.
func runCoordRead(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet(coordReadCommand, flag.ContinueOnError)

	kind := flags.String("kind", "offer", "the settlement message type to read")
	author := flags.String("author", "", "the pinned author's x-only public key")
	encryption := flags.String("encryption", os.Getenv("BLAKE_SWAP_NOSTR_ENCRYPTION"),
		"coordination encryption: off or nip44_v2 (default: BLAKE_SWAP_NOSTR_ENCRYPTION)")

	positional, err := parsePositional(flags, args, 1)

	if err != nil {
		return err
	}

	if len(positional) != 1 {
		return fmt.Errorf("%s requires a swap id", coordReadCommand)
	}

	swapID := strings.TrimSpace(positional[0])

	if strings.TrimSpace(*author) == "" {
		return fmt.Errorf("%s requires --author: an unpinned read proves nothing", coordReadCommand)
	}

	transport, mode, err := peerTransport(*encryption)

	if err != nil {
		return err
	}

	message, err := transport.Receive(ctx, swapID, *kind, coord.ReceiveOptions{
		ExpectedPubkey: strings.ToLower(strings.TrimSpace(*author)),
		Mode:           mode,
	})

	if err != nil {
		return err
	}

	encoded, err := event.CanonicalJSON(message.Body)

	if err != nil {
		return err
	}

	fmt.Printf("COORDINATION MESSAGE VALIDATED\n")
	fmt.Printf("type:     %s\n", message.Kind)
	fmt.Printf("swap id:  %s\n", swapID)
	fmt.Printf("sender:   %s\n", message.Sender)
	fmt.Printf("event id: %s\n", message.Location)
	fmt.Printf("encryption: %s\n", modeLabel(mode))
	fmt.Printf("body:     %s\n", string(encoded))

	return nil
}

// runCoordReply answers a counterparty's offer with this participant's acceptance.
//
// Bob's acceptance is the one settlement message a bare coordination peer can produce
// honestly: it says who Bob is and which contract it accepts, and every other type
// would claim a chain fact this peer does not have. The contract keys are supplied
// because a peer with no wallet has none of its own; the transport still refuses the
// message if it is not a valid `accept` for the offer it answers.
func runCoordReply(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet(coordReplyCommand, flag.ContinueOnError)

	kind := flags.String("kind", "accept", "the settlement message type to reply with")
	author := flags.String("author", "", "the pinned author of the offer being answered")
	blakeBob := flags.String("blake-bob-pub", "", "Bob's BLAKE contract public key, compressed hex")
	btcBob := flags.String("btc-bob-pub", "", "Bob's Bitcoin contract public key, compressed hex")
	encryption := flags.String("encryption", os.Getenv("BLAKE_SWAP_NOSTR_ENCRYPTION"),
		"coordination encryption: off or nip44_v2 (default: BLAKE_SWAP_NOSTR_ENCRYPTION)")

	positional, err := parsePositional(flags, args, 1)

	if err != nil {
		return err
	}

	if len(positional) != 1 {
		return fmt.Errorf("%s requires a swap id", coordReplyCommand)
	}

	swapID := strings.TrimSpace(positional[0])

	if *kind != "accept" {
		return fmt.Errorf(
			"%s can only answer with accept: another settlement type would claim a "+
				"chain fact this peer does not have", coordReplyCommand,
		)
	}

	if strings.TrimSpace(*author) == "" {
		return fmt.Errorf("%s requires --author: the offer must be pinned to be answered",
			coordReplyCommand)
	}

	if *blakeBob == "" || *btcBob == "" {
		return fmt.Errorf(
			"%s requires --blake-bob-pub and --btc-bob-pub: an acceptance names the "+
				"contract keys it will settle with", coordReplyCommand,
		)
	}

	transport, mode, err := peerTransport(*encryption)

	if err != nil {
		return err
	}

	offer, err := transport.Receive(ctx, swapID, "offer", coord.ReceiveOptions{
		ExpectedPubkey: strings.ToLower(strings.TrimSpace(*author)),
		Mode:           mode,
	})

	if err != nil {
		return err
	}

	secretHash, ok := offer.Body["secret_hash"].(string)

	if !ok || secretHash == "" {
		return fmt.Errorf("the offer has no secret_hash to accept")
	}

	if offer.Body["swap_id"] != swapID {
		return fmt.Errorf("the offer is for another swap")
	}

	location, err := transport.Send(ctx, swapID, "accept", map[string]any{
		"type":          "accept",
		"swap_id":       swapID,
		"secret_hash":   secretHash,
		"blake_bob_pub": *blakeBob,
		"btc_bob_pub":   *btcBob,
	}, coord.SendOptions{
		LocalRole:    "bob",
		ExpectedRole: "bob",
		PeerPubkey:   offer.Sender,
		Mode:         mode,
	})

	if err != nil {
		return err
	}

	key, err := peerIdentity()

	if err != nil {
		return err
	}

	fmt.Printf("COORDINATION REPLY PUBLISHED\n")
	fmt.Printf("type:     accept\n")
	fmt.Printf("swap id:  %s\n", swapID)
	fmt.Printf("event id: %s\n", location)
	fmt.Printf("sender:   %s\n", key.Public)
	fmt.Printf("answered: %s\n", offer.Sender)

	return nil
}

// modeLabel renders the confidentiality mode for operator output.
func modeLabel(mode nostr.Mode) string {
	if mode == nostr.ModeNIP44 {
		return string(mode)
	}

	return "off"
}
