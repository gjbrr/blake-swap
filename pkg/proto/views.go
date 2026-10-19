package proto

//
// The view models are the structured half of a command result.
//
// The page must never parse the CLI's text: a renderer that reads "txid:  <64 hex>"
// out of a string is a second protocol client, and the first time the wording changes
// the UI silently shows nothing. So each command that has a view also returns one, and
// the page renders the view.
//
// Two rules run through every field here:
//
//   - money is a string, produced by pkg/amount or pkg/decimal. A JSON number would be
//     read as a float by the browser and 0.1 would stop being 0.1; and
//   - a view carries no secret. The secret hash is public by design and may appear; a
//     preimage, a key, or a passphrase may not, and there is no field for one.
//

// SetupView is the Settings screen: what this client is configured with, and what it
// can see. It holds no secret: the identity is the public key, and the proxy is an
// address the operator typed.
type SetupView struct {
	// VaultPresent reports whether an encrypted store exists on this machine.
	VaultPresent bool `json:"vault_present"`
	// Unlocked reports whether the derived key is in the worker's memory.
	Unlocked bool `json:"unlocked"`
	// Identity is the Nostr public key, empty when none is configured.
	Identity string `json:"identity,omitempty"`
	// Relays is where the market and coordination read from.
	Relays []string `json:"relays,omitempty"`
	// Proxy is the app server base URL the worker reaches both chains through.
	Proxy string `json:"proxy,omitempty"`
	// Wallets are the four wallet names, and whether the node can see each one.
	Wallets WalletView `json:"wallets"`
	// Heights are the two chain tips, zero when unreachable.
	Heights HeightView `json:"heights"`
	// ProxyHealthy reports whether both chains answered through the proxy, and
	// ProxyDetail is the refusal when one did not.
	ProxyHealthy bool   `json:"proxy_healthy"`
	ProxyDetail  string `json:"proxy_detail,omitempty"`
}

// WalletView is the four wallet names this participant's node uses.
//
// A participant has one signing wallet and one watch-only wallet per chain. It is not
// Alice's BLAKE wallet and Bob's Bitcoin wallet: that mapping is a test rig's, and a
// real participant on one node needs their own name for each chain.
type WalletView struct {
	Blake      string `json:"blake"`
	BTC        string `json:"btc"`
	BlakeWatch string `json:"blake_watch"`
	BTCWatch   string `json:"btc_watch"`
	// Visible reports whether the node can see a wallet with that name. It is a map
	// rather than four booleans because a missing wallet is the normal state before
	// the node has one, and the UI shows it per name.
	Visible map[string]bool `json:"visible,omitempty"`
}

// HeightView is the two chain tips.
type HeightView struct {
	Blake int64 `json:"blake"`
	BTC   int64 `json:"btc"`
}

// SwapSummary is one swap, as the detail view and the dashboard rows show it.
type SwapSummary struct {
	SwapID string `json:"swap_id"`
	// State is the durable checkpoint.
	State string `json:"state"`
	// Role is the protocol role this participant holds, derived from the record's
	// identity pins. It is never chosen by the person using the client.
	Role string `json:"role"`
	// Counterparty is the other participant's Nostr public key, when known.
	Counterparty string `json:"counterparty,omitempty"`
	// BlakeAmount and BTCSats are the agreed trade. BTCSats is an exact integer
	// string, so a browser can render it without a float and without formatting it.
	BlakeAmount string `json:"blake_amount"`
	BTCSats     string `json:"btc_sats"`
	// SecretHash is the hashlock. It is public: both chains hold it.
	SecretHash string `json:"secret_hash"`
	// CreatedAt is the record's creation time in Unix seconds.
	CreatedAt int64 `json:"created_at"`
	// BTCTimeout and BlakeTimeout are the two contract timelocks, in blocks.
	BTCTimeout   int64 `json:"btc_timeout"`
	BlakeTimeout int64 `json:"blake_timeout"`
	// Funding and Settlement are the transaction anchors.
	Funding    FundingView    `json:"funding"`
	Settlement SettlementView `json:"settlement"`
	// NextAction, DecisionClass and Reason come from the reconciliation report, which
	// is the only thing that decides what may be done next.
	NextAction    string `json:"next_action"`
	DecisionClass string `json:"decision_class"`
	Reason        string `json:"reason"`
	// Recovered reports that this participant adopted a settlement the record already
	// named rather than signing a new one.
	Recovered bool `json:"recovered"`
	// Chains are the per-chain observations the report made.
	Chains ChainView `json:"chains"`
	// Timeline is the protocol's own step list, with the reached steps marked.
	Timeline []TimelineStep `json:"timeline,omitempty"`
	// Unsolvable lists the HTLCs on this swap that no loaded wallet can spend.
	Unsolvable []string `json:"unsolvable,omitempty"`
	// Negotiation is the handshake state, which is not the same as the checkpoint.
	Negotiation string `json:"negotiation,omitempty"`
}

// FundingView is the two funding outpoints.
type FundingView struct {
	BlakeTxID string `json:"blake_txid,omitempty"`
	BlakeVout int32  `json:"blake_vout,omitempty"`
	BTCTxID   string `json:"btc_txid,omitempty"`
	BTCVout   int32  `json:"btc_vout,omitempty"`
}

// SettlementView is the four settlement transactions and whether each is confirmed.
type SettlementView struct {
	BTCClaim        TxView `json:"btc_claim"`
	BTCRefund       TxView `json:"btc_refund"`
	BlakeClaim      TxView `json:"blake_claim"`
	BlakeRefund     TxView `json:"blake_refund"`
	ReorgClaimTxID  string `json:"blake_reorg_claim_txid,omitempty"`
	ReorgRefundTxID string `json:"blake_reorg_refund_txid,omitempty"`
}

// TxView is one recorded transaction.
type TxView struct {
	TxID string `json:"txid,omitempty"`
	// BlockHash is empty while the transaction is in the mempool.
	BlockHash string `json:"blockhash,omitempty"`
	// Confirmed is BlockHash != "", spelled out so the page does not have to infer it.
	Confirmed bool `json:"confirmed"`
}

// ChainView is what the reconciliation report observed on each chain.
type ChainView struct {
	Blake ObservationView `json:"blake"`
	BTC   ObservationView `json:"btc"`
}

// ObservationView is one chain's observation.
type ObservationView struct {
	// Status is absent, funded, spent or unknown.
	Status string `json:"status"`
	// Branch is claim, refund or empty.
	Branch string `json:"branch,omitempty"`
	// TxID is the transaction the observation names, when it names one.
	TxID string `json:"txid,omitempty"`
	// Height is the chain height the observation was made at.
	Height int64 `json:"height,omitempty"`
}

// TimelineStep is one checkpoint in the protocol's step list.
type TimelineStep struct {
	State string `json:"state"`
	// Reached means the current state is at or after this one on the branch that was
	// actually taken.
	Reached bool `json:"reached"`
	// Current marks the checkpoint the record is at.
	Current bool `json:"current"`
	// Refund marks the abort branch, so the UI can show it as a branch rather than as
	// a step everybody passes through.
	Refund bool `json:"refund"`
}

// DashboardView is the Swaps screen: every record, classified.
type DashboardView struct {
	Active    []DashboardRow `json:"active,omitempty"`
	Attention []DashboardRow `json:"attention,omitempty"`
	Settled   []DashboardRow `json:"settled,omitempty"`
	Archived  []DashboardRow `json:"archived,omitempty"`
	Heights   HeightView     `json:"heights"`
	// Unsolvable lists every HTLC on this machine that no loaded wallet can spend.
	// It is the dashboard's most important line and has no per-swap equivalent.
	Unsolvable []UnsolvableView `json:"unsolvable,omitempty"`
	// Failed lists records whose report could not be built.
	Failed []DashboardRow `json:"failed,omitempty"`
}

// DashboardRow is one swap in a dashboard group.
type DashboardRow struct {
	SwapID        string `json:"swap_id"`
	State         string `json:"state"`
	Role          string `json:"role"`
	Outcome       string `json:"outcome"`
	NextAction    string `json:"next_action"`
	DecisionClass string `json:"decision_class"`
	Reason        string `json:"reason"`
	BlakeAmount   string `json:"blake_amount"`
	BTCSats       string `json:"btc_sats"`
	BlakeStatus   string `json:"blake_status"`
	BTCStatus     string `json:"btc_status"`
	// TxIDs are the settlement anchors this record holds.
	TxIDs map[string]string `json:"txids,omitempty"`
}

// UnsolvableView is one HTLC no loaded wallet can spend.
type UnsolvableView struct {
	SwapID string `json:"swap_id"`
	Chain  string `json:"chain"`
	Reason string `json:"reason"`
}

// MarketView is the Market screen: what is advertised, what this participant said and
// what applies to them.
type MarketView struct {
	// Read is the scan line, such as "12 unique event(s) from 1/1 relay(s)". It is
	// kept because a market with no offers and a market whose relays did not answer
	// look identical without it.
	Read string `json:"read,omitempty"`
	// Relays is the configured relay list.
	Relays []string `json:"relays,omitempty"`
	// Open is every open offer from anybody.
	Open []MarketOffer `json:"open,omitempty"`
	// Mine is this participant's own offers, whatever their state.
	Mine []MarketOffer `json:"mine,omitempty"`
	// Takes are valid takes on this participant's offers.
	Takes []MarketTake `json:"takes,omitempty"`
	// Matches are verified matches naming this participant.
	Matches []MarketMatch `json:"matches,omitempty"`
	// Selections are the durable market-to-swap bindings this client holds.
	Selections []MarketSelection `json:"selections,omitempty"`
	// Err is a relay or validation refusal, so a failed refresh is shown rather than
	// rendered as an empty market.
	Err string `json:"error,omitempty"`
}

// MarketOffer is one advertisement.
type MarketOffer struct {
	EventID     string `json:"event_id"`
	Maker       string `json:"maker"`
	Direction   string `json:"direction"`
	BlakeAmount string `json:"blake_amount"`
	BTCSats     string `json:"btc_sats"`
	CreatedAt   int64  `json:"created_at"`
	ExpiresAt   int64  `json:"expires_at"`
	// State is open, matched, cancelled or expired, from the evidence.
	State string `json:"state"`
	// Mine reports whether this participant signed it.
	Mine bool `json:"mine"`
}

// MarketTake is one take of one offer.
type MarketTake struct {
	EventID     string `json:"event_id"`
	OfferID     string `json:"offer_id"`
	Taker       string `json:"taker"`
	BlakeAmount string `json:"blake_amount"`
	BTCSats     string `json:"btc_sats"`
	CreatedAt   int64  `json:"created_at"`
	// Accepted reports whether a swap already exists for this take.
	Accepted bool `json:"accepted"`
}

// MarketMatch is one verified match.
type MarketMatch struct {
	EventID     string `json:"event_id"`
	OfferID     string `json:"offer_id"`
	TakeID      string `json:"take_id"`
	SwapID      string `json:"swap_id"`
	Maker       string `json:"maker"`
	Taker       string `json:"taker"`
	BlakeAmount string `json:"blake_amount"`
	BTCSats     string `json:"btc_sats"`
	CreatedAt   int64  `json:"created_at"`
	// Imported reports whether this participant holds the swap it names.
	Imported bool `json:"imported"`
}

// MarketSelection is one durable market-to-swap binding.
type MarketSelection struct {
	OfferID string `json:"offer_id"`
	TakeID  string `json:"take_id,omitempty"`
	MatchID string `json:"match_id,omitempty"`
	SwapID  string `json:"swap_id"`
	Role    string `json:"role"`
	// Direction is the market direction the binding was made under.
	Direction string `json:"direction,omitempty"`
}

// ActionView is what a command that changed something did.
type ActionView struct {
	// Command is the command that ran.
	Command string `json:"command"`
	// SwapID is the record it acted on, when it acted on one.
	SwapID string `json:"swap_id,omitempty"`
	// Kind names the action in the UI's vocabulary, such as "fund-blake".
	Kind string `json:"kind,omitempty"`
	// TxID, BlockHash and Confirmed describe the transaction, when one was produced.
	TxID      string `json:"txid,omitempty"`
	BlockHash string `json:"blockhash,omitempty"`
	Confirmed bool   `json:"confirmed"`
	Pending   bool   `json:"pending"`
	// Recovered reports that the transaction was adopted from the record rather than
	// signed now.
	Recovered bool `json:"recovered"`
	// FeeSats, Vsize and FeeRate are the measured fee, when one was measured. FeeRate
	// is a string because it is money.
	FeeSats   string `json:"fee_sats,omitempty"`
	Vsize     int64  `json:"vsize,omitempty"`
	FeeRate   string `json:"fee_rate,omitempty"`
	FeeSource string `json:"fee_source,omitempty"`
	// State is the checkpoint after the action.
	State string `json:"state,omitempty"`
	// Message is where a published message went, or the event id.
	Message string `json:"message,omitempty"`
	// Sender is the authenticated author of a received message.
	Sender string `json:"sender,omitempty"`
	// Start and End are the states a sync or resume moved between.
	Start string `json:"start,omitempty"`
	End   string `json:"end,omitempty"`
	// Performed reports whether a resume actually did the recommended action.
	Performed bool `json:"performed,omitempty"`
	// DecisionClass and Reason are the reconciliation's own wording, for an action
	// that reports a recommendation rather than a transaction.
	DecisionClass string `json:"decision_class,omitempty"`
	Reason        string `json:"reason,omitempty"`
	// Details are the action's own extra lines, already rendered as exact strings.
	Details []ActionFact `json:"details,omitempty"`
}

// ActionFact is one labelled fact about an action, for the UI to render as a row.
type ActionFact struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// ErrorView is a refusal.
//
// It carries the engine's wording unchanged. The page shows it verbatim: a generic
// "something went wrong" throws away the one sentence that tells an operator what to do.
type ErrorView struct {
	Command string `json:"command"`
	Message string `json:"message"`
}
