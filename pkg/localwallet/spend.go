package localwallet

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"git.smesh.lol/tinywallet/pkg/bech32"
	"git.smesh.lol/tinywallet/pkg/psbt"
	"git.smesh.lol/tinywallet/pkg/script"
	"git.smesh.lol/tinywallet/pkg/tx"

	"github.com/gjbrr/blake-swap/pkg/contract"
	"github.com/gjbrr/blake-swap/pkg/settle"
)

// SpendContract builds and finalises a contract spend the way a local wallet can: the coin, the
// witness script and the preimage are all known here, so no descriptor has to be filled in by a
// node. The result is final hex; broadcasting stays the caller's decision.
func (n *Node) SpendContract(ctx context.Context, req settle.LocalSpendRequest) (string, error) {
	terms, err := ParseDescriptor(req.Descriptor)

	if err != nil {
		return "", err
	}

	witness, err := script.HTLC(terms.Claimer, terms.Refunder, terms.SecretHash, terms.Locktime)

	if err != nil {
		return "", fmt.Errorf("localwallet: %w", err)
	}

	outputScript := script.P2WSH(witness)

	coins, err := n.Wallet.Coins(ctx, string(req.Chain))

	if err != nil {
		return "", err
	}

	var (
		found bool
		value uint64
	)

	for _, coin := range coins {
		if coin.TxID != req.Funding.TxID || coin.Vout != uint32(req.Funding.Vout) {
			continue
		}

		if string(coin.Script) != string(outputScript) {
			return "", fmt.Errorf("localwallet: the funding outpoint does not pay this contract")
		}

		value = coin.Value
		found = true

		break
	}

	if !found {
		return "", fmt.Errorf("localwallet: the funding outpoint is not one of the wallet's coins")
	}

	if int64(value) != req.InputSats {
		return "", fmt.Errorf("localwallet: the contract holds %d, the plan expects %d", value, req.InputSats)
	}

	if req.FeeSats < 0 || req.FeeSats >= req.InputSats {
		return "", fmt.Errorf("localwallet: a fee of %d on %d", req.FeeSats, req.InputSats)
	}

	_, version, program, err := bech32.DecodeSegwit(req.Destination)

	if err != nil {
		return "", fmt.Errorf("localwallet: destination: %w", err)
	}

	if version != 0 || len(program) != 20 {
		return "", fmt.Errorf("localwallet: the destination is not a P2WPKH address")
	}

	claim := tx.New()
	claim.AddInput(tx.Outpoint{Hash: parseTxID(req.Funding.TxID), Index: uint32(req.Funding.Vout)}, 0xfffffffe)
	claim.AddOutput(uint64(req.InputSats-req.FeeSats), script.P2WPKH([20]byte(program)))

	if req.Refund {
		claim.LockTime = uint32(req.LockTime)
	}

	segwitInput := psbt.SegwitInput{
		Amount:        uint64(req.InputSats),
		ScriptPubKey:  outputScript,
		WitnessScript: witness,
	}

	if !req.Refund {
		preimage, err := hex.DecodeString(req.Preimage)

		if err != nil {
			return "", fmt.Errorf("localwallet: preimage: %w", err)
		}

		hash, err := hex.DecodeString(req.SecretHash)

		if err != nil || len(hash) != 32 {
			return "", fmt.Errorf("localwallet: secret hash")
		}

		var secretHash [32]byte

		copy(secretHash[:], hash)

		segwitInput.Preimage = preimage
		segwitInput.SecretHash = secretHash
	}

	unsigned, err := psbt.Write(claim, []psbt.SegwitInput{segwitInput})

	if err != nil {
		return "", fmt.Errorf("localwallet: build spend: %w", err)
	}

	signed, count, err := n.Wallet.SignPSBT(ctx, string(req.Chain), unsigned, Policy{
		MaxSpend: uint64(req.InputSats),
		MaxFee:   uint64(req.FeeSats),
	})

	if err != nil {
		return "", fmt.Errorf("localwallet: sign spend: %w", err)
	}

	if count != 1 {
		return "", fmt.Errorf("localwallet: the wallet signed %d inputs", count)
	}

	items, err := psbt.FinalWitness(signed)

	if err != nil {
		return "", fmt.Errorf("localwallet: %w", err)
	}

	claim.Inputs[0].Witness = items

	raw, err := claim.SerializeWitness()

	if err != nil {
		return "", err
	}

	return hex.EncodeToString(raw), nil
}

// ParseDescriptor reads the HTLC terms out of the descriptor the swap builds.
//
// The template is the one pkg/htlc renders and the node compiles; this is the reverse direction,
// and it exists because a local wallet has no descriptor compiler to ask.
func ParseDescriptor(descriptor string) (params contract.Terms, err error) {
	body := descriptor

	if at := strings.Index(body, "#"); at >= 0 {
		body = body[:at]
	}

	if !strings.HasPrefix(body, "wsh(") || !strings.HasSuffix(body, ")") {
		return contract.Terms{}, fmt.Errorf("localwallet: %q is not a wsh descriptor", descriptor)
	}

	keys := keyOccurrences(body)

	if len(keys) != 2 {
		return contract.Terms{}, fmt.Errorf("localwallet: %d public keys in the descriptor", len(keys))
	}

	hashAt := strings.Index(body, "sha256(")

	if hashAt < 0 {
		return contract.Terms{}, fmt.Errorf("localwallet: the descriptor has no sha256")
	}

	hashText := body[hashAt+len("sha256("):]

	if end := strings.Index(hashText, ")"); end >= 0 {
		hashText = hashText[:end]
	}

	afterAt := strings.Index(body, "after(")

	if afterAt < 0 {
		return contract.Terms{}, fmt.Errorf("localwallet: the descriptor has no after")
	}

	lockText := body[afterAt+len("after("):]

	if end := strings.Index(lockText, ")"); end >= 0 {
		lockText = lockText[:end]
	}

	locktime, err := strconv.ParseUint(strings.TrimSpace(lockText), 10, 32)

	if err != nil || locktime == 0 {
		return contract.Terms{}, fmt.Errorf("localwallet: descriptor locktime %q", lockText)
	}

	claim, err := hex.DecodeString(keys[0])

	if err != nil {
		return contract.Terms{}, fmt.Errorf("localwallet: claimer key: %w", err)
	}

	refund, err := hex.DecodeString(keys[1])

	if err != nil {
		return contract.Terms{}, fmt.Errorf("localwallet: refunder key: %w", err)
	}

	raw, err := hex.DecodeString(strings.TrimSpace(hashText))

	if err != nil || len(raw) != 32 {
		return contract.Terms{}, fmt.Errorf("localwallet: secret hash %q", hashText)
	}

	var terms contract.Terms

	terms.Claimer = claim
	terms.Refunder = refund
	copy(terms.SecretHash[:], raw)
	terms.Locktime = uint32(locktime)

	return terms, nil
}

// keyOccurrences returns the public keys inside pk(...) in order of appearance.
func keyOccurrences(body string) []string {
	var keys []string

	rest := body

	for {
		at := strings.Index(rest, "pk(")

		if at < 0 {
			return keys
		}

		rest = rest[at+len("pk("):]

		end := strings.Index(rest, ")")

		if end < 0 {
			return keys
		}

		keys = append(keys, rest[:end])
		rest = rest[end:]
	}
}

// parseTxID reverses a displayed transaction id into wire order.
func parseTxID(text string) [32]byte {
	var out [32]byte

	raw, err := hex.DecodeString(text)

	if err != nil || len(raw) != 32 {
		return out
	}

	for i, b := range raw {
		out[len(raw)-1-i] = b
	}

	return out
}
