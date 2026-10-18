# Porting the Python engine to Go (TinyGo): control flow, types, and a package map

This is an analysis-and-plan document, not an implementation. It answers three
questions:

1. what the Python engine actually is, structurally;
2. which Go packages should replace it, and what lives in each;
3. what has to change because the target is TinyGo wasm in a browser, not a
   Python process with a filesystem.

Numbers below were extracted from the source, not estimated: 23,767 lines in
`src/swap_v2.py`, 58 operator commands, 42 durable swap fields, 9 states, 12
reconcile actions, 30 RPC methods, 7 legacy wire messages plus 4 market message
types.

---

## 1. The Python engine at a glance

| Module | Lines | Role |
|---|---|---|
| `src/swap_v2.py` | 23,767 | everything: crypto glue, chain RPC, storage, coordination, market, settlement, CLI |
| `src/swap_wire.py` | 724 | dependency-free wire schema and validation for the 7 swap messages |
| `src/nip44.py` | 457 | NIP-44 v2 encryption |
| `src/swap_timeout_policy.py` | 440 | CLTV/economic timeout math |
| `src/swap_agent.py` | 486 | headless polling daemon (one step per tick) |
| `src/fee_policy.py` | 264 | feerate selection and the fee cap |

The shape is a **CLI over a durable state machine**. Every command is the same
transaction:

```
load swap  ->  validate state/role  ->  observe chain truth  ->  act
           ->  persist (atomic)    ->  coordinate (transport)  ->  print
```

Above that sits the driver (`run`/`resume`), which is a loop:

```
reconcile (read-only) -> choose ONE action -> execute it -> reconcile again
```

`reconcile` is the brain: it observes both HTLCs, derives one conservative
recommendation, and never executes it. The driver executes only allowlisted
actions and only for the role that may perform them.

### 1.1 Durable state

One JSON record per swap, per local role (`state/alice/<swap_id>.json`,
`state/bob/<swap_id>.json`), written atomically (temp file + fsync + rename).
A `flock` serialises funding. Separate keyspaces exist for the message spool,
a verified-event cache, market bindings, market selections and ignore
tombstones.

The `Swap` record has **42 fields**, and they fall into clear groups:

| Group | Fields |
|---|---|
| identity | `swap_id`, `state`, `created_at`, `timeout_policy_version` |
| economics | `btc_sats`, `blake_amount_base_units`, `secret_hash` |
| secret | `secret_hex` (Alice), `revealed_secret_hex` (Bob, recovered from Bitcoin) |
| keys | `blake_alice_pub`, `blake_bob_pub`, `btc_alice_pub`, `btc_bob_pub`, `nostr_alice_pub`, `nostr_bob_pub` |
| timeouts | `blake_timeout`, `btc_timeout`, `btc_timeout_window_seconds`, `blake_timeout_window_seconds`, `bob_recovery_seconds` |
| scan anchors | `blake_scan_start_height`, `btc_scan_start_height` |
| funding | `{blake,btc}_funding_{txid,vout,blockhash}` |
| settlement | `{btc,blake}_{claim,refund}_{txid,blockhash,hex}` |
| reorg salvage | `blake_reorg_claim_txid`, `blake_reorg_refund_txid` |

### 1.2 State machine

```
OFFERED -> BLAKE_FUNDED -> BTC_FUNDED -> SECRET_REVEALED -> BTC_CLAIMED -> BLAKE_CLAIMED -> COMPLETE
                     \-> BLAKE_REFUNDED        \-> BTC_REFUNDED -> BLAKE_REFUNDED
```

The forward table is data (`ALLOWED_TRANSITIONS`). Reorganisation recovery is a
second, explicitly enumerated table of **backward** edges (`REORG_TRANSITIONS`)
so a recovery path cannot invent a transition:

```
BLAKE_CLAIMED  -> BTC_CLAIMED
BLAKE_REFUNDED -> BTC_REFUNDED
```

### 1.3 The decision table

`_reconcile_next_action` maps (state, role, BLAKE observation, BTC observation,
heights) to exactly one of **12 actions**, which become **5 decision classes**:

| Actions | Class |
|---|---|
| `none` | settled |
| `wait` | waiting |
| `sync-funding`, `sync-settlement` | recovery-needed |
| `manual-attention` | manual-attention |
| `fund-blake`, `fund-btc`, `claim-btc`, `claim-blake`, `refund-btc`, `refund-blake`, `complete` | actionable |

Observation vocabulary (per chain): `absent`, `pending`, `funded`, `spent`
(with `branch` = `claim` | `refund`), `pending-spend`, `unknown`, `invalid`,
`pending-or-ambiguous`. Confirmed chain truth outranks a stale checkpoint: the
table prefers `sync-*` over any action that would require a newer local state.

The driver's allowlist is narrower than the actionable set: it may execute
`sync-funding`, `sync-settlement`, `complete`, `claim-btc`, `claim-blake`,
`refund-btc`, `refund-blake`, with `fund-btc` gated by policy and `fund-blake`
treated as economic (manual).

### 1.4 Wire and market vocabulary

Swap coordination (validated by `swap_wire.SCHEMAS`), 7 types:
`offer`, `accept`, `blake-funded`, `btc-funded`, `btc-claimed`,
`blake-claimed`, `btc-refunded`.

Market (versioned 1 and 2), 4 types: `market-offer`, `market-take`,
`market-cancel`, `market-match`, carried as signed kind-1 Nostr events with
`t=blake-swap-market`, `base`, `quote`, `direction` tags.

### 1.5 Chain, crypto, coordination

- **30 RPC methods** across `descriptor/derive`, `wallet`, `rawtx/psbt`,
  `mempool`, `block` and `broadcast` families. Descriptors are compiled by the
  node, so the node is the authority on the HTLC script.
- **Settlement** is a PSBT pipeline: `createrawtransaction` → `converttopsbt` →
  `utxoupdatepsbt` (descriptor) → `walletprocesspsbt` → inject the SHA-256
  preimage (PSBT key `0x0b`) → `finalizepsbt` → `decoderawtransaction` (vsize)
  → iterate on fee → `sendrawtransaction` → persist the exact signed hex.
- **Observation** is wallet-first (watch-only descriptor import), with a
  bounded verbosity-2 block scan as the fallback.
- **Coordination** is either a file spool or Nostr: kind-1 events, BIP-340
  signatures, mandatory author pinning, optional NIP-44 v2, relay WebSocket
  `EVENT`/`REQ`/`EOSE`, and a verified-event cache.
- **Crypto**: secp256k1 (coincurve), BIP-340 Schnorr, SHA-256, NIP-44
  (ChaCha20 + HMAC-SHA256 + HKDF over an x-only ECDH).

---

## 2. What changes because the target is TinyGo in a browser

| Python | Go/TinyGo | Why |
|---|---|---|
| `subprocess` + `bitcoin-cli` | JSON-RPC over HTTP through the app's own server (`/rpc/btc`, `/rpc/blake`) | a browser cannot open a TCP socket to a node; the proxy keeps credentials out of the page |
| files + `os.replace` + `fsync` + `flock` | `store` over IndexedDB (async) + a per-swap actor in the worker | no filesystem; the worker is the single writer, which replaces `flock` |
| `os.environ` config | one `Config` value delivered to the worker at boot | wasm has no environment |
| `print()` | structured log events to the page console | the page is the only UI |
| `asyncio` + `websockets` | JS `WebSocket` driven through the `link` bridge | no sockets in wasm |
| `coincurve` (libsecp256k1) | pure-Go secp256k1 + BIP-340 | no cgo in wasm |
| `argparse`, 58 commands | a typed Go API, plus the adapter shim that speaks `blake-swap-adapter-v1` | the CLI is a transport, not the protocol |
| one process, two roles | one participant per browser; the store still keyspaces by role so tests can run both | privacy and simplicity |

Two facts worth stating plainly:

- **The state machine does not need a browser.** The value layer
  (`amount`, `wire`, `market`, `state`, `model`, `timeout`, `fees`,
  `reconcile`, `htlc`, `psbt`) is pure Go and should be developed and tested
  with `go test` on the host. Wasm packaging is a thin layer on top.
- **The parity harness already exists.** `blake-swap-adapter-v1` (JSON over
  stdin/stdout: `capabilities`, `command`, `state`) is what the Python
  adversarial and conformance suites drive. Implementing that adapter over the
  Go library — as a *native* binary first — lets the existing 60-suite corpus
  run against Go, including cross-language combinations (Python Alice / Go
  Bob), without writing a new test corpus.

---

## 3. The Go package map

Five layers. Arrows are import direction; nothing lower imports anything
higher.

```
                     cmd/webpage    cmd/webworker    cmd/server
                            \           |            /
  app layer           page(P)   app(worker domains)   jsonrpc / rpcproxy
                                   |
  orchestration      swap  agent  marketflow
                       |      |        |
  io layer           chain  fund  settle  coord  nostr  store
                       \     |      |      |      |      /
  crypto layer                keys  nip44  event
                                   |
  value layer    amount  wire  market  state  model  timeout  fees  reconcile  htlc  psbt
```

| Package | Port of | Contents |
|---|---|---|
| `pkg/amount` | `blake_base_units*`, `format_blake_amount` | BLAKE base units ↔ decimal string, BTC sats ↔ coin, exact parsing, no float |
| `pkg/wire` | `swap_wire.py` | the 7 swap messages as structs, the shared envelope (`swap_id`, `type`, `secret_hash`), `Validate`, `Encode`/`Decode`, and the same refusal rules |
| `pkg/market` | market builders/validators in `swap_v2.py` | `Offer`/`Take`/`Match`/`Cancel` (versions 1 and 2), direction → protocol roles, offer/take/match binding rules |
| `pkg/state` | `State`, `ALLOWED_TRANSITIONS`, `REORG_TRANSITIONS` | the state enum, forward and reorg edges, and the two transition functions |
| `pkg/model` | `Swap`, `make_swap`, `migrate_swap_record` | the 42-field record, defaults, invariants, legacy migration (decimal → base units) |
| `pkg/timeout` | `swap_timeout_policy.py` | `Policy`, `Plan`, `Build`, `Validate`, `Describe`; the ten policy inputs and the time-not-blocks validation |
| `pkg/fees` | `fee_policy.py` | rate choice from estimator + floors, `FeeForVsize`, cap enforcement, minimum viable input, the two cap knobs |
| `pkg/reconcile` | `_reconcile_observe_*`, `_reconcile_next_action`, `_reconcile_execution_readiness` | `Observation`, `Heights`, `Decision`, `Readiness`; the 12-action table as data plus its pure evaluator |
| `pkg/htlc` | `htlc_descriptor_body`, `classify_htlc_spend`, address derivation | the `wsh(or_i(and_v(...)))` body and the exact witness-shape classifier for claim/refund |
| `pkg/psbt` | `add_sha256_preimage`, `read_compact_size`, `find_map_end` | minimal PSBT parse/serialize and the `0x0b` hashlock injection; bounds-checked |
| `pkg/keys` | `nostr_private_key_hex`, pubkey derivation, signing, `nostr_event_id` | secp256k1 keys, compressed pubkeys, BIP-340 sign/verify, x-only ECDH |
| `pkg/nip44` | `nip44.py` | pad/unpad, HKDF, ChaCha20, HMAC, encrypt/decrypt, payload detection |
| `pkg/event` | `build_nostr_event`, verification, tags | kind-1 event construction, id computation, signature verification, author pinning |
| `pkg/rpc` | `run_chain_cli` | JSON-RPC call with the timeout, retry allowlist, transient-errno policy and the "state-changing calls are never retried" rule |
| `pkg/chain` | `btc_cli`/`blake_cli`, wallets, `gettxout`/`getrawtransaction` | per-chain client, wallet selection, height/utxo/tx reads, confirmation counting |
| `pkg/fund` | funding intent, `sendtoaddress`, descriptor import, watch-only wallets | funding one leg, the intent record that covers the crash window, watch-only descriptor import and solvability |
| `pkg/settle` | `build_dynamic_settlement_spend` and the claim/refund builders | the PSBT pipeline, the fee/vsize convergence loop, caps, broadcast, signed-hex persistence, rebroadcast |
| `pkg/coord` | `file_*`/`nostr_*` send/receive, `receive_message` | one `Transport` interface, file/store and Nostr implementations, role pinning and binding checks |
| `pkg/nostr` | relay client, event cache | WebSocket sessions over the `link` bridge, publish with relay report, `REQ`/`EOSE` fetch, verified-event cache |
| `pkg/store` | `save_swap`, `load_swap`, message spool, bindings | durable records keyed by (role, swap_id), atomic commit, spool, market bindings/selections, tombstones |
| `pkg/swap` | the 58 commands | the typed command surface: `New`, `Publish*`, `Receive*`, `Fund*`, `Claim*`, `Refund*`, `Sync*`, `Reconcile`, `Next`, `Dashboard`, `Recover` |
| `pkg/agent` | `swap_agent.py` | the polling loop and the driver's allowlist/role/step-budget policy |
| `pkg/marketflow` | market lifecycle | publish offer, take, match, accept, import a match into a swap, cancel, ignore |
| `pkg/jsonrpc` | (new) | the page ↔ worker request/response contract, so the page never links the engine |
| `cmd/server` | (extends today's) | static app **plus** `/rpc/{btc,blake}` node proxy |
| `pkg/app`, `pkg/page` | (new / extends) | worker domain wiring; page presentation |

### 3.1 API sketches

```go
// pkg/state
type State string
const (
    Offered State = "OFFERED"; BlakeFunded = "BLAKE_FUNDED"; /* ... */
)
func (s State) CanTransitionTo(next State) bool
func ApplyReorg(sw *model.Swap, target State) error

// pkg/reconcile
type Observation struct {
    Status  Status // Absent, Pending, Funded, Spent, PendingSpend, Unknown, Invalid, Ambiguous
    Branch  Branch // None, Claim, Refund
    TxID    string; Vout uint32; BlockHash string; Confirmations int32
}
type Decision struct{ Action Action; Class Class; Reason string }
func Decide(sw *model.Swap, role Role, blake, btc Observation, h Heights) Decision
func Readiness(sw *model.Swap, role Role, a Action, h Heights) Readiness

// pkg/wire
type Message interface{ Kind() Kind }
type Offer struct { SwapID string; SecretHash string; CreatedAt int64; Sats int64; /* ... */ }
func Decode(b []byte) (Message, error) // rejects unknown types and extra fields
func Encode(m Message) ([]byte, error)

// pkg/settle
type Plan struct{ Hex string; Fee int64; Vsize int32; Rate Decimal; Source RateSource }
func Build(ctx context.Context, n *chain.Node, sp SpendParams, build func(fee int64) (string, error)) (Plan, error)
func Broadcast(ctx context.Context, n *chain.Node, hex string) (string, error)

// pkg/swap
type Engine struct{ BTC, BLAKE *chain.Node; Store store.Store; Coord coord.Coordinator; Fees fees.Policy; Timeouts timeout.Policy }
func (e *Engine) New(role Role, p NewParams) (*model.Swap, error)
func (e *Engine) ClaimBTC(role Role, id SwapID, feeRate *Decimal) (Result, error)
func (e *Engine) Reconcile(role Role, id SwapID) (reconcile.Report, error)
func (e *Engine) Next(role Role, id SwapID) (reconcile.Action, error) // exactly one step
```

### 3.2 Deliberately not ported

- `argparse` and the CLI plumbing (the adapter and `jsonrpc` replace it).
- File transport and the message directory (a `StoreTransport` replaces it;
  keeping it would only serve local Python cross-checks).
- `flock`, `fsync`, `os.replace` (single-writer worker + IndexedDB transaction).
- Python exception strings (the adapter contract explicitly allows different
  refusal text; refusal *semantics* must match).
- The verbosity-2 block scan, if watch-only observation + the recorded anchor
  cover every case the corpus exercises — needs a decision, not an assumption.

---

## 4. Milestones, in the order that buys the most certainty

| # | Milestone | Done when |
|---|---|---|
| M0 | **Spikes**: TinyGo + secp256k1/BIP-340 vectors; PSBT preimage injection; `store` on IndexedDB; JS `WebSocket` bridge; node proxy | each spike is a failing-first test |
| M1 | Value layer (`amount`, `wire`, `market`, `state`, `model`, `timeout`, `fees`) | ported vectors from the Python suites pass under `go test` on the host |
| M2 | `protocol`/`reconcile` + `htlc`/`psbt` | the 12-action table and witness classifier match Python on the recorded fixtures |
| M3 | `rpc`/`chain`/`fund`/`settle` against a real regtest node through the proxy | read-only parity: heights, utxos, confirmations, descriptor checksums, vsize |
| M4 | **`blake-swap-adapter-v1` as a native Go binary** | the Python seeded/adversarial corpus runs Go against Python in all four role combinations |
| M5 | `store`/`coord`/`nostr` + `swap` commands | a full swap between two browser instances on regtest |
| M6 | `marketflow` + page UI | offer → take → match → accept → swap → settle, driven from the UI |

M4 is the pivot: it converts "we think the port is faithful" into a mechanical
claim, using tests that already exist.

---

## 5. Decisions I need from you

1. **Node access model.** Proxy the node through `cmd/server` (recommended:
   browser stays credential-free, and the proxy is the one place to rate-limit),
   or expose an authenticated node endpoint to the worker directly?
2. **Secrets at rest.** The swap secret and the Nostr key must survive a
   reload to claim/refund. IndexedDB in plaintext, or a passphrase-encrypted
   vault (WebCrypto) with the key never persisted? I recommend the vault.
3. **Command surface.** A clean typed Go API with the adapter as a thin shim
   (recommended, keeps the corpus), or a 1:1 command-string API so the adapter
   is trivial and the library is shaped by the CLI?
4. **Watch-only plus scan.** Keep the bounded scan as a fallback, or rely on
   watch-only observation only for v1?
5. **Market scope.** Is the market flow (offer/take/match) in v1 of the Go
   port, or is v1 the two-party swap only?

Once these are settled, M0–M2 can start; nothing in them depends on the
browser.
