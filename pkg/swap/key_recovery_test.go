//go:build !js

package swap_test

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/internal/rigtest"
	"github.com/gjbrr/blake-swap/pkg/keys"
)

// Parity: tests/htlc_key_recovery_tests.py
//
// The drill: fund an HTLC whose refund key this wallet has never held, prove the
// wallet cannot spend it, prove `recovery-plan` describes what is needed, then
// import the private form of the descriptor into a fresh wallet and refund with it.
//
// This is deliberately not a rendering test. The evidence that matters is the node
// saying `solvable: true` for a wallet that did not exist when the HTLC was funded,
// and a refund transaction that spends the output.
func TestHTLCKeyRecoverySuite(t *testing.T) {
	r := rigtest.Load(t)

	messages := t.TempDir()

	alice := rigtest.NewSide(t, "go", "alice")
	bob := rigtest.NewSide(t, "go", "bob")

	btcStart := r.HeightOf(t, "btc")
	blakeStart := r.HeightOf(t, "blake")

	output := r.Run(t, alice, messages, "new", "--blake", "0.013", "--sats", "2600",
		"--btc-timeout", fmt.Sprint(btcStart+40),
		"--blake-timeout", fmt.Sprint(blakeStart+80))

	id := rigtest.SwapID(t, output)

	// A refund key this wallet has never seen. `publish-offer` fills in only the
	// keys that are absent, so the edit below is honoured and the HTLC is funded to
	// a script the signing wallet cannot satisfy.
	secret := make([]byte, 32)

	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}

	foreign, err := keys.FromSecret(secret)

	if err != nil {
		t.Fatal(err)
	}

	foreignPub := foreign.CompressedPublicHex()

	recordPath := filepath.Join(alice.State, id+".json")

	record := rigtest.Record(t, alice, id)
	record["blake_alice_pub"] = foreignPub

	encoded, err := json.MarshalIndent(record, "", "  ")

	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(recordPath, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	r.Run(t, alice, messages, "publish-offer", id)
	r.Run(t, bob, messages, "accept-offer", id, "--expected-alice-pub", alice.Public)
	r.Run(t, alice, messages, "receive-accept", id)
	r.Run(t, alice, messages, "fund-blake", id)
	r.Run(t, alice, messages, "publish-blake-funded", id)
	r.Run(t, bob, messages, "receive-blake-funded", id)

	r.AssertState(t, alice, messages, id, "BLAKE_FUNDED")

	// The plan must name the descriptor, the address and the branch, and must say
	// that no loaded wallet can spend it.
	plan := r.Run(t, alice, messages, "recovery-plan", id)

	for _, expected := range []string{
		"HTLC RECOVERY PLAN",
		"descriptor:",
		"address:",
		"refund key:",
		foreignPub,
		"solvable:     NO",
	} {
		if !strings.Contains(plan, expected) {
			t.Fatalf("the recovery plan omitted %q:\n%s", expected, plan)
		}
	}

	descriptor := planField(t, plan, "descriptor")
	address := planField(t, plan, "address")

	if !strings.HasPrefix(address, "bcrt1") {
		t.Fatalf("the plan's address is not a regtest address: %q", address)
	}

	// The watch-only wallet imported the descriptor when the leg was funded, so it
	// can see the HTLC even though it cannot spend it.
	watched := walletAddressInfo(t, r, "blake", "alice-watch", address)

	if !watched.Solvable {
		t.Fatalf("the watch-only wallet cannot see the HTLC: %+v", watched)
	}

	// A watch-only wallet reports imported addresses as its own; what makes it safe
	// is that it holds no private keys at all.
	var watchedWallet struct {
		PrivateKeysEnabled bool `json:"private_keys_enabled"`
	}

	if err := json.Unmarshal([]byte(r.Node(t, "blake", "-rpcwallet=alice-watch",
		"getwalletinfo")), &watchedWallet); err != nil {
		t.Fatalf("getwalletinfo: %v", err)
	}

	if watchedWallet.PrivateKeysEnabled {
		t.Fatal("the watch-only wallet has private keys enabled")
	}

	// The signing wallet cannot spend it, which is the premise of the whole case.
	signing := walletAddressInfo(t, r, "blake", "alice", address)

	if signing.IsMine || signing.Solvable {
		t.Fatalf("the funding wallet can spend an HTLC whose key it does not hold: %+v",
			signing)
	}

	// The private form of the same script, in a wallet created after the fact.
	recovered := recoverIntoFreshWallet(t, r, descriptor, foreignPub, wif(secret))

	if !recovered.IsMine || !recovered.Solvable {
		t.Fatalf("the recovered wallet cannot spend the HTLC: %+v", recovered)
	}

	// And it really spends it: the refund is built and signed by that wallet.
	r.MineTo(t, "blake", "alice", r.FieldInt(t, alice, messages, id, "blake_timeout"))

	recoveredSide := &rigtest.Side{
		Name: "go", Role: "alice", State: alice.State,
		KeyFile: alice.KeyFile, Public: alice.Public,
	}

	refundOutput, exit, err := r.Exec(t, recoveredSide, messages,
		[]string{"BLAKE_SWAP_BLAKE_WALLET=recovered"}, "refund-blake", id)

	if err != nil {
		t.Fatalf("refunding with the recovered wallet: %v", err)
	}

	if exit != 0 {
		t.Fatalf("the recovered wallet could not refund (exit %d):\n%s", exit, refundOutput)
	}

	if state, _ := rigtest.RecordField(t, alice, id, "state"); fmt.Sprint(state) != "BLAKE_REFUNDED" {
		// A refund submitted exactly at its CLTV height can be final for the next
		// block; adopt the recorded transaction rather than signing another.
		r.Mine(t, "blake", "alice", 1)
		r.Run(t, recoveredSide, messages, "refund-blake", id)
	}

	r.AssertState(t, alice, messages, id, "BLAKE_REFUNDED")

	fundingTxID, fundingVout := fundingOutpoint(t, alice, id, "blake")

	if utxo := r.Node(t, "blake", "gettxout", fundingTxID,
		fmt.Sprint(fundingVout)); strings.TrimSpace(utxo) != "" {
		t.Fatal("the HTLC is unspent after the recovered wallet refunded it")
	}
}

// addressInfo is what the node says about one address in one wallet.
type addressInfo struct {
	IsMine   bool `json:"ismine"`
	Solvable bool `json:"solvable"`
}

// walletAddressInfo asks one wallet about one address.
func walletAddressInfo(t *testing.T, r *rigtest.Rig, chain, wallet, address string) addressInfo {
	t.Helper()

	raw := r.Node(t, chain, "-rpcwallet="+wallet, "getaddressinfo", address)

	var info addressInfo

	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		t.Fatalf("getaddressinfo: %v", err)
	}

	return info
}

// recoverIntoFreshWallet substitutes the private key into the descriptor and
// imports it into a wallet that did not exist when the HTLC was funded.
func recoverIntoFreshWallet(t *testing.T, r *rigtest.Rig, descriptor, pubkey, private string) addressInfo {
	t.Helper()

	unsigned := descriptor

	if index := strings.Index(unsigned, "#"); index >= 0 {
		unsigned = unsigned[:index]
	}

	if !strings.Contains(unsigned, pubkey) {
		t.Fatalf("the descriptor does not contain the key it should: %s", unsigned)
	}

	withKey := strings.ReplaceAll(unsigned, pubkey, private)

	// The node computes the checksum for the substituted string; a descriptor that
	// is wrong is refused here rather than imported and silently useless.
	raw := r.Node(t, "blake", "getdescriptorinfo", withKey)

	var described struct {
		Descriptor string `json:"descriptor"`
		Checksum   string `json:"checksum"`
		IsSolvable bool   `json:"issolvable"`
	}

	if err := json.Unmarshal([]byte(raw), &described); err != nil {
		t.Fatalf("getdescriptorinfo: %v", err)
	}

	if !described.IsSolvable {
		t.Fatalf("the substituted descriptor is not solvable:\n%s", raw)
	}

	// A descriptor wallet that holds private keys, created fresh. The rig persists
	// between runs, so a wallet left by an earlier run is removed rather than
	// reused: "fresh" is the premise of the drill.
	_, _ = r.NodeTry(t, "blake", "unloadwallet", "recovered")
	_ = os.RemoveAll(filepath.Join(r.BlakeDatadir, "regtest", "wallets", "recovered"))

	r.Node(t, "blake", "createwallet", "recovered", "false", "false", "", "false", "true")

	// `getdescriptorinfo` returns the *public* form of the descriptor, but the
	// checksum it reports is the one for the string that was handed to it. That is
	// what makes it possible to import the private form: the node will not accept a
	// descriptor carrying a private key without a checksum.
	if described.Checksum == "" {
		t.Fatalf("getdescriptorinfo returned no checksum:\n%s", raw)
	}

	signed := withKey + "#" + described.Checksum

	request, err := json.Marshal([]map[string]any{{
		"desc":      signed,
		"timestamp": "now",
		"active":    false,
		"label":     "htlc-recovery",
	}})

	if err != nil {
		t.Fatal(err)
	}

	imported := r.Node(t, "blake", "-rpcwallet=recovered", "importdescriptors", string(request))

	var results []struct {
		Success bool `json:"success"`
		Error   *struct {
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal([]byte(imported), &results); err != nil {
		t.Fatalf("importdescriptors: %v\nraw=%q", err, imported)
	}

	if len(results) != 1 || !results[0].Success {
		t.Fatalf("importdescriptors did not succeed:\nraw=%q", imported)
	}

	address := r.Node(t, "blake", "deriveaddresses", signed)

	var addresses []string

	if err := json.Unmarshal([]byte(address), &addresses); err != nil || len(addresses) == 0 {
		t.Fatalf("deriveaddresses: %v\n%s", err, address)
	}

	return walletAddressInfo(t, r, "blake", "recovered", addresses[0])
}

// planField reads one "label: value" line from the recovery plan.
func planField(t *testing.T, plan, label string) string {
	t.Helper()

	pattern := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(label) + `:\s+(\S+)\s*$`)
	match := pattern.FindStringSubmatch(plan)

	if len(match) != 2 {
		t.Fatalf("the plan has no %q line:\n%s", label, plan)
	}

	return match[1]
}

// wif encodes a secret as a compressed testnet private key, which is the form a
// descriptor's `pk()` accepts as a private key.
func wif(secret []byte) string {
	payload := append([]byte{0xef}, secret...)
	payload = append(payload, 0x01)

	first := sha256.Sum256(payload)
	second := sha256.Sum256(first[:])

	return base58(append(payload, second[:4]...))
}

// base58 encodes a byte string in Bitcoin's base58 alphabet.
func base58(input []byte) string {
	const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

	value := new(big.Int).SetBytes(input)
	radix := big.NewInt(58)

	var digits []byte

	for value.Sign() > 0 {
		remainder := new(big.Int)
		value.DivMod(value, radix, remainder)
		digits = append(digits, alphabet[remainder.Int64()])
	}

	for _, octet := range input {
		if octet != 0 {
			break
		}

		digits = append(digits, alphabet[0])
	}

	for left, right := 0, len(digits)-1; left < right; left, right = left+1, right-1 {
		digits[left], digits[right] = digits[right], digits[left]
	}

	return string(digits)
}
