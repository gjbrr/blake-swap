# next phase: one wasm web app and the Start9 chain-data package

status: the current client phase stops when the CI is green. this document maps the next
phase and is written to be read by the client. no new feature work starts before the CI
clean-room gate is green.

## executive summary

the product is a non-custodial swap client for BTC and XBT, and it is one web app. the
wallet, the swap engine and the chain client are one Go codebase compiled to wasm. the
chain data is BIP157 compact filters served by the Start9 package over a Nostr
rendezvous, and the same package keeps the coordination record and history for the swaps
that pass through it. the Start9 is a commodity chain-data source and rendezvous host,
not a custodian and not a chain indexer.

there is one build and one client. there is no native client, no mobile app, no sidecar
and no second codebase. the same wasm bundle can be wrapped in a desktop shell later with
no change to the code, but that is a packaging option, not a deliverable and not part of
this phase.

the Start9 package at `~/s/gjbrr/blake-swap-startos` is part of the work: its
configuration enables the BIP157 compact filter capability on the chain nodes, and the
package runs the server that starts the rendezvous listener.

## what exists now

the v2.1 browser client has the market, the reverse maker and taker paths, the shared
ignore-match tombstone, the sealed vault with backup and restore, the four wallet slots
and the activity log. it uses the node wallet RPC for UTXO discovery and signing. that is
the caveat this phase removes: the node operator can see the swap wallet's addresses and
UTXOs, and if the operator is not the user, the operator can sign.

the CI is red at `d8014f7` and at `6f6c8a8`. three failures:

- `internal/docsalign` requires generated wasm and screenshots in a clean checkout.
- `alice-watch` cannot see the HTLC because `importdescriptors` uses `timestamp: "now"`
  after the funding block.
- the pre-BTC refund test compares absolute heights across two chains. the timeout policy
  deliberately compares the encoded windows, not the heights. the test is wrong.

the first work item is to make these green. the watch-only timestamp is a real recovery
bug, not just a test problem.

## the target model

1. **one web app, one build, wasm.** the wallet, the HTLC logic, the swap state machine
   and the chain client are Go, compiled to wasm, loaded by the page. the page is the
   product. no shell owns a second implementation of anything.
2. **keys live in the wasm client.** the seed is generated in the client and stored
   sealed in the vault. the client derives addresses, selects UTXOs, builds transactions,
   signs the HTLC funding, claim and refund paths, and broadcasts the signed bytes. the
   node never holds the client's keys and never signs for the client.
3. **compact filters are the chain source.** the client matches its own scripts locally,
   so the gateway never sees the wallet's scanning set or its other addresses. it sees
   only what the swap record carries and what funding reveals on-chain.
4. **one transport for the page and the gateway.** a browser cannot open a Bitcoin P2P
   TCP socket and the Start9 has no inbound port, so both ends meet at a public Nostr
   relay. the gateway dials out and starts the rendezvous listener; the page opens a
   WebSocket bridge for bulk filters. Nostr is discovery and control only.
5. **the Start9 package is the chain-data source and the swap record.** its
   configuration enables `blockfilterindex=basic` for BTC Core and XBT Knots, and it runs
   the gateway server that serves filter headers and filters and starts the rendezvous
   listener. it also stores the coordination record of each swap done through it: terms,
   participants, state and txids. it holds no keys, signs nothing and learns nothing
   beyond the coordination. any BIP157 peer can replace the chain-data half; the swap
   record belongs to the rendezvous half.
6. **offers carry terms, not outpoints.** an offer names direction, amount, price, timeout
   policy, secret hash and the maker's pubkeys. the taker supplies their pubkeys and a
   payout address. the wallet selects UTXOs at funding time. the receiver supplies no
   UTXOs; the HTLC address is derived from the terms.
7. **one wallet, two chain profiles.** BTC and XBT use the same transaction format and the
   same cryptography. a profile names the chain parameters, the expected genesis, the
   bech32 HRP, the coin type and the account path. the client verifies the genesis and
   refuses a mismatch. the wallet code is shared; the profile is data, not a branch.
8. **no wallet RPC in the client paths.** the page does not call `listunspent`,
   `getwalletinfo`, `getnewaddress`, `sendtoaddress`, `walletprocesspsbt` or any address
   query on a node wallet. it uses chain data and broadcast only. the CLI and the regtest
   rig may keep a node-backed adapter for compatibility, behind the same wallet seam.

## phase A: the wasm client, chain source and signing

the wasm client carries the whole product, so this is where the risk is and where the
first spike goes. there is no second native client to de-risk it.

the chain source:

- the browser transport is a WebSocket tunnel to the gateway, not a direct TCP socket.
  the TinyGo Neutrino peer stack runs over a `net.Conn` shim; the gateway forwards the
  stream to the node's P2P.
- the client syncs block headers, filter headers and compact filters, and verifies the
  header chain and the filter header chain itself. a gateway can withhold, not forge.
- the first sync is the only heavy moment. block headers are about 72 MB for Bitcoin,
  filter headers about 29 MB, and the compact filters are GB-scale. a new swap wallet
  starts near the tip, so its first sync is small; a restored old wallet is the heavy
  case. the 20 minute target is for a new wallet, a fast bridge and a checkpoint.
- storage is IndexedDB or Cache Storage. call `navigator.storage.persist()` and treat
  eviction as the real limit, especially on small devices.

the wallet:

- BIP39 mnemonic from 32 bytes of entropy. show it once, require a confirmation, store it
  sealed in the vault. the vault backup must include it. the seed, not the wallet
  database, is the recovery artifact.
- BIP32/BIP84 derivation. P2WPKH for deposits and change, P2WSH for the HTLCs. each chain
  gets a distinct account path so the same seed does not derive the same keys on both
  chains.
- coin selection at funding time. largest-first or accumulate is enough for a dedicated
  swap wallet. optional coin control. change to a fresh address. fee rate from a setting
  or a gateway estimate. dust handling.
- signing for P2WPKH and the HTLC claim and refund witnesses happens in wasm. the same
  builder and signer serve BTC and XBT. only signed bytes leave the client.
- recovery: restore from the mnemonic, rescan both chains, find an already-funded HTLC.
  the `timestamp: "now"` class of bug must not exist here.

`btcwallet` is the native reference, not a drop-in wasm library. a minimal
TinyGo-compatible wallet or a carefully ported subset is the realistic path, and it stays
the only wallet implementation.

the spike must prove, on regtest:

- the Neutrino subset syncs headers and filters from the gateway over the bridge on both
  chains.
- a P2WPKH spend and an HTLC fund, claim and refund signed in wasm are accepted by both
  nodes.
- a restored wallet rescans and sees an already-funded HTLC.

if a spike fails, stop and report. there is no fallback to the node wallet.

## phase B: the Start9 package

the package is at `~/s/gjbrr/blake-swap-startos` (repo `gjbrr/blake-swap-startos`,
branch `master`). it is part of this phase, not an external dependency.

the node configuration:

- enable `blockfilterindex=basic` for BTC Core and XBT Knots, for each chain the gateway
  offers. the package sets the setting or requires it, restarts the node on change, and
  refuses to start the gateway until the filter index reports ready.
- verify with `getblockfilter` at a known block hash on both chains before announcing the
  gateway.

the gateway server:

- runs inside the package, chain data only: `info`, `ping`, `headers`, `cfheaders`,
  `cfilters`, `tx`, `txout`, `fee`, `broadcast`.
- never exposes `listunspent`, `getwalletinfo`, `getnewaddress`, `sendtoaddress`,
  `walletprocesspsbt` or any address query. read-only plus broadcast.
- starts the rendezvous listener on boot: dials the configured public relays, publishes
  presence, subscribes to requests addressed to its pubkey, returns chunked responses and
  accepts the WebSocket bridge. the home network needs no inbound port.
- config and volume: relay list, gateway pubkey generated on first start, offered chains,
  rate limit, optional allowlist. survives restart and package update.
- health: node tip against gateway tip, filter index height per chain, relay connections,
  bridge sessions.

the swap record:

- both ends use the same gateway, so the gateway is the natural holder of the interaction
  history. it stores, per swap: the offer terms, the participant pubkeys, the state
  machine position, the funding txids, the claim or refund txid, and timestamps.
- it stores nothing else. no seed, no private key, no wallet descriptor, no address scan,
  no preimage before the claim reveals it on-chain. no KYC and no external query.
- the record is what lets either end resume after a restart and what the web app shows as
  history. it is the coordination history, not a chain index.
- the record is deliberately not replicated or pooled across gateways. aggregating the
  orderbook is safe because offers are public by design; pooling swap records would join
  each participant's separate interactions into one linkable graph. a gateway holds only
  the swaps coordinated through it, and the client's own vault is the only place a
  cross-relay history can exist.
- retention is a setting: keep, or prune after a stated period. clarity here matters more
  than the default.

the gateway holds no keys and signs nothing. the relay sees ciphertext; the gateway sees
the requested heights and hashes plus the swap record it stores. the gateway can withhold
data, but the client verifies the header and filter chain, so it cannot forge it.

the package's current operator-driven CLI alpha is not the target shape. its swap actions
either become operator actions behind the new gateway or retire; the package's primary
role in this phase is chain data and rendezvous. do not keep two competing service models
in one package.

## phase C: registry and rendezvous protocol

the goal is simple: the web app finds a Start9 gateway on a public relay and talks to it,
without the Start9 having a public inbound address. the gateway is already connected to
the relay the client uses. this is not NIP-46 bunker style. the gateway is not a signer
and has no key material beyond its own transport identity.

a gateway advertises a URI:

    nostr+chaindata://<gateway-pubkey>?relay=<wss-relay>&bridge=<wss-bridge>&endpoint=<optional>&v=1&chains=btc,xbt

- the gateway must already be connected to at least one relay in the URI.
- `bridge` is an optional public WebSocket tunnel.
- `endpoint` is an optional direct WebSocket or HTTP address for LAN, public or onion.
- the client generates an ephemeral keypair. the content is NIP-44 encrypted, so the
  relay sees ciphertext.

proposed event kinds: `23196` for a request and `23197` for a response, distinct from
NIP-47's `23194` and `23195`. a request has a `p` tag for the gateway, the content is
NIP-44 encrypted JSON:

    {"id": "req-1", "method": "cfilters", "params": {"from": 900000, "to": 900100}}

a response has a `p` tag for the client and an `e` tag for the request event id. the
content is encrypted JSON:

    {"id": "req-1", "result": {...}, "error": null}

large responses are chunked. each chunk carries a `seq` and a `more` flag; the client
reassembles and verifies. the default maximum event size is 60 KB; the gateway refuses a
range that would exceed it and tells the client to split. the chunking path is the
fallback; the WebSocket bridge is the primary data plane.

### presence and failure

the registry's `updated_at` is the freshness signal. a `ping` returns the tip and the
gateway version. if a request times out, the app tries the next relay in the URI, then the
next gateway in the registry. a gateway that is offline is an availability failure, not a
protocol failure. multiple relays and multiple gateways remove the single point.

### security properties

- no keys on the gateway; no signing; no wallet RPC.
- the relay sees ciphertext and traffic metadata, not the wallet's addresses.
- the gateway sees the requested heights, hashes and transactions, plus the swap record
  it stores. it does not see the wallet's scanning set, its other addresses or its UTXOs.
- the client verifies the header chain and the filter headers. a gateway can withhold,
  not forge.
- only the funding transaction reveals the exact UTXOs, on-chain, at funding time.
- the client signs every spend. the gateway can refuse to broadcast, but it cannot spend.
- there is no cross-gateway swap record query and no replication of the record. one
  gateway cannot learn what another gateway hosted.

## phase D: the web app

this is the deliverable. it downloads the wasm bundle, connects to a public rendezvous
relay, discovers a gateway, and syncs compact filters from it. the wallet and the seed
live in the browser vault; the gateway never sees the addresses.

the existing market, reverse maker and taker, ignore-match, vault, wallet slot and
activity views are reused. the web app adds gateway discovery, the sync status, the wallet
balance, receive and funding. the page holds no node credentials and makes no wallet RPC
calls.

the history view reads the gateway's swap record for swaps coordinated there, and the
local vault for everything else. the record is the interaction history, never a balance
authority: balances come from the client's own filter scan.

the browser is the only place the seed lives, so the vault and the export path are the
security surface. the UI states this plainly. storage is persistent only if the user
grants it; an evicted cache means a rescan, not a lost wallet, because the seed is the
recovery artifact.

the browser tests grow from the current 95 assertions to cover discovery, sync, balance,
receive, funding, the swap in both directions and the restore path.

## no native, no mobile

the same wasm bundle can be wrapped in a desktop shell later with no change to the code.
that is a one-line packaging claim, not a milestone, a target or a second client. there
is no mobile app and no native build in this phase. if a hypothetical shell ever needs
logic, the logic goes into the wasm core.

## milestones and estimate

the current phase stops at M0. the next phase is M1 to M4.

- M0, CI green: fix the docs test, the watch-only timestamp and the timeout test. 1 to 2
  days.
- M1, wasm chain source and signing spike: the WebSocket bridge, the `net.Conn` shim, the
  Neutrino subset, and valid P2WPKH and HTLC spends signed in wasm on both chains. 1 to
  1.5 weeks. if a spike fails, stop and report.
- M2, Start9 package: `blockfilterindex=basic` on both chains, the gateway API, the
  rendezvous listener, the bridge, chunking, health, package config and volume. 1.5 to 2
  weeks.
- M3, web app MVP: discovery, sync, wallet, receive, funding, swap in both directions,
  recovery, browser tests, the 20 minute first sync target. 2 to 3 weeks; the TinyGo port
  is the swing item.
- M4, hardening and acceptance: restore, eviction, relay loss, gateway loss, long sync,
  and the clean-room CI. 3 to 5 days.

M1 to M4 is about 5 to 7 weeks; the TinyGo port is the swing item. the current contract is
usd 200 per week for 8 weeks; this fits the next phase with room for hardening. the
estimates are for planning, not a fixed quote; the spikes decide the risk.

## acceptance

web app:

- open the page with no install, create and restore a wallet from the mnemonic.
- discover a gateway through a public relay with no inbound port on the Start9.
- sync BTC and XBT filters through the gateway, verify both chains, show the tip and the
  balance.
- receive on both chains, select UTXOs at funding, complete a swap in both directions
  with client-side signing.
- the node holds no keys and the page makes no wallet RPC calls.
- only the funding inputs are visible on-chain; the seed backup restores the wallet.
- the restored wallet rescans and sees an already-funded HTLC.
- the gateway keeps the swap record across a restart and both ends see the same history.
- the existing browser assertions and the new ones pass in a clean checkout.

Start9 package:

- a fresh install plus configuration produces `blockfilterindex=basic` on both chains and
  a running gateway.
- the gateway announces on the configured relay and the web app syncs through it.
- no inbound port, no keys and no wallet RPC on the box.
- the package restarts cleanly and the gateway returns with its pubkey and config intact.

## open questions

- XBT BIP157 support and the scarcity of public XBT filter peers. the Start9 is likely the
  practical peer; the M1 spike decides.
- data volume and latency over the bridge. the WebSocket is the primary data plane; the
  Nostr control events are the fallback. WebRTC or WebTransport is a later optimization,
  not the MVP.
- operations: who runs the public rendezvous relays and the gateways; how they are
  funded; how abuse is handled.
- swap record retention and deletion: how long a gateway keeps history, and whether a
  user can ask for deletion.
- whether the existing package's swap actions are kept as operator actions or retired
  once the web app owns the flow.

## risks

- the TinyGo Neutrino port and its btcsuite dependencies. this is the main risk; the 20
  minute sync target is secondary.
- XBT Knots does not serve BIP157, or the filter index is too heavy for the Start9. the M1
  spike decides; if it fails, the gateway cannot serve XBT.
- browser storage quota and eviction. a full filter set is GB-scale; a new swap wallet is
  small, an old restored wallet is not.
- browser key security. the page is the only custody surface; the vault and export path
  must be the strongest code in the tree.
- bridge bandwidth, relay limits and latency. use the WebSocket as the data plane and
  multiple bridges and relays.
- gateway availability. multiple gateways and relays; the client verifies, so a bad
  gateway can withhold but not forge.
- scope creep. there is no native target and no mobile target. do not build a shell, a
  second build or a second client before the web app is accepted.

## not in scope

- a native client, a desktop shell, a mobile app or a second build.
- a browser extension.
- hardware wallets.
- fiat on-ramps.
- a general-purpose wallet, coinjoin or multi-account history.
- a custodian, a central chain indexer or a wallet RPC for the client.
- WebRTC or WebTransport before the WebSocket bridge is proven.

## immediate stopping point

the current phase stops when M0 is green. no new features before that. the spec is added
to `docs/NEXT_PHASE.md`, the tree is clean, the CI is green, and the tag is left where it
is. the client reviews this document and decides whether M1 to M4 is the next contract.
