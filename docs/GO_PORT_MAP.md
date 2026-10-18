# Porting the Python engine to Go (TinyGo): control flow, types, and a package map

This is the analysis-and-plan document for the port. It records what the Python
engine is, structurally; what changes because the target is a browser client
with a Go server behind it; which Go packages replace it; and what the first
spike round has already proved.

Numbers were extracted from the source, not estimated: 23,767 lines in
`src/swap_v2.py`, 58 operator commands, 42 durable swap fields, 9 states, 12
reconcile actions, 30 RPC methods, 7 swap wire messages plus 4 market message
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

One JSON record per swap, per local role, written atomically. Separate
keyspaces exist for the message spool, a verified-event cache, market bindings,
market selections and ignore tombstones.

The `Swap` record has **42 fields**, in clear groups:

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

Swap coordination (validated by `swap_wire.SCHEMAS`), 7 types: `offer`,
`accept`, `blake-funded`, `btc-funded`, `btc-claimed`, `blake-claimed`,
`btc-refunded`.

Market (versions 1 and 2), 4 types: `market-offer`, `market-take`,
`market-cancel`, `market-match`, carried as signed kind-1 Nostr events with
`t=blake-swap-market`, `base`, `quote`, `direction` tags.

### 1.5 Chain, crypto, coordination

- **30 RPC methods** across descriptor/derive, wallet, rawtx/psbt, mempool,
  block and broadcast families. Descriptors are compiled by the node, so the
  node is the authority on the HTLC script.
- **Settlement** is a PSBT pipeline: `createrawtransaction` → `converttopsbt` →
  `utxoupdatepsbt` → `walletprocesspsbt` → inject the SHA-256 preimage (PSBT key
  `0x0b`) → `finalizepsbt` → `decoderawtransaction` (vsize) → iterate on fee →
  `sendrawtransaction` → persist the exact signed hex.
- **Observation** is wallet-first (watch-only descriptor import), with the
  bounded verbosity-2 block scan as the fallback. **Both are kept.**
- **Coordination** is either a file spool or Nostr: kind-1 events, BIP-340
  signatures, mandatory author pinning, optional NIP-44 v2, relay `REQ`/`EOSE`,
  and a verified-event cache.
- **Crypto**: secp256k1, BIP-340 Schnorr, SHA-256, NIP-44 (ChaCha20 + HMAC +
  HKDF over an ECDH secret).

---

## 2. Scope and shape of the port

**We ship a client; we replicate the server stack it needs.** Three programs:

| Program | Runs on | Job |
|---|---|---|
| `cmd/webpage` + `cmd/webworker` | browser (TinyGo wasm) | the client: presentation, protocol domains, encrypted local state |
| `cmd/blake-swap-server` | a server | serve the app; **dial the nodes**; optionally proxy relays; deployable config |
| `cmd/blake-swap-cli` | a host | drop-in replacement for `swap_v2.py`, used by the existing suites |

### 2.1 Who connects to which socket

This is the one topology question, answered explicitly:

```
browser page  --postMessage-->  browser worker        (two wasm instances)
worker        --HTTPS/JSON-RPC-->  blake-swap-server   (app + node RPC proxy)
blake-swap-server --TCP JSON-RPC-->  bitcoind / BLAKE node
worker        --WSS-->  Nostr relays                   (browser WebSocket, directly)
```

- **The browser never opens a TCP socket.** It cannot, and it should not hold
  node credentials. It speaks HTTPS to our server and WSS to relays.
- **The server is the only thing that dials node RPC.** That is also the only
  place where node credentials, rate limits and an allowlist of RPC methods
  live. `cmd/blake-swap-server` therefore has two faces: static hosting, and
  `/rpc/{btc,blake}` JSON-RPC forwarding.
- **Relays are reached directly by the browser.** A relay proxy on the server is
  optional (useful for a relay that forbids browser origins); it is not on the
  critical path.

Nothing in the client listens on a socket. The server listens on one HTTP(S)
port.

### 2.2 Reuse from `../orly`

Measured with TinyGo 0.43 wasm builds, not assumed:

| Orly package | TinyGo wasm | Verdict |
|---|---|---|
| `pkg/nostr/crypto/keys` | builds and runs | **use as-is**: key generation, pubkey derivation |
| `pkg/nostr/crypto/encryption` | builds and runs (encrypt→decrypt round trip verified) | **use as-is**: NIP-44 v2, plus NIP-04 if ever needed |
| `pkg/nostr/crypto/ec/{schnorr,secp256k1,...}` | builds | **use as-is** for BIP-340 sign/verify |
| `pkg/p256k1` + `.../schnorr`, `.../keys` | builds and runs; **signing** is byte-identical to libsecp256k1, **verification** rejects valid signatures under TinyGo | signing and ECDH only; verification is implemented in `pkg/keys` |
| `pkg/nostr/encoders/{event,filter,tag,kind,envelope}` | builds and runs | **use as-is**: event and filter model |
| `pkg/nostr/ws` | **fails**: `gorilla/websocket` needs `tls.Conn.VerifyHostname` | write a JS `WebSocket` bridge (same pattern as `link`) |
| `pkg/wasmdb` | **fails**: `pkg/database` references `NRCConnection`, undefined in this build configuration | do not use; see the store note below |
| `pkg/database` | fails for the same reason | not used by the client |

One caveat, found by running the official BIP-340 vector under
`tinygo test -target=wasm`: orly's Schnorr *verifier* returns false for a valid
signature, while its *signer* produces exactly the libsecp256k1 signature. A
verifier that rejects everything is a denial of service, so `pkg/keys`
implements BIP-340 verification itself on `math/big` (the arithmetic layer the
port has already proved under TinyGo) and keeps orly for signing and ECDH. The
vectors are generated by libsecp256k1, not by our own code, including a case
whose full public key has odd y so the x-only even-y lift is exercised.

So: **all Nostr data types and the cryptography that works come from orly
unchanged.** The
two platform edges — sockets and durable storage — are ours, because they are
exactly the parts a browser cannot do from Go.

The store finding is worth stating precisely: the raw
`aperturerobotics/go-indexeddb` binding *compiles* under TinyGo and starts, but
`Request.Await` did not resume the Go scheduler in a browser (with either the
default or the asyncify scheduler). Rather than fight it, IndexedDB lives on the
JavaScript side behind the same request/response envelope the link already uses:
`web/store.js` performs `get/put/delete/list`, and Go sees a synchronous-looking
API. That keeps the platform seam in one place and the Go code portable.

### 2.3 Encrypted state at rest

There is no filesystem and no OS key store, so the vault is explicit:

- The user supplies a passphrase at unlock.
- `PBKDF2-HMAC-SHA256` (or scrypt, if it proves cheap enough in wasm) derives a
  32-byte master key from it with a per-vault random salt.
- `conversation_key = HKDF-SHA256(master, info="blake-swap vault v1")`.
- Each record is encrypted with **NIP-44's own construction**
  (`encryption.Encrypt`, `EncryptOptions{Salt: randomNonce32}`): padding,
  ChaCha20, HMAC-SHA256, base64 payload with the `0x02` version byte.
- The plaintext is `recordKey || 0x00 || JSON`. Binding the key into the
  plaintext matters: NIP-44's MAC authenticates the nonce and ciphertext but not
  the storage key, so without this a ciphertext could be moved between records
  and would still decrypt.
- Only ciphertext reaches IndexedDB. The master key lives in worker memory for
  the session; sealed records are unreadable after a reload without the
  passphrase.

### 2.4 The CLI is a compatibility surface, not the architecture

The Go library gets a clean typed API (`pkg/swap`). `cmd/blake-swap-cli` is a
thin shim that reproduces the Python command surface **exactly** — same
subcommand names, flags, positional arguments, output lines that the suites
assert on, and exit codes — so it can be dropped in as `$SWAP_V2` and the
existing 60-suite corpus runs unchanged. That is the cheapest possible
definition of "faithful", and it is why the shim is in scope even though the
browser client never uses it.

---

## 3. The Go package map

Five layers. Arrows are import direction; nothing lower imports anything higher.

```
                     cmd/webpage   cmd/webworker   cmd/blake-swap-server   cmd/blake-swap-cli
                            \           |              /                        /
  app layer           page(P)   app(worker domains)  server(static+rpc proxy)  cli(shim)
                                   |
  orchestration      swap  agent  marketflow
                       |      |        |
  io layer           chain  fund  settle  coord  nostr  store  platform
                       \     |      |      |      |      |      /
  crypto layer            keys  vault  nip44  event          (orly where it works)
                                   |
  value layer    amount  wire  market  state  model  timeout  fees  reconcile  htlc  psbt
```

| Package | Port of | Contents |
|---|---|---|
| `pkg/amount` | `blake_base_units*`, `format_blake_amount` | BLAKE base units ↔ decimal, BTC sats ↔ coin, exact parsing, no float, and `Coin` for the two JSON shapes a node uses for the same quantity |
| `pkg/wire` | `swap_wire.py` | the 7 swap messages as structs, the shared envelope, `Validate`, `Encode`/`Decode` |
| `pkg/market` | market builders/validators | `Offer`/`Take`/`Match`/`Cancel` v1 and v2, direction → roles, binding rules |
| `pkg/state` | `State`, transition tables | the enum, forward and reorg edges, the two transition functions |
| `pkg/model` | `Swap`, `make_swap`, migration | the 42-field record, defaults, invariants, legacy migration |
| `pkg/timeout` | `swap_timeout_policy.py` | `Policy`, `Plan`, `Build`, `Validate`, `Describe` |
| `pkg/fees` | `fee_policy.py` | rate choice, `FeeForVsize`, caps, minimum viable input |
| `pkg/reconcile` | `_reconcile_*` | `Observation`, `Decision`, `Readiness`; the 12-action table, pure |
| `pkg/htlc` | descriptor body, spend classifier | `wsh(or_i(...))` body and the exact witness-shape classifier |
| `pkg/psbt` | `add_sha256_preimage` et al. | minimal PSBT parse/serialize and `0x0b` injection, bounds-checked |
| `pkg/keys` | `nostr_private_key_hex`, pubkeys | thin wrapper over orly `crypto/keys` + `p256k1` |
| `pkg/nip44` | `nip44.py` | thin wrapper over orly `crypto/encryption` + the payload checks Python makes |
| `pkg/event` | `build_nostr_event`, verification | kind-1 events over orly `encoders`, author pinning, event id |
| `pkg/vault` | (new) | passphrase KDF, record encryption with NIP-44 primitives, key binding |
| `pkg/rpc` | `run_chain_cli` | JSON-RPC over HTTPS, timeout, retry allowlist, "state-changing calls are never retried" |
| `pkg/chain` | `btc_cli`/`blake_cli`, wallets | per-chain client, wallet selection, height/utxo/tx reads, confirmations |
| `pkg/fund` | funding intent, descriptor import, watch-only | funding one leg, the crash-window intent record, watch-only import and solvability |
| `pkg/settle` | `build_dynamic_settlement_spend` + builders | PSBT pipeline, fee/vsize convergence, caps, broadcast, rebroadcast |
| `pkg/observe` | `_reconcile_observe_htlc`, scan | wallet-first observation **and** the bounded fallback scan |
| `pkg/coord` | `file_*`/`nostr_*` send/receive | one `Transport` interface: encrypted store spool, or Nostr |
| `pkg/nostr` | relay client, event cache | relay sessions **over a JS WebSocket bridge**, publish report, `REQ`/`EOSE`, cache |
| `pkg/platform` | (new) | the JS seams: WebSocket, IndexedDB store, clock, console |
| `pkg/store` | `save_swap`, spool, bindings | records keyed by (role, swap_id), commit, spool, bindings/selections, tombstones; encrypted at rest |
| `pkg/swap` | the 58 commands | the typed command API: `New`, `Publish*`, `Receive*`, `Fund*`, `Claim*`, `Refund*`, `Sync*`, `Reconcile`, `Next`, `Dashboard`, `Recover` |
| `pkg/agent` | `swap_agent.py` | polling loop, allowlist, role checks, step budget |
| `pkg/marketflow` | market lifecycle | offer, take, match, accept, import, cancel, ignore |
| `pkg/cli` | `argparse` + `main()` | drop-in CLI: parsing, dispatch, output, exit codes |
| `cmd/blake-swap-server` | (new) | static hosting, `/rpc/{btc,blake}` proxy, config, auth token |
| `pkg/jsonrpc` | (new) | page ↔ worker request/response contract |
| `pkg/app`, `pkg/page` | (new / extends) | worker domain wiring; page presentation |

### 3.1 API sketches

```go
// pkg/reconcile
type Observation struct {
    Status Status // Absent, Pending, Funded, Spent, PendingSpend, Unknown, Invalid, Ambiguous
    Branch Branch // None, Claim, Refund
    TxID string; Vout uint32; BlockHash string; Confirmations int32
}
type Decision struct{ Action Action; Class Class; Reason string }
func Decide(sw *model.Swap, role Role, blake, btc Observation, h Heights) Decision

// pkg/store (encrypted at rest)
type Store interface {
    Unlock(ctx context.Context, passphrase string) error
    Load(role Role, id SwapID) (*model.Swap, error)
    Save(sw *model.Swap) error
    Spool(role Role, id SwapID, kind wire.Kind, payload []byte) error
    Take(role Role, id SwapID, kind wire.Kind) ([]byte, error)
}

// pkg/swap
type Engine struct{ BTC, BLAKE *chain.Node; Store store.Store; Coord coord.Coordinator; Fees fees.Policy; Timeouts timeout.Policy }
func (e *Engine) New(role Role, p NewParams) (*model.Swap, error)
func (e *Engine) ClaimBTC(role Role, id SwapID, feeRate *amount.Decimal) (Result, error)
func (e *Engine) Reconcile(role Role, id SwapID) (reconcile.Report, error)
func (e *Engine) Next(role Role, id SwapID) (reconcile.Action, error)
```

---

## 4. Milestones

| # | Milestone | Done when |
|---|---|---|
| M0 | Spikes | **partly done**: orly crypto + NIP-44 + event encoders run under TinyGo; `ws` and `wasmdb` are ruled out. Remaining: JS WebSocket bridge, JS IndexedDB store, node proxy |
| M1 | Value layer (`amount`, `wire`, `market`, `state`, `model`, `timeout`, `fees`) | ported vectors pass under `go test` on the host |
| M2 | `reconcile`, `htlc`, `psbt` | the 12-action table and witness classifier match Python on recorded fixtures |
| M3 | `rpc`/`chain`/`fund`/`settle`/`observe` against a real regtest through the proxy | **partly done**: `rpc` and `chain` are in and tested against the rig; `fund`, `settle` and `observe` remain |
| M4 | `pkg/cli` + `cmd/blake-swap-cli` | the existing suites run with `$SWAP_V2` pointed at the Go binary |
| M5 | `store`/`vault`/`coord`/`nostr` + worker domains | two browser instances complete a swap on regtest |
| M6 | `marketflow` (offer, take, match, cancel, import) + page UI | the whole market cycle driven from the UI |

M4 and the orly reuse are the two big schedule wins: one gives a mechanical
parity test (the corpus already exists), the other removes the crypto and Nostr
data-model work entirely.

---

## 5. Decisions taken

1. **Client only; server stack replicated.** The browser client is the product;
   `cmd/blake-swap-server` exists because something must dial node RPC and host
   the app.
2. **Node access is proxied.** Credentials never enter the browser; the proxy
   allowlists methods and rate-limits.
3. **State is encrypted at rest with the NIP-44 cryptosystem**, passphrase
   unlocked, record keys bound into the plaintext.
4. **Clean Go API, with a drop-in CLI shim** that reproduces the Python command
   surface and output exactly, so the existing suites validate the port.
5. **Watch-only observation and the bounded fallback scan are both kept.**
6. **Market scope is total**: offer, take, cancel, match, accept, import,
   ignore, reverse directions.
7. **Amounts never touch a float.** The node writes the same quantity two
   ways — a bare number from `getrawtransaction`, a quoted string from
   `listunspent` — so `amount.Coin` accepts both and keeps the digits the
   node sent. A float would turn `0.1` into a nearby number and the engine
   would compare its own records against the wrong amount.

Open, deliberately: the passphrase UX (when it is asked, and what happens on a
wrong one), whether the server also proxies relays, and whether the CLI shim
ships in the browser bundle or only on hosts (it should be host-only).
