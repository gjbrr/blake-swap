//go:build !js

package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/coord"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/platform"
	"github.com/gjbrr/blake-swap/pkg/proto"
	"github.com/gjbrr/blake-swap/pkg/relaystub"
)

// coordinationOver starts a relay and returns a coordination domain over an open vault
// with an identity and the stub as its relay list.
func coordinationOver(t *testing.T) (*coordDomain, *relaystub.Stub, *vaultDomain) {
	t.Helper()

	stub, err := relaystub.New()

	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(stub.Close)

	vault := openMarketVault(t)

	domain := newCoordDomain(vault, func(ctx context.Context, url string) (nostr.Conn, error) {
		return platform.DialWebSocket(ctx, url)
	})

	if result := ask(t, &marketDomain{vault: vault, dialer: domain.dialer}, "market.set-relays",
		map[string]string{"relays": stub.URL}); !result.OK {
		t.Fatalf("setting relays: %+v", result)
	}

	return domain, stub, vault
}

// openSwap drives the coordination domain's own record creation.
func openSwap(t *testing.T, domain *coordDomain, peer string) string {
	t.Helper()

	result := coordinatorAsk(t, domain, "coord.open", map[string]string{
		"blake": "10", "sats": "50000", "nostr_bob_pub": peer,
	})

	if !result.OK {
		t.Fatalf("coord.open: %+v", result)
	}

	return fieldValue(t, result.Text, "swap id:")
}

// coordinatorAsk sends one request through the coordination domain.
func coordinatorAsk(t *testing.T, domain *coordDomain, command string, args map[string]string) proto.Result {
	t.Helper()

	return domain.Handle(context.Background(), proto.Request{Command: command, Args: args})
}

// bobKey is the counterparty identity the tests pin.
func bobKey(t *testing.T) *nostr.Key {
	t.Helper()

	key, err := nostr.NewKey(strings.Repeat("22", 32))

	if err != nil {
		t.Fatal(err)
	}

	return key
}

// malloryKey is the identity the tests use to sign the message that must be refused.
func malloryKey(t *testing.T) *nostr.Key {
	t.Helper()

	key, err := nostr.NewKey(strings.Repeat("33", 32))

	if err != nil {
		t.Fatal(err)
	}

	return key
}

// publishAs signs and publishes one coordination message as another participant.
func publishAs(
	t *testing.T, domain *coordDomain, key *nostr.Key, swapID, kind string,
	body map[string]any, mode nostr.Mode, peer string,
) string {
	t.Helper()

	built, err := nostr.BuildCoordinationEvent(key, swapID, kind, body, nostr.BuildOptions{
		Mode: mode, PeerPubkey: peer, CreatedAt: domain.now().Unix(),
	})

	if err != nil {
		t.Fatal(err)
	}

	client, err := domain.client(context.Background())

	if err != nil {
		t.Fatal(err)
	}

	report, err := client.Publish(context.Background(), built)

	if err != nil {
		t.Fatal(err)
	}

	return report.EventID
}

// acceptBody is a valid accept body for one swap.
func acceptBody(t *testing.T, domain *coordDomain, swapID string) map[string]any {
	t.Helper()

	record, _, err := domain.record(context.Background(), swapID)

	if err != nil {
		t.Fatal(err)
	}

	return map[string]any{
		"type":          "accept",
		"swap_id":       record.SwapID,
		"secret_hash":   record.SecretHash,
		"blake_bob_pub": strings.Repeat("02", 33),
		"btc_bob_pub":   strings.Repeat("03", 33),
	}
}

// TestCoordDomainPublishesAnOffer: the browser's transport publishes through the same
// builder and the same relay client the CLI uses.
func TestCoordDomainPublishesAnOffer(t *testing.T) {
	domain, stub, _ := coordinationOver(t)

	swapID := openSwap(t, domain, "")

	result := coordinatorAsk(t, domain, "coord.offer.send", map[string]string{"swap_id": swapID})

	if !result.OK {
		t.Fatalf("coord.offer.send: %+v", result)
	}

	if !strings.Contains(result.Text, "COORDINATION MESSAGE PUBLISHED") {
		t.Fatalf("text = %q", result.Text)
	}

	events := stub.Events()

	if len(events) != 1 {
		t.Fatalf("published %d event(s)", len(events))
	}

	declared, ok := nostr.EventMessageType(events[0])

	if !ok || declared != "offer" {
		t.Fatalf("declared type = %q, %v", declared, ok)
	}

	body, err := nostr.DecodeContent(domainKey(t, domain), events[0], swapID, "offer", nostr.ModeOff)

	if err != nil {
		t.Fatal(err)
	}

	if body["swap_id"] != swapID || body["type"] != "offer" {
		t.Fatalf("body = %v", body)
	}
}

// domainKey is the vault identity of a coordination domain.
func domainKey(t *testing.T, domain *coordDomain) *nostr.Key {
	t.Helper()

	key, err := domain.identity(context.Background())

	if err != nil {
		t.Fatal(err)
	}

	return key
}

// TestCoordDomainRefusesToSignTheWrongRole: the sender role is the protocol's, not the
// caller's. Alice cannot sign Bob's acceptance.
func TestCoordDomainRefusesToSignTheWrongRole(t *testing.T) {
	domain, _, _ := coordinationOver(t)

	swapID := openSwap(t, domain, bobKey(t).Public)

	result := coordinatorAsk(t, domain, "coord.accept.send", map[string]string{"swap_id": swapID})

	if result.OK || !strings.Contains(result.Error, "SECURITY FAILURE") {
		t.Fatalf("an accept was signed by Alice: %+v", result)
	}
}

// TestCoordDomainRefusesToReceiveTheWrongRole: the receive side of the same rule. Alice
// cannot receive her own offer as though she were Bob.
func TestCoordDomainRefusesToReceiveTheWrongRole(t *testing.T) {
	domain, _, _ := coordinationOver(t)

	swapID := openSwap(t, domain, bobKey(t).Public)

	result := coordinatorAsk(t, domain, "coord.offer.receive", map[string]string{"swap_id": swapID})

	if result.OK || !strings.Contains(result.Error, "cannot receive offer") {
		t.Fatalf("Alice received her own offer as Bob: %+v", result)
	}
}

// TestCoordDomainReceivesThePinnedReply is the round trip the browser check asserts: a
// reply signed by the pinned counterparty is read, validated and rendered.
func TestCoordDomainReceivesThePinnedReply(t *testing.T) {
	domain, _, _ := coordinationOver(t)

	bob := bobKey(t)
	peer := bob.Public
	swapID := openSwap(t, domain, peer)

	publishAs(t, domain, bob, swapID, "accept", acceptBody(t, domain, swapID), nostr.ModeOff, "")

	result := coordinatorAsk(t, domain, "coord.accept.receive", map[string]string{"swap_id": swapID})

	if !result.OK {
		t.Fatalf("coord.accept.receive: %+v", result)
	}

	if !strings.Contains(result.Text, "COORDINATION MESSAGE RECEIVED") ||
		!strings.Contains(result.Text, peer) {
		t.Fatalf("text = %q", result.Text)
	}

	if !strings.Contains(result.Text, `"type":"accept"`) {
		t.Fatalf("the validated body is not rendered:\n%s", result.Text)
	}
}

// TestCoordDomainRefusesAMessageFromTheWrongAuthor: a valid accept under the wrong key
// is refused by name, not reported as "nothing found".
func TestCoordDomainRefusesAMessageFromTheWrongAuthor(t *testing.T) {
	domain, _, _ := coordinationOver(t)

	bob := bobKey(t)
	swapID := openSwap(t, domain, bob.Public)

	publishAs(t, domain, malloryKey(t), swapID, "accept", acceptBody(t, domain, swapID),
		nostr.ModeOff, "")

	result := coordinatorAsk(t, domain, "coord.accept.receive", map[string]string{"swap_id": swapID})

	if result.OK {
		t.Fatalf("a message from the wrong author was accepted: %+v", result)
	}

	if !strings.Contains(result.Error, "SECURITY FAILURE") ||
		!strings.Contains(result.Error, malloryKey(t).Public) ||
		!strings.Contains(result.Error, bob.Public) {
		t.Fatalf("the refusal does not name both authors: %q", result.Error)
	}
}

// TestCoordDomainRefusesAPlaintextReplyUnderNIP44 is the encryption policy at the
// receive side: the mode requires ciphertext, so a plaintext settlement message is a
// downgrade and a refusal.
func TestCoordDomainRefusesAPlaintextReplyUnderNIP44(t *testing.T) {
	domain, _, _ := coordinationOver(t)

	bob := bobKey(t)
	swapID := openSwap(t, domain, bob.Public)

	if result := coordinatorAsk(t, domain, "coord.set-mode",
		map[string]string{"mode": "nip44_v2"}); !result.OK {
		t.Fatalf("coord.set-mode: %+v", result)
	}

	publishAs(t, domain, bob, swapID, "accept", acceptBody(t, domain, swapID), nostr.ModeOff, "")

	result := coordinatorAsk(t, domain, "coord.accept.receive", map[string]string{"swap_id": swapID})

	if result.OK || !strings.Contains(result.Error, "downgrade") {
		t.Fatalf("a plaintext accept under nip44_v2 was accepted: %+v", result)
	}
}

// TestCoordDomainEncryptedRoundTrip: under the configured NIP-44 mode an encrypted
// reply decrypts and a plaintext one does not.
func TestCoordDomainEncryptedRoundTrip(t *testing.T) {
	domain, _, _ := coordinationOver(t)

	bob := bobKey(t)
	swapID := openSwap(t, domain, bob.Public)

	if result := coordinatorAsk(t, domain, "coord.set-mode",
		map[string]string{"mode": "nip44_v2"}); !result.OK {
		t.Fatalf("coord.set-mode: %+v", result)
	}

	// Alice publishes an offer; the offer is exempt from encryption by policy because
	// it is first contact, so the transport must still accept it in this mode.
	if result := coordinatorAsk(t, domain, "coord.offer.send",
		map[string]string{"swap_id": swapID}); !result.OK {
		t.Fatalf("coord.offer.send under nip44: %+v", result)
	}

	alice := domainKey(t, domain)

	publishAs(t, domain, bob, swapID, "accept", acceptBody(t, domain, swapID),
		nostr.ModeNIP44, alice.Public)

	result := coordinatorAsk(t, domain, "coord.accept.receive", map[string]string{"swap_id": swapID})

	if !result.OK {
		t.Fatalf("coord.accept.receive: %+v", result)
	}

	if !strings.Contains(result.Text, bob.Public) {
		t.Fatalf("text = %q", result.Text)
	}
}

// TestCoordDomainRefusesAnUnknownMode: a mode nobody defines is refused where it is
// typed, so a participant who mistyped it cannot silently publish plaintext.
func TestCoordDomainRefusesAnUnknownMode(t *testing.T) {
	domain, _, _ := coordinationOver(t)

	result := coordinatorAsk(t, domain, "coord.set-mode", map[string]string{"mode": "aes"})

	if result.OK || !strings.Contains(result.Error, "BLAKE_SWAP_NOSTR_ENCRYPTION") {
		t.Fatalf("an unknown mode was accepted: %+v", result)
	}
}

// TestCoordCommandsCoverEverySettlementType: all seven types have a send and a receive
// command, and each parses back to its own type and direction.
func TestCoordCommandsCoverEverySettlementType(t *testing.T) {
	commands := CoordinationCommands()

	for _, kind := range settlementKinds() {
		for _, direction := range []string{"send", "receive"} {
			command := "coord." + kind + "." + direction

			found := false

			for _, candidate := range commands {
				if candidate == command {
					found = true
				}
			}

			if !found {
				t.Errorf("%s is not in the command table", command)
			}

			parsedKind, parsedDirection, ok := parseCoordCommand(command)

			if !ok || parsedKind != kind || parsedDirection != direction {
				t.Errorf("%s parsed as %q %q %v", command, parsedKind, parsedDirection, ok)
			}
		}
	}

	if len(commands) != 3+2*len(settlementKinds()) {
		t.Fatalf("command table has %d entries", len(commands))
	}
}

// TestEngineDomainCarriesTheCoordinationTransport: the engine's own announce and
// receive commands must work in the browser, so the transport the coordination domain
// publishes with is the transport the engine holds.
func TestEngineDomainCarriesTheCoordinationTransport(t *testing.T) {
	domain, stub, vault := coordinationOver(t)

	if result := coordinatorAsk(t, domain, "coord.set-mode",
		map[string]string{"mode": "nip44_v2"}); !result.OK {
		t.Fatalf("coord.set-mode: %+v", result)
	}

	// The engine domain needs a proxy address, which is configuration rather than a
	// connection: nothing below dials it.
	if result := ask(t, &marketDomain{vault: vault, dialer: domain.dialer}, "market.set-relays",
		map[string]string{"relays": stub.URL}); !result.OK {
		t.Fatalf("setting relays: %+v", result)
	}

	if err := vault.writeSecret(context.Background(), SecretProxy, "http://127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}

	engine := newEngineDomain(vault, domain.dialer)

	built, err := engine.build(context.Background())

	if err != nil {
		t.Fatal(err)
	}

	transport, ok := built.Transport.(*coord.NostrTransport)

	if !ok {
		t.Fatalf("the engine's transport is %T, not the relay transport", built.Transport)
	}

	if transport.Identity == nil || transport.Client == nil {
		t.Fatalf("the engine's transport is incomplete: %+v", transport)
	}

	if len(transport.Client.Relays) != 1 || transport.Client.Relays[0] != stub.URL {
		t.Fatalf("the engine's relays are %v", transport.Client.Relays)
	}

	if built.Mode != nostr.ModeNIP44 {
		t.Fatalf("the engine's mode is %q", built.Mode)
	}
}

// TestCoordDomainRefusesAnUnknownCommand: the domain answers its own prefix, and a
// command outside its vocabulary is refused by name.
func TestCoordDomainRefusesAnUnknownCommand(t *testing.T) {
	domain, _, _ := coordinationOver(t)

	result := coordinatorAsk(t, domain, "coord.accept.explode", nil)

	if result.OK || !strings.Contains(result.Error, "unsupported coordination command") {
		t.Fatalf("result = %+v", result)
	}
}
