// Package model is the durable swap record: its shape, its invariants, and the
// migration from the pre-base-unit format.
//
// It is the port of the Swap dataclass, make_swap, migrate_swap_record and
// Swap.validate in the Python engine. The record is the single source of truth
// for one participant's view of one swap, so the validation here is not
// defensive decoration: a malformed record that is accepted becomes a
// settlement built from the wrong facts.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/state"
)

// Swap is one participant's durable record of one swap.
//
// Optional fields are pointers so that "absent" and "empty" stay distinct on
// the wire and on disk, exactly as None and "" are distinct in Python.
type Swap struct {
	SwapID    string      `json:"swap_id"`
	State     state.State `json:"state"`
	CreatedAt int64       `json:"created_at"`

	// Economics. Base units are canonical; the decimal string is a rendering.
	BlakeAmountBaseUnits int64 `json:"blake_amount_base_units"`
	BTCSats              int64 `json:"btc_sats"`

	// The secret. Alice holds secret_hex from creation; Bob only ever learns
	// it from a Bitcoin claim, which is revealed_secret_hex.
	SecretHex         *string `json:"secret_hex"`
	SecretHash        string  `json:"secret_hash"`
	RevealedSecretHex *string `json:"revealed_secret_hex"`

	// Timeouts.
	BlakeTimeout int64 `json:"blake_timeout"`
	BTCTimeout   int64 `json:"btc_timeout"`

	TimeoutPolicyVersion      *string `json:"timeout_policy_version"`
	BTCTimeoutWindowSeconds   *int64  `json:"btc_timeout_window_seconds"`
	BlakeTimeoutWindowSeconds *int64  `json:"blake_timeout_window_seconds"`
	BobRecoverySeconds        *int64  `json:"bob_recovery_seconds"`

	// Recovery anchors: a confirmed funding cannot precede these heights.
	BlakeScanStartHeight *int64 `json:"blake_scan_start_height"`
	BTCScanStartHeight   *int64 `json:"btc_scan_start_height"`

	// Funding outpoints and their chain anchors.
	BlakeFundingTxID      *string `json:"blake_funding_txid"`
	BlakeFundingVout      *int32  `json:"blake_funding_vout"`
	BlakeFundingBlockHash *string `json:"blake_funding_blockhash"`
	BTCFundingTxID        *string `json:"btc_funding_txid"`
	BTCFundingVout        *int32  `json:"btc_funding_vout"`
	BTCFundingBlockHash   *string `json:"btc_funding_blockhash"`

	// Settlement public keys, per chain.
	BlakeAlicePub *string `json:"blake_alice_pub"`
	BlakeBobPub   *string `json:"blake_bob_pub"`
	BTCAlicePub   *string `json:"btc_alice_pub"`
	BTCBobPub     *string `json:"btc_bob_pub"`

	// Coordination identities.
	NostrAlicePub *string `json:"nostr_alice_pub"`
	NostrBobPub   *string `json:"nostr_bob_pub"`

	// Bitcoin settlement.
	BTCCLaimTxID       *string `json:"btc_claim_txid"`
	BTCCLaimBlockHash  *string `json:"btc_claim_blockhash"`
	BTCCLaimHex        *string `json:"btc_claim_hex"`
	BTCRefundTxID      *string `json:"btc_refund_txid"`
	BTCRefundBlockHash *string `json:"btc_refund_blockhash"`
	BTCRefundHex       *string `json:"btc_refund_hex"`

	// BLAKE settlement, including the signed hex kept for rebroadcast after a
	// crash or a mempool loss.
	BlakeClaimTxID       *string `json:"blake_claim_txid"`
	BlakeClaimBlockHash  *string `json:"blake_claim_blockhash"`
	BlakeClaimHex        *string `json:"blake_claim_hex"`
	BlakeRefundTxID      *string `json:"blake_refund_txid"`
	BlakeRefundBlockHash *string `json:"blake_refund_blockhash"`
	BlakeRefundHex       *string `json:"blake_refund_hex"`

	// A settlement that was rolled out of the active chain and is expected to
	// reappear; kept separately so the live txid can be cleared without losing
	// the identity.
	BlakeReorgClaimTxID  *string `json:"blake_reorg_claim_txid"`
	BlakeReorgRefundTxID *string `json:"blake_reorg_refund_txid"`
}

// NewParams is everything a caller must decide to create a swap.
type NewParams struct {
	// SwapID is optional; a random 12-hex identifier is generated when empty.
	SwapID string

	BlakeAmount amount.Blake
	BTCSats     int64

	BlakeTimeout int64
	BTCTimeout   int64

	// SecretHex and SecretHash must be a 32-byte secret and its SHA-256.
	// pkg/keys produces them from a crypto/rand source; the browser produces
	// them from WebCrypto.
	SecretHex  string
	SecretHash string

	// Chain heights observed when the swap is created. A confirmed funding
	// cannot legitimately precede them, so they bound the funding scan.
	BlakeScanStart int64
	BTCScanStart   int64

	// Timeout policy metadata, when a plan was used.
	PolicyVersion      string
	BTCTimeoutWindow   *int64
	BlakeTimeoutWindow *int64
	BobRecovery        *int64
}

// NewSwapID returns a random 12-hex-character protocol swap identifier.
//
// A swap ID is public coordination data: creating one does not create Alice's
// secret and does not imply either party has funded.
func NewSwapID(r io.Reader) (string, error) {
	var raw [6]byte

	if _, err := io.ReadFull(r, raw[:]); err != nil {
		return "", fmt.Errorf("swap id: %w", err)
	}

	return hex.EncodeToString(raw[:]), nil
}

// NewSecret returns a fresh 32-byte secret and its SHA-256 hash, both hex.
func NewSecret(r io.Reader) (secretHex, secretHash string, err error) {
	var secret [32]byte

	if _, err := io.ReadFull(r, secret[:]); err != nil {
		return "", "", fmt.Errorf("secret: %w", err)
	}

	hash := sha256.Sum256(secret[:])

	return hex.EncodeToString(secret[:]), hex.EncodeToString(hash[:]), nil
}

// New assembles a swap record and validates it.
func New(r io.Reader, p NewParams) (*Swap, error) {
	id := p.SwapID

	if id == "" {
		generated, err := NewSwapID(r)

		if err != nil {
			return nil, err
		}

		id = generated
	}

	if !IsSwapID(id) {
		return nil, fmt.Errorf("swap_id must be 12 hex characters")
	}

	secretHex := p.SecretHex
	secretHash := p.SecretHash

	if secretHex == "" || secretHash == "" {
		generatedSecret, generatedHash, err := NewSecret(r)

		if err != nil {
			return nil, err
		}

		secretHex, secretHash = generatedSecret, generatedHash
	}

	if p.BlakeAmount <= 0 {
		return nil, fmt.Errorf("BLAKE amount must be positive")
	}

	if p.BTCSats <= 0 {
		return nil, fmt.Errorf("BTC amount must be positive")
	}

	swap := &Swap{
		SwapID:               id,
		State:                state.Offered,
		CreatedAt:            time.Now().Unix(),
		BlakeAmountBaseUnits: int64(p.BlakeAmount),
		BTCSats:              p.BTCSats,
		SecretHex:            &secretHex,
		SecretHash:           secretHash,
		BlakeTimeout:         p.BlakeTimeout,
		BTCTimeout:           p.BTCTimeout,
	}

	if p.PolicyVersion != "" {
		swap.TimeoutPolicyVersion = &p.PolicyVersion
	}

	swap.BTCTimeoutWindowSeconds = p.BTCTimeoutWindow
	swap.BlakeTimeoutWindowSeconds = p.BlakeTimeoutWindow
	swap.BobRecoverySeconds = p.BobRecovery

	blakeStart, btcStart := p.BlakeScanStart, p.BTCScanStart
	swap.BlakeScanStartHeight = &blakeStart
	swap.BTCScanStartHeight = &btcStart

	if err := swap.Validate(); err != nil {
		return nil, err
	}

	return swap, nil
}

// IsSwapID reports whether s is a valid protocol swap identifier.
func IsSwapID(s string) bool {
	if len(s) != 12 {
		return false
	}

	// Lowercase only. The wire schema owns this rule and spells it
	// `^[0-9a-f]{12}$`; accepting uppercase here would let this port name a
	// swap in a file that no message could ever refer to, and the two
	// implementations would disagree about which records are canonical.
	for _, r := range s {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}

	return true
}

// IsHex32 reports whether s is 32 bytes of hex.
func IsHex32(s string) bool {
	if len(s) != 64 {
		return false
	}

	_, err := hex.DecodeString(s)

	return err == nil
}

// Validate enforces the record's invariants.
//
// The state field is checked here as well; the Python record does not check it
// on load, but a record whose state cannot be parsed is not a record this
// engine can act on, and refusing it at the boundary gives a better error than
// failing later inside a decision.
func (s *Swap) Validate() error {
	if !IsSwapID(s.SwapID) {
		return fmt.Errorf("swap_id must be 12 hex characters")
	}

	if _, err := state.Parse(string(s.State)); err != nil {
		return err
	}

	if !IsHex32(s.SecretHash) {
		return fmt.Errorf("secret_hash must be 32 bytes")
	}

	if s.SecretHex != nil {
		if !IsHex32(*s.SecretHex) {
			return fmt.Errorf("secret_hex must be 32 bytes")
		}

		hash := sha256.Sum256(mustHex(*s.SecretHex))

		if hex.EncodeToString(hash[:]) != s.SecretHash {
			return fmt.Errorf("secret_hex does not match secret_hash")
		}
	}

	if s.RevealedSecretHex != nil {
		if !IsHex32(*s.RevealedSecretHex) {
			return fmt.Errorf("revealed_secret_hex must be 32 bytes")
		}

		hash := sha256.Sum256(mustHex(*s.RevealedSecretHex))

		if hex.EncodeToString(hash[:]) != s.SecretHash {
			return fmt.Errorf("revealed_secret_hex does not match secret_hash")
		}
	}

	if s.BlakeAmountBaseUnits <= 0 {
		return fmt.Errorf("BLAKE amount must be positive")
	}

	if s.BTCSats <= 0 {
		return fmt.Errorf("BTC amount must be positive")
	}

	type hexField struct {
		name  string
		value *string
	}

	hexFields := []hexField{
		{"blake_funding_txid", s.BlakeFundingTxID},
		{"btc_funding_txid", s.BTCFundingTxID},
		{"btc_claim_txid", s.BTCCLaimTxID},
		{"blake_claim_txid", s.BlakeClaimTxID},
		{"btc_refund_txid", s.BTCRefundTxID},
		{"blake_refund_txid", s.BlakeRefundTxID},
		{"blake_reorg_claim_txid", s.BlakeReorgClaimTxID},
		{"blake_reorg_refund_txid", s.BlakeReorgRefundTxID},
		{"blake_funding_blockhash", s.BlakeFundingBlockHash},
		{"btc_funding_blockhash", s.BTCFundingBlockHash},
		{"btc_claim_blockhash", s.BTCCLaimBlockHash},
		{"blake_claim_blockhash", s.BlakeClaimBlockHash},
		{"btc_refund_blockhash", s.BTCRefundBlockHash},
		{"blake_refund_blockhash", s.BlakeRefundBlockHash},
	}

	for _, field := range hexFields {
		if field.value == nil {
			continue
		}

		if !IsHex32(*field.value) {
			return fmt.Errorf("%s must be 32-byte hex", field.name)
		}
	}

	type hexSettlement struct {
		name      string
		txHex     *string
		anchor    *string
		altAnchor *string
	}

	for _, field := range []hexSettlement{
		{"btc_claim_hex", s.BTCCLaimHex, s.BTCCLaimTxID, nil},
		{"btc_refund_hex", s.BTCRefundHex, s.BTCRefundTxID, nil},
		{"blake_claim_hex", s.BlakeClaimHex, s.BlakeClaimTxID, s.BlakeReorgClaimTxID},
		{"blake_refund_hex", s.BlakeRefundHex, s.BlakeRefundTxID, s.BlakeReorgRefundTxID},
	} {
		if field.txHex == nil {
			continue
		}

		hasAnchor := field.anchor != nil ||
			(field.altAnchor != nil && *field.altAnchor != "")

		if !hasAnchor {
			return fmt.Errorf(
				"%s requires a settlement txid identity anchor", field.name,
			)
		}

		if *field.txHex == "" {
			return fmt.Errorf("%s must be valid hex", field.name)
		}

		if _, err := hex.DecodeString(*field.txHex); err != nil {
			return fmt.Errorf("%s must be valid hex", field.name)
		}
	}

	return nil
}

// Transition applies a forward state transition to the record.
func (s *Swap) Transition(to state.State) error {
	next, err := state.Transition(s.State, to)

	if err != nil {
		return err
	}

	s.State = next

	return nil
}

// ApplyReorg applies a reorganisation correction to the record.
func (s *Swap) ApplyReorg(to state.State) error {
	next, err := state.ApplyReorg(s.State, to)

	if err != nil {
		return err
	}

	s.State = next

	return nil
}

// BlakeAmount returns the canonical BLAKE amount.
func (s *Swap) BlakeAmount() amount.Blake {
	return amount.Blake(s.BlakeAmountBaseUnits)
}

// Marshal renders the record as JSON, one line, keys in field order.
func (s *Swap) Marshal() ([]byte, error) {
	return json.Marshal(s)
}

// Decode parses a stored record, applying the legacy migration first.
//
// `warn` is called for anything the migration changed or ignored; nil
// silences it. Reading is non-destructive: the caller decides when to write the
// migrated shape back.
func Decode(data []byte, warn func(string)) (*Swap, error) {
	var record map[string]any

	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("decode swap record: %w", err)
	}

	if err := Migrate(record, warn); err != nil {
		return nil, err
	}

	reencoded, err := json.Marshal(record)

	if err != nil {
		return nil, err
	}

	var swap Swap

	if err := json.Unmarshal(reencoded, &swap); err != nil {
		return nil, fmt.Errorf("decode swap record: %w", err)
	}

	if err := swap.Validate(); err != nil {
		return nil, err
	}

	return &swap, nil
}

// Migrate brings one stored record up to the current shape, in place.
//
// The only migration so far is BLAKE amounts: records written before the change
// carry `blake_amount` as a JSON number (a Python float), and records written
// after it carry `blake_amount_base_units` as an integer. A legacy value is
// converted exactly where that is possible; where the float cannot be
// represented in base units at all it is rounded to the nearest one and the
// change is reported, because silently discarding value would be worse than
// saying so.
func Migrate(record map[string]any, warn func(string)) error {
	legacy, hadLegacy := record["blake_amount"]
	delete(record, "blake_amount")

	units, hasUnits := record["blake_amount_base_units"]

	switch {
	case hasUnits && units != nil:
		if !hadLegacy || legacy == nil {
			return nil
		}

		//
		// Both present: the integer is canonical, but a disagreement means
		// the record was hand-edited or half-written. Trust the integer, and
		// say so rather than picking silently.
		//
		expected, err := amount.ParseBlake(legacy)

		if err != nil {
			return nil
		}

		actual, ok := toInt64(units)

		if ok && expected != amount.Blake(actual) && warn != nil {
			warn("stored blake_amount disagrees with blake_amount_base_units; using the integer")
		}

		return nil

	case hadLegacy && legacy != nil:
		blake, rounded, err := amount.ParseBlakeLegacy(legacy)

		if err != nil {
			return fmt.Errorf("legacy BLAKE amount: %w", err)
		}

		record["blake_amount_base_units"] = int64(blake)

		if rounded && warn != nil {
			warn(fmt.Sprintf(
				"migrated legacy BLAKE amount %v to %d base units (%s BLAKE), which is the nearest representable value",
				legacy, int64(blake), blake.Format(),
			))
		}
	}

	return nil
}

// toInt64 reads an integer that may have arrived from JSON as float64.
func toInt64(v any) (int64, bool) {
	switch value := v.(type) {
	case int64:
		return value, true
	case int:
		return int64(value), true
	case float64:
		if value != float64(int64(value)) {
			return 0, false
		}

		return int64(value), true
	case json.Number:
		n, err := value.Int64()

		return n, err == nil
	default:
		return 0, false
	}
}

func mustHex(s string) []byte {
	raw, err := hex.DecodeString(s)

	if err != nil {
		// Validate has already refused anything that is not hex, so this
		// cannot happen on a validated record.
		panic(err)
	}

	return raw
}
