package swap

import (
	"context"
	"fmt"
	"io"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/coord"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/state"
	"github.com/gjbrr/blake-swap/pkg/store"
	"github.com/gjbrr/blake-swap/pkg/timeout"
)

// NewRequest is what a caller decides when creating a swap.
type NewRequest struct {
	// BlakeAmount is the trade size in BLAKE base units.
	BlakeAmount amount.Blake
	// BTCSats is the trade size in satoshis.
	BTCSats int64
	// SwapID is optional; a random identifier is generated when empty.
	SwapID string
	// BlakeTimeout and BTCTimeout are an explicit pair, for regtest and tests.
	// Supplying only one is refused: half a contract is not a contract.
	BlakeTimeout int64
	BTCTimeout   int64
	// NostrBobPub pins the counterparty's authenticated identity when the swap was
	// negotiated through a signed market take. Left empty, Bob's identity is learned
	// later from his own accept message, which is how a direct handshake works.
	NostrBobPub string
	// Random is the entropy source for the secret and the identifier.
	Random io.Reader
	// Policy overrides the timeout policy. Zero means DefaultPolicy.
	Policy timeout.Policy
}

// NewResult is what a created swap looks like.
type NewResult struct {
	Swap     *model.Swap
	Plan     timeout.Plan
	Archived []string
}

// New creates a swap as protocol Alice.
//
// The timeouts are the whole point of this command. Without them the swap is a
// trade with no deadline, and both sides' recovery paths are undefined; so the
// heights come from the policy plan against *current* chain tips unless the caller
// supplies an explicit pair, which exists for regtest and is still validated as a
// coherent proposal.
func (e *Engine) New(ctx context.Context, req NewRequest) (*NewResult, error) {
	if req.BlakeAmount <= 0 {
		return nil, fmt.Errorf("BLAKE amount must be positive")
	}

	if req.BTCSats <= 0 {
		return nil, fmt.Errorf("BTC amount must be positive")
	}

	if e.Blake == nil || e.BTC == nil {
		return nil, fmt.Errorf("the engine needs both chain nodes")
	}

	blakeHeight, err := e.Blake.Height(ctx)

	if err != nil {
		return nil, err
	}

	btcHeight, err := e.BTC.Height(ctx)

	if err != nil {
		return nil, err
	}

	policy := req.Policy

	if policy.BTCBlockSeconds == 0 {
		policy = timeout.DefaultPolicy()
	}

	plan := timeout.Plan{}

	params := model.NewParams{
		SwapID:         req.SwapID,
		BlakeAmount:    req.BlakeAmount,
		BTCSats:        req.BTCSats,
		BlakeScanStart: blakeHeight,
		BTCScanStart:   btcHeight,
	}

	explicitBlake := req.BlakeTimeout > 0
	explicitBTC := req.BTCTimeout > 0

	if explicitBlake != explicitBTC {
		return nil, fmt.Errorf(
			"either supply both --btc-timeout and --blake-timeout, or supply neither",
		)
	}

	if explicitBlake {
		params.BlakeTimeout = req.BlakeTimeout
		params.BTCTimeout = req.BTCTimeout

		//
		// An explicit pair is a test override, but the offer it produces must
		// still be well formed: a peer refuses an offer with no v1.5 metadata, so
		// advertise the windows the supplied heights actually imply.
		//
		btcWindow := maxInt64(0, req.BTCTimeout-btcHeight) * int64(policy.BTCBlockSeconds)
		blakeWindow := maxInt64(0, req.BlakeTimeout-blakeHeight) * int64(policy.BlakeBlockSeconds)

		params.PolicyVersion = "1.5"
		params.BTCTimeoutWindow = &btcWindow
		params.BlakeTimeoutWindow = &blakeWindow

		recovery := blakeWindow - btcWindow
		if recovery < 0 {
			recovery = 0
		}

		params.BobRecovery = &recovery

		plan = timeout.Plan{
			BTCStartHeight: btcHeight, BlakeStartHeight: blakeHeight,
			BTCTimeoutHeight: req.BTCTimeout, BlakeTimeoutHeight: req.BlakeTimeout,
			BTCWindowSeconds: btcWindow, BlakeWindowSeconds: blakeWindow,
			BobRecoverySeconds: recovery,
		}
	} else {
		computed, err := timeout.BuildPlan(btcHeight, blakeHeight, policy)

		if err != nil {
			return nil, err
		}

		if err := timeout.ValidatePlan(computed, policy); err != nil {
			return nil, err
		}

		plan = computed

		params.BlakeTimeout = computed.BlakeTimeoutHeight
		params.BTCTimeout = computed.BTCTimeoutHeight
		params.PolicyVersion = "1.5"
		params.BTCTimeoutWindow = &computed.BTCWindowSeconds
		params.BlakeTimeoutWindow = &computed.BlakeWindowSeconds
		params.BobRecovery = &computed.BobRecoverySeconds
	}

	if req.Random == nil {
		return nil, fmt.Errorf("creating a swap requires an entropy source")
	}

	swap, err := model.New(req.Random, params)

	if err != nil {
		return nil, err
	}

	//
	// The creator of a normal swap is protocol Alice, and her own participant
	// identity is pinned immediately: a swap whose Alice identity is unknown could
	// never be published or funded, and pinning it later would be a second chance
	// for a record to disagree with itself.
	//
	if e.Identity != nil {
		alice := e.Identity.PublicKey()
		swap.NostrAlicePub = &alice
	}

	// A market take is a signed statement of who the counterparty is, so when the
	// caller supplies that identity it is pinned here rather than left to a later
	// message that a different machine could sign.
	if req.NostrBobPub != "" {
		bob := req.NostrBobPub
		swap.NostrBobPub = &bob
	}

	if err := ssave(e, ctx, swap, store.Alice); err != nil {
		return nil, err
	}

	archived, err := e.archiveSuperseded(ctx, swap.SwapID)

	if err != nil {
		return nil, err
	}

	return &NewResult{Swap: swap, Plan: plan, Archived: archived}, nil
}

// ssave saves a record as the given role without changing the engine's default.
func ssave(e *Engine, ctx context.Context, swap *model.Swap, role string) error {
	previous := e.Role
	e.Role = role

	defer func() { e.Role = previous }()

	return e.save(ctx, swap)
}

// archiveSuperseded archives the unannounced placeholders a new swap supersedes.
func (e *Engine) archiveSuperseded(ctx context.Context, keep string) ([]string, error) {
	return store.ArchiveSuperseded(ctx, e.Store, keep, store.Alice)
}

// PublishOffer publishes Alice's offer.
//
// Alice creates only her own contract keys, from her own wallet. Bob's keys are
// his to create, and a swap where one side named the other's keys would let the
// naming side pick a key it controls.
func (e *Engine) PublishOffer(ctx context.Context, swapID string) (*MessageResult, error) {
	swap, err := e.load(ctx, swapID)

	if err != nil {
		return nil, err
	}

	if _, err := e.requireRole(swap, "alice", "offer publication"); err != nil {
		return nil, err
	}

	if swap.State != state.Offered {
		return nil, fmt.Errorf("publish-offer requires Alice state OFFERED")
	}

	if swap.BlakeAlicePub == nil || *swap.BlakeAlicePub == "" {
		pubkey, err := e.walletPubkey(ctx, chain.Blake)

		if err != nil {
			return nil, err
		}

		swap.BlakeAlicePub = &pubkey
	}

	if swap.BTCAlicePub == nil || *swap.BTCAlicePub == "" {
		pubkey, err := e.walletPubkey(ctx, chain.BTC)

		if err != nil {
			return nil, err
		}

		swap.BTCAlicePub = &pubkey
	}

	if err := e.save(ctx, swap); err != nil {
		return nil, err
	}

	body := map[string]any{
		"type":            "offer",
		"swap_id":         swap.SwapID,
		"blake_amount":    amount.Blake(swap.BlakeAmountBaseUnits).Format(),
		"btc_sats":        swap.BTCSats,
		"secret_hash":     swap.SecretHash,
		"btc_timeout":     swap.BTCTimeout,
		"blake_timeout":   swap.BlakeTimeout,
		"created_at":      swap.CreatedAt,
		"blake_alice_pub": *swap.BlakeAlicePub,
		"btc_alice_pub":   *swap.BTCAlicePub,
	}

	if swap.TimeoutPolicyVersion != nil && *swap.TimeoutPolicyVersion == "1.5" {
		body["timeout_policy_version"] = "1.5"

		if swap.BTCTimeoutWindowSeconds != nil {
			body["btc_timeout_window_seconds"] = *swap.BTCTimeoutWindowSeconds
		}

		if swap.BlakeTimeoutWindowSeconds != nil {
			body["blake_timeout_window_seconds"] = *swap.BlakeTimeoutWindowSeconds
		}

		if swap.BobRecoverySeconds != nil {
			body["bob_recovery_seconds"] = *swap.BobRecoverySeconds
		}
	}

	location, err := e.send(ctx, swap, "offer", body)

	if err != nil {
		return nil, err
	}

	return &MessageResult{Kind: "offer", Location: location, State: swap.State}, nil
}

// walletPubkey returns a fresh public key from the signing wallet.
//
// The contract keys are the participant's wallet keys, not a second key store: the
// wallet is what will sign the settlement, so the HTLC has to name a key it holds.
func (e *Engine) walletPubkey(ctx context.Context, ch chain.Chain) (string, error) {
	wallet := e.walletForSettlement(ch)

	bound := e.nodeFor(ch).Bind(wallet)

	address, err := bound.NewAddress(ctx)

	if err != nil {
		return "", err
	}

	info, err := bound.AddressInfo(ctx, address)

	if err != nil {
		return "", err
	}

	if info.Pubkey == "" {
		return "", fmt.Errorf("could not obtain a pubkey from the %s wallet", chainLabel(ch))
	}

	return info.Pubkey, nil
}

// AcceptRequest is what Bob decides when taking an offer.
type AcceptRequest struct {
	// SwapID is the offer's swap id.
	SwapID string
	// ExpectedAlicePub pins the maker. Over relays an unpinned author is refused
	// unless AllowUnpinnedAuthor is set explicitly.
	ExpectedAlicePub string
	// AllowUnpinnedAuthor accepts whichever author answers first. It exists
	// because the reference allows it explicitly, and reaching it by omission
	// would be a way to accept a stranger's offer.
	AllowUnpinnedAuthor bool
	// ExpectedBlakeAmount and ExpectedBTCSats pin the trade a market match
	// already authenticated.
	ExpectedBlakeAmount *amount.Blake
	ExpectedBTCSats     *int64
	// Policy overrides the timeout policy. Zero means DefaultPolicy.
	Policy timeout.Policy
}

// AcceptResult is what an accepted offer produced.
type AcceptResult struct {
	Swap     *model.Swap
	Timeout  *TimeoutCheck
	Location string
	State    state.State
}

// TimeoutCheck records the independent timeout validation Bob performed.
type TimeoutCheck struct {
	BTCRemainingSeconds   int64
	BlakeRemainingSeconds int64
	ActualRecoveryGap     int64
	RequiredBTCWindow     int64
	RequiredRecovery      int64
}

// AcceptOffer consumes an offer as Bob and answers with his acceptance.
//
// The timeout validation is the security boundary, and it runs before Bob creates
// keys, saves state or sends anything. Alice's metadata is authenticated evidence,
// not authority: Bob measures the actual CLTV heights against his own chain tips
// and his own policy, and it is the *heights* that decide, not the advertisement.
func (e *Engine) AcceptOffer(ctx context.Context, req AcceptRequest) (*AcceptResult, error) {
	if e.Transport == nil {
		return nil, fmt.Errorf("the engine has no coordination transport")
	}

	if e.Identity == nil {
		return nil, fmt.Errorf("no local Nostr identity is configured")
	}

	bobPub := e.Identity.PublicKey()

	if e.Mode == "nip44_v2" && req.ExpectedAlicePub == "" && !req.AllowUnpinnedAuthor {
		return nil, fmt.Errorf(
			"SECURITY FAILURE: accepting an offer over an encrypted transport requires " +
				"the expected Alice identity; pass --expected-alice-pub, or pass " +
				"--allow-unpinned-author to accept whichever author the relay returns",
		)
	}

	message, err := e.Transport.Receive(ctx, req.SwapID, "offer", coord.ReceiveOptions{
		ExpectedPubkey: req.ExpectedAlicePub,
		Mode:           e.Mode,
	})

	if err != nil {
		return nil, err
	}

	if req.ExpectedAlicePub != "" && message.Sender != req.ExpectedAlicePub {
		return nil, fmt.Errorf("SECURITY FAILURE: swap offer was not signed by matched Alice")
	}

	body := message.Body

	if body["type"] != "offer" {
		return nil, fmt.Errorf("message is not an offer")
	}

	if _, present := body["secret_hex"]; present {
		return nil, fmt.Errorf("SECURITY FAILURE: offer contains secret_hex")
	}

	blakeUnits, err := blakeBaseUnits(body["blake_amount"])

	if err != nil {
		return nil, err
	}

	if req.ExpectedBlakeAmount != nil && amount.Blake(blakeUnits) != *req.ExpectedBlakeAmount {
		return nil, fmt.Errorf(
			"SECURITY FAILURE: swap offer BLAKE amount differs from market match",
		)
	}

	sats, err := intField(body, "btc_sats")

	if err != nil {
		return nil, err
	}

	if req.ExpectedBTCSats != nil && sats != *req.ExpectedBTCSats {
		return nil, fmt.Errorf(
			"SECURITY FAILURE: swap offer BTC amount differs from market match",
		)
	}

	policy := req.Policy

	if policy.BTCBlockSeconds == 0 {
		policy = timeout.DefaultPolicy()
	}

	check, err := e.validateOfferedTimeouts(ctx, body, policy)

	if err != nil {
		return nil, err
	}

	blakeHeight, err := e.Blake.Height(ctx)

	if err != nil {
		return nil, err
	}

	btcHeight, err := e.BTC.Height(ctx)

	if err != nil {
		return nil, err
	}

	swap := &model.Swap{
		SwapID:               req.SwapID,
		State:                state.Offered,
		BlakeAmountBaseUnits: blakeUnits,
		BTCSats:              sats,
		SecretHash:           fmt.Sprint(body["secret_hash"]),
		BlakeTimeout:         timeoutOf(body["blake_timeout"]),
		BTCTimeout:           timeoutOf(body["btc_timeout"]),
	}

	if createdAt, err := intField(body, "created_at"); err == nil {
		swap.CreatedAt = createdAt
	}

	if peer := message.Sender; peer != "" {
		swap.NostrAlicePub = &peer
	}

	swap.NostrBobPub = &bobPub

	if version, ok := body["timeout_policy_version"].(string); ok {
		swap.TimeoutPolicyVersion = &version
	}

	if window, ok := intFieldOptional(body, "btc_timeout_window_seconds"); ok {
		swap.BTCTimeoutWindowSeconds = &window
	}

	if window, ok := intFieldOptional(body, "blake_timeout_window_seconds"); ok {
		swap.BlakeTimeoutWindowSeconds = &window
	}

	if recovery, ok := intFieldOptional(body, "bob_recovery_seconds"); ok {
		swap.BobRecoverySeconds = &recovery
	}

	// Bob's own recovery anchors: Alice cannot fund before this record exists, so
	// Alice cannot legitimately name a height Bob should trust.
	swap.BlakeScanStartHeight = &blakeHeight
	swap.BTCScanStartHeight = &btcHeight

	blakeAlice, ok := body["blake_alice_pub"].(string)

	if !ok || blakeAlice == "" {
		return nil, fmt.Errorf("offer has no blake_alice_pub")
	}

	btcAlice, ok := body["btc_alice_pub"].(string)

	if !ok || btcAlice == "" {
		return nil, fmt.Errorf("offer has no btc_alice_pub")
	}

	swap.BlakeAlicePub = &blakeAlice
	swap.BTCAlicePub = &btcAlice

	// Bob creates only Bob-owned keys, from his own wallet.
	blakeKey, err := e.walletPubkey(ctx, chain.Blake)

	if err != nil {
		return nil, err
	}

	btcKey, err := e.walletPubkey(ctx, chain.BTC)

	if err != nil {
		return nil, err
	}

	swap.BlakeBobPub = &blakeKey
	swap.BTCBobPub = &btcKey

	if err := swap.Validate(); err != nil {
		return nil, err
	}

	if err := ssave(e, ctx, swap, store.Bob); err != nil {
		return nil, err
	}

	accept := map[string]any{
		"type":          "accept",
		"swap_id":       swap.SwapID,
		"secret_hash":   swap.SecretHash,
		"blake_bob_pub": blakeKey,
		"btc_bob_pub":   btcKey,
	}

	location, err := e.send(ctx, swap, "accept", accept)

	if err != nil {
		return nil, err
	}

	return &AcceptResult{
		Swap: swap, Timeout: check, Location: location, State: swap.State,
	}, nil
}

// ReceiveAccept consumes Bob's acceptance as Alice.
func (e *Engine) ReceiveAccept(ctx context.Context, swapID string) (*MessageResult, error) {
	swap, err := e.load(ctx, swapID)

	if err != nil {
		return nil, err
	}

	peer, err := e.peerFor(swap)

	if err != nil {
		return nil, err
	}

	message, err := e.Transport.Receive(ctx, swapID, "accept", coord.ReceiveOptions{
		ExpectedPubkey: peer,
		Mode:           e.Mode,
	})

	if err != nil {
		return nil, err
	}

	if message.Body["type"] != "accept" {
		return nil, fmt.Errorf("message is not an accept")
	}

	if _, present := message.Body["secret_hex"]; present {
		return nil, fmt.Errorf("SECURITY FAILURE: accept message contains secret_hex")
	}

	if message.Body["swap_id"] != swap.SwapID {
		return nil, fmt.Errorf("swap_id mismatch")
	}

	if message.Body["secret_hash"] != swap.SecretHash {
		return nil, fmt.Errorf("secret_hash mismatch")
	}

	blakeKey, err := stringField(message.Body, "blake_bob_pub")

	if err != nil {
		return nil, err
	}

	btcKey, err := stringField(message.Body, "btc_bob_pub")

	if err != nil {
		return nil, err
	}

	swap.BlakeBobPub = &blakeKey
	swap.BTCBobPub = &btcKey

	if message.Sender != "" {
		if swap.NostrBobPub != nil && *swap.NostrBobPub != message.Sender {
			return nil, fmt.Errorf(
				"SECURITY FAILURE: acceptance sender identity differs from pinned Bob identity",
			)
		}

		bob := message.Sender
		swap.NostrBobPub = &bob
	}

	if err := e.save(ctx, swap); err != nil {
		return nil, err
	}

	return &MessageResult{
		Kind: "accept", Location: message.Location, Sender: message.Sender, State: swap.State,
	}, nil
}

// validateOfferedTimeouts is Bob's independent timeout check.
//
// Every number that matters is derived from the *heights* Bob was offered, against
// Bob's own chain tips and policy. The signed metadata is checked for coherence
// too, but it is never the authority — a maker who advertises a generous window and
// signs heights that leave none must be refused.
func (e *Engine) validateOfferedTimeouts(
	ctx context.Context, body map[string]any, policy timeout.Policy,
) (*TimeoutCheck, error) {
	version, ok := body["timeout_policy_version"].(string)

	if !ok || version == "" {
		return nil, fmt.Errorf("SECURITY FAILURE: timeout policy version is required")
	}

	if version != "1.5" {
		return nil, fmt.Errorf(
			"SECURITY FAILURE: unsupported timeout policy version %q", version,
		)
	}

	required := []string{
		"btc_timeout", "blake_timeout",
		"btc_timeout_window_seconds", "blake_timeout_window_seconds",
		"bob_recovery_seconds",
	}

	for _, field := range required {
		value, ok := body[field]

		if !ok {
			return nil, fmt.Errorf("SECURITY FAILURE: v1.5 offer missing %s", field)
		}

		number, ok := toInt64(value)

		if !ok {
			return nil, fmt.Errorf(
				"SECURITY FAILURE: v1.5 offer field %s must be an integer", field,
			)
		}

		if number <= 0 {
			return nil, fmt.Errorf(
				"SECURITY FAILURE: v1.5 offer field %s must be positive", field,
			)
		}
	}

	btcHeight, err := e.BTC.Height(ctx)

	if err != nil {
		return nil, err
	}

	blakeHeight, err := e.Blake.Height(ctx)

	if err != nil {
		return nil, err
	}

	btcTimeout := timeoutOf(body["btc_timeout"])
	blakeTimeout := timeoutOf(body["blake_timeout"])

	if btcTimeout <= btcHeight {
		return nil, fmt.Errorf("SECURITY FAILURE: Bitcoin timeout is already expired")
	}

	if blakeTimeout <= blakeHeight {
		return nil, fmt.Errorf("SECURITY FAILURE: BLAKE timeout is already expired")
	}

	localMinimum, err := timeout.BuildPlan(btcHeight, blakeHeight, policy)

	if err != nil {
		return nil, err
	}

	btcRemaining := (btcTimeout - btcHeight) * int64(policy.BTCBlockSeconds)
	blakeRemaining := (blakeTimeout - blakeHeight) * int64(policy.BlakeBlockSeconds)

	actualGap := blakeRemaining - btcRemaining

	if btcRemaining < localMinimum.BTCWindowSeconds {
		return nil, fmt.Errorf(
			"SECURITY FAILURE: Bitcoin timeout does not leave Bob's locally required " +
				"BTC safety window",
		)
	}

	if actualGap < localMinimum.BobRecoverySeconds {
		return nil, fmt.Errorf(
			"SECURITY FAILURE: BLAKE timeout does not leave Bob's locally required " +
				"cross-chain recovery window",
		)
	}

	offeredBTC, _ := intFieldOptional(body, "btc_timeout_window_seconds")
	offeredBlake, _ := intFieldOptional(body, "blake_timeout_window_seconds")
	offeredRecovery, _ := intFieldOptional(body, "bob_recovery_seconds")

	if offeredBTC < localMinimum.BTCWindowSeconds {
		return nil, fmt.Errorf(
			"SECURITY FAILURE: advertised BTC safety window is below Bob's policy",
		)
	}

	if offeredRecovery < localMinimum.BobRecoverySeconds {
		return nil, fmt.Errorf(
			"SECURITY FAILURE: advertised Bob recovery budget is below Bob's policy",
		)
	}

	if offeredBlake-offeredBTC < offeredRecovery {
		return nil, fmt.Errorf(
			"SECURITY FAILURE: advertised timeout metadata does not preserve its own " +
				"recovery budget",
		)
	}

	return &TimeoutCheck{
		BTCRemainingSeconds:   btcRemaining,
		BlakeRemainingSeconds: blakeRemaining,
		ActualRecoveryGap:     actualGap,
		RequiredBTCWindow:     localMinimum.BTCWindowSeconds,
		RequiredRecovery:      localMinimum.BobRecoverySeconds,
	}, nil
}

// maxInt64 returns the larger of two integers.
func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}

	return b
}

// toInt64 converts a decoded JSON number.
func toInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	case float64:
		if typed != float64(int64(typed)) {
			return 0, false
		}

		return int64(typed), true
	}

	return 0, false
}

// intFieldOptional reads an optional integer field.
func intFieldOptional(body map[string]any, name string) (int64, bool) {
	value, ok := body[name]

	if !ok {
		return 0, false
	}

	return toInt64(value)
}
