// Package wire is the swap coordination wire format.
//
// It is the port of swap_wire.py, and it deliberately knows nothing about
// wallets, nodes, relays, local state or the state machine. Its job is
// narrower: bytes to a structurally valid protocol message, and back to
// bytes in one canonical form.
//
// The Python module's strictness is preserved because it is what makes two
// independent implementations agree: unknown fields are refused, duplicated
// JSON keys are refused, an integer field that arrives as a float is refused,
// `secret_hex` is forbidden on the wire, and an amount finer than one base unit
// is refused rather than rounded.
package wire

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/gjbrr/blake-swap/pkg/decimal"
)

// Kind identifies a coordination message.
type Kind string

// The seven settlement message types.
const (
	KindOffer        Kind = "offer"
	KindAccept       Kind = "accept"
	KindBlakeFunded  Kind = "blake-funded"
	KindBTCFunded    Kind = "btc-funded"
	KindBTCCLaimed   Kind = "btc-claimed"
	KindBlakeClaimed Kind = "blake-claimed"
	KindBTCRefunded  Kind = "btc-refunded"
)

// SettlementTypes is every accepted type, in the schema's order.
var SettlementTypes = []Kind{
	KindOffer,
	KindAccept,
	KindBlakeFunded,
	KindBTCFunded,
	KindBTCCLaimed,
	KindBlakeClaimed,
	KindBTCRefunded,
}

// AmountDecimals is the protocol's smallest unit, matching the chain.
const AmountDecimals = 8

var (
	swapIDPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)
	hex32Pattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	pubkeyPattern = regexp.MustCompile(`^(02|03)[0-9a-f]{64}$`)
)

// WireError is a structural refusal. It is distinct from an engine refusal so
// callers can tell "this message is not a protocol message" from "this protocol
// message is not acceptable now".
type WireError struct{ msg string }

func (e *WireError) Error() string { return e.msg }

func refuse(format string, args ...any) error {
	return &WireError{msg: fmt.Sprintf(format, args...)}
}

// required is the required-field set for each type. Every type has exactly the
// fields it needs and no optional ones, which is what lets unknown-field
// rejection be total.
var required = map[Kind][]string{
	KindOffer: {
		"type", "swap_id", "blake_amount", "btc_sats", "secret_hash",
		"btc_timeout", "blake_timeout", "created_at", "blake_alice_pub",
		"btc_alice_pub", "timeout_policy_version", "btc_timeout_window_seconds",
		"blake_timeout_window_seconds", "bob_recovery_seconds",
	},
	KindAccept: {
		"type", "swap_id", "secret_hash", "blake_bob_pub", "btc_bob_pub",
	},
	KindBlakeFunded: {
		"type", "swap_id", "secret_hash", "blake_amount", "blake_timeout",
		"blake_funding_txid", "blake_funding_vout",
	},
	KindBTCFunded: {
		"type", "swap_id", "secret_hash", "btc_sats", "btc_timeout",
		"btc_funding_txid", "btc_funding_vout",
	},
	KindBTCCLaimed: {
		"type", "swap_id", "secret_hash", "btc_claim_txid",
		"btc_claim_blockhash",
	},
	KindBlakeClaimed: {
		"type", "swap_id", "secret_hash", "blake_claim_txid",
		"blake_claim_blockhash",
	},
	KindBTCRefunded: {
		"type", "swap_id", "secret_hash", "btc_refund_txid",
		"btc_refund_blockhash",
	},
}

// RequiredFields returns a copy of the required-field set for a type. It exists
// so tests and independent implementations can check the schema itself.
func RequiredFields(k Kind) []string {
	out := make([]string, len(required[k]))
	copy(out, required[k])
	sort.Strings(out)

	return out
}

// Validate checks one decoded message and returns its type.
//
// `expected` may be nil, or a type the message must match: the engine uses that
// to refuse a message of the right shape but the wrong stage.
func Validate(message any, expected *Kind) (Kind, error) {
	object, ok := message.(map[string]any)

	if !ok {
		return "", refuse("message must be a JSON object")
	}

	if _, forbidden := object["secret_hex"]; forbidden {
		return "", refuse("secret_hex is forbidden on the wire")
	}

	msgType, err := requireString(object, "type")

	if err != nil {
		return "", err
	}

	kind := Kind(msgType)

	if !known(kind) {
		return "", refuse("unsupported settlement message type: %q", msgType)
	}

	if expected != nil && kind != *expected {
		return "", refuse("message type does not match expected type")
	}

	swapID, err := requireString(object, "swap_id")

	if err != nil {
		return "", err
	}

	if !swapIDPattern.MatchString(swapID) {
		return "", refuse("swap_id must be 12 lowercase hex characters")
	}

	allowed := map[string]bool{}

	for _, field := range required[kind] {
		allowed[field] = true

		if _, present := object[field]; !present {
			return "", refuse("missing required field(s): %s", field)
		}
	}

	var unknown []string

	for field := range object {
		if !allowed[field] {
			unknown = append(unknown, field)
		}
	}

	if len(unknown) > 0 {
		sort.Strings(unknown)

		return "", refuse("unknown field(s): %s", strings.Join(unknown, ", "))
	}

	if err := validateFields(kind, object); err != nil {
		return "", err
	}

	return kind, nil
}

func known(kind Kind) bool {
	for _, candidate := range SettlementTypes {
		if candidate == kind {
			return true
		}
	}

	return false
}

func validateFields(kind Kind, message map[string]any) error {
	switch kind {
	case KindOffer:
		if err := requireAmount(message, "blake_amount"); err != nil {
			return err
		}

		if _, err := requirePositiveInt(message, "btc_sats"); err != nil {
			return err
		}

		if _, err := requireHex32(message, "secret_hash"); err != nil {
			return err
		}

		if _, err := requirePositiveInt(message, "btc_timeout"); err != nil {
			return err
		}

		if _, err := requirePositiveInt(message, "blake_timeout"); err != nil {
			return err
		}

		if _, err := requireNonNegativeInt(message, "created_at"); err != nil {
			return err
		}

		if _, err := requirePubkey(message, "blake_alice_pub"); err != nil {
			return err
		}

		if _, err := requirePubkey(message, "btc_alice_pub"); err != nil {
			return err
		}

		version, err := requireString(message, "timeout_policy_version")

		if err != nil {
			return err
		}

		if version != "1.5" {
			return refuse("unsupported timeout_policy_version")
		}

		btcWindow, err := requirePositiveInt(message, "btc_timeout_window_seconds")

		if err != nil {
			return err
		}

		blakeWindow, err := requirePositiveInt(message, "blake_timeout_window_seconds")

		if err != nil {
			return err
		}

		recovery, err := requirePositiveInt(message, "bob_recovery_seconds")

		if err != nil {
			return err
		}

		if blakeWindow-btcWindow < recovery {
			return refuse("v1.5 timeout metadata does not preserve recovery budget")
		}

	case KindAccept:
		if _, err := requireHex32(message, "secret_hash"); err != nil {
			return err
		}

		if _, err := requirePubkey(message, "blake_bob_pub"); err != nil {
			return err
		}

		if _, err := requirePubkey(message, "btc_bob_pub"); err != nil {
			return err
		}

	case KindBlakeFunded:
		if _, err := requireHex32(message, "secret_hash"); err != nil {
			return err
		}

		if err := requireAmount(message, "blake_amount"); err != nil {
			return err
		}

		if _, err := requirePositiveInt(message, "blake_timeout"); err != nil {
			return err
		}

		if _, err := requireHex32(message, "blake_funding_txid"); err != nil {
			return err
		}

		if _, err := requireNonNegativeInt(message, "blake_funding_vout"); err != nil {
			return err
		}

	case KindBTCFunded:
		if _, err := requireHex32(message, "secret_hash"); err != nil {
			return err
		}

		if _, err := requirePositiveInt(message, "btc_sats"); err != nil {
			return err
		}

		if _, err := requirePositiveInt(message, "btc_timeout"); err != nil {
			return err
		}

		if _, err := requireHex32(message, "btc_funding_txid"); err != nil {
			return err
		}

		if _, err := requireNonNegativeInt(message, "btc_funding_vout"); err != nil {
			return err
		}

	case KindBTCCLaimed:
		for _, field := range []string{"secret_hash", "btc_claim_txid", "btc_claim_blockhash"} {
			if _, err := requireHex32(message, field); err != nil {
				return err
			}
		}

	case KindBlakeClaimed:
		for _, field := range []string{"secret_hash", "blake_claim_txid", "blake_claim_blockhash"} {
			if _, err := requireHex32(message, field); err != nil {
				return err
			}
		}

	case KindBTCRefunded:
		for _, field := range []string{"secret_hash", "btc_refund_txid", "btc_refund_blockhash"} {
			if _, err := requireHex32(message, field); err != nil {
				return err
			}
		}
	}

	return nil
}

func requireString(message map[string]any, field string) (string, error) {
	value, present := message[field]

	if !present {
		return "", refuse("%s must be a string", field)
	}

	text, ok := value.(string)

	if !ok {
		return "", refuse("%s must be a string", field)
	}

	return text, nil
}

// integer reads a JSON integer.
//
// Decoding uses json.Number, so the distinction Python gets from json.loads is
// preserved: "25000" is an integer, "25000.0" and "true" are not.
func integer(message map[string]any, field string) (int64, error) {
	value, present := message[field]

	if !present {
		return 0, refuse("%s must be an integer", field)
	}

	number, ok := value.(json.Number)

	if !ok {
		return 0, refuse("%s must be an integer", field)
	}

	n, err := number.Int64()

	if err != nil {
		return 0, refuse("%s must be an integer", field)
	}

	return n, nil
}

func requirePositiveInt(message map[string]any, field string) (int64, error) {
	n, err := integer(message, field)

	if err != nil {
		return 0, err
	}

	if n <= 0 {
		return 0, refuse("%s must be positive", field)
	}

	return n, nil
}

func requireNonNegativeInt(message map[string]any, field string) (int64, error) {
	n, err := integer(message, field)

	if err != nil {
		return 0, err
	}

	if n < 0 {
		return 0, refuse("%s must be non-negative", field)
	}

	return n, nil
}

func requireHex32(message map[string]any, field string) (string, error) {
	value, err := requireString(message, field)

	if err != nil {
		return "", err
	}

	if !hex32Pattern.MatchString(value) {
		return "", refuse("%s must be 32-byte lowercase hex", field)
	}

	return value, nil
}

func requirePubkey(message map[string]any, field string) (string, error) {
	value, err := requireString(message, field)

	if err != nil {
		return "", err
	}

	if !pubkeyPattern.MatchString(value) {
		return "", refuse("%s must be a compressed secp256k1 public key", field)
	}

	return value, nil
}

func requireAmount(message map[string]any, field string) error {
	value, err := requireString(message, field)

	if err != nil {
		return err
	}

	parsed, err := decimal.Parse(value)

	if err != nil {
		return refuse("%s must be a decimal string", field)
	}

	if parsed.Sign() <= 0 {
		return refuse("%s must be positive", field)
	}

	if !parsed.Shift(AmountDecimals).IsInteger() {
		return refuse("%s must not be finer than %d decimal places", field, AmountDecimals)
	}

	return nil
}

// FormatAmount renders a wire amount canonically: a plain decimal string, no
// exponent notation, no trailing zeros, never finer than a base unit.
func FormatAmount(value any) (string, error) {
	parsed, err := decimalFrom(value)

	if err != nil {
		return "", refuse("amount %v is not a decimal number", value)
	}

	if parsed.Sign() <= 0 {
		return "", refuse("amount must be a positive finite decimal")
	}

	return parsed.String(), nil
}

func decimalFrom(value any) (decimal.Dec, error) {
	switch v := value.(type) {
	case decimal.Dec:
		return v, nil
	case string:
		return decimal.Parse(v)
	case int:
		return decimal.FromInt64(int64(v)), nil
	case int64:
		return decimal.FromInt64(v), nil
	case float64:
		return decimal.FromFloat64(v)
	default:
		return decimal.Dec{}, fmt.Errorf("not a decimal")
	}
}

// SHA256Hex is a small helper independent implementations find useful.
func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

// --- decoding ---------------------------------------------------------------

// Decode parses and validates one JSON wire message.
//
// Duplicate JSON object keys are rejected at every level, as the Python decoder
// rejects them: a payload that means two things is not a protocol message.
func Decode(data []byte, expected *Kind) (map[string]any, error) {
	if !utf8.Valid(data) {
		return nil, refuse("wire message is not valid UTF-8")
	}

	if err := rejectDuplicateKeys(data); err != nil {
		return nil, err
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	var decoded any

	if err := decoder.Decode(&decoded); err != nil {
		return nil, refuse("invalid JSON")
	}

	// Reject trailing content after the object.
	var trailing any

	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, refuse("invalid JSON")
	}

	if _, err := Validate(decoded, expected); err != nil {
		return nil, err
	}

	object, _ := decoded.(map[string]any)

	// A shallow copy, so a caller mutating its input is not part of validation.
	out := make(map[string]any, len(object))

	for key, value := range object {
		out[key] = value
	}

	return out, nil
}

// Encode validates then serialises to the canonical form used inside signed
// Nostr event content: compact, keys sorted, no ASCII escaping.
func Encode(message any, expected *Kind) ([]byte, error) {
	if _, err := Validate(message, expected); err != nil {
		return nil, err
	}

	object, _ := message.(map[string]any)

	var buffer bytes.Buffer

	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(object); err != nil {
		return nil, err
	}

	encoded := bytes.TrimRight(buffer.Bytes(), "\n")

	return encoded, nil
}

// rejectDuplicateKeys walks the JSON token stream and refuses repeated keys in
// any object.
func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	if err := scanValue(decoder); err != nil {
		return refuse("invalid JSON")
	}

	return nil
}

func scanValue(decoder *json.Decoder) error {
	token, err := decoder.Token()

	if err != nil {
		return err
	}

	delim, ok := token.(json.Delim)

	if !ok {
		return nil
	}

	switch delim {
	case '{':
		seen := map[string]bool{}

		for decoder.More() {
			keyToken, err := decoder.Token()

			if err != nil {
				return err
			}

			key, _ := keyToken.(string)

			if seen[key] {
				return refuse("duplicate JSON key: %s", key)
			}

			seen[key] = true

			if err := scanValue(decoder); err != nil {
				return err
			}
		}

		_, err = decoder.Token()

		return err

	case '[':
		for decoder.More() {
			if err := scanValue(decoder); err != nil {
				return err
			}
		}

		_, err = decoder.Token()

		return err
	}

	return nil
}
