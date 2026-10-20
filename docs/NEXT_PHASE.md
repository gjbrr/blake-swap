# next phase: native Neutrino client, Start9 chain-data gateway, and the web app

status: the current client phase stops when the CI is green. this document maps the next
phase and is written to be read by the client. no new feature work starts before the CI
clean-room gate is green.

## executive summary

the product is a non-custodial swap client for BTC and XBT. the wallet keys live in the
client. the chain data comes from BIP157 compact filters, either from a native Neutrino
light node or from a Start9 gateway that serves filters over a Nostr rendezvous. the swap
itself is the existing HTLC protocol over Nostr. the Start9 is a commodity chain-data
source and relay host, not a custodian or a central indexer.

three shells share one Go core: a native desktop client with Neutrino, a web app that
uses a Start9 gateway because a browser cannot speak Bitcoin P2P TCP, and a mobile shell
that reuses the same core. the native client is the strong custody path. the web app is
the no-install path.

## what exists now

the v2.1 browser client has the market, the reverse maker and taker paths, the shared
ignore-match tombstone, the sealed vault with backup and restore, the four wallet slots
and the activity log. it uses the node wallet RPC for UTXO discovery and signing. that is
the caveat: the node operator can see the swap wallet's addresses and UTXOs, and if the
operator is not the user, the operator can sign.

the CI is red at `d8014f7` and at `6f6c8a8`. three failures:

- `internal/docsalign` requires generated wasm and screenshots in a clean checkout.
- `alice-watch` cannot see the HTLC because `importdescriptors` uses `timestamp: "now"`
  after the funding block.
- the pre-BTC refund test compares absolute heights across two chains. the timeout policy
  deliberately compares the encoded windows, not the heights. the test is wrong.

the first work item is to make these green. the watch-only timestamp is a real recovery
bug, not just a test problem.

## the target model

1. **one wallet, two chain profiles.** BTC and XBT use the same transaction format and the
   same cryptography. the differences are the node or peer that follows the chain, the
   genesis hash and the chain parameters. the wallet code is shared. a profile names the
   peer or endpoint, the expected genesis, the bech32 HRP, the coin type and the account
   path. the client verifies the genesis and refuses a mismatch.
2. **the client holds the keys.** the seed is generated in the client and stored sealed in
   the vault. the client derives addresses, selects UTXOs, builds transactions, signs the
   HTLC funding, claim and refund paths, and broadcasts the signed bytes. the node never
   holds the client's keys and never signs for the client.
3. **compact filters are the chain source.** the native client runs Neutrino against a
   BIP157 peer. the web app uses a Start9 gateway that serves filter headers and filters
   over a Nostr rendezvous. the client matches its own scripts locally, so neither the
   gateway nor the relay sees the wallet's addresses. only the funding transaction
   reveals the exact UTXOs, on-chain, at funding time.
4. **the Start9 is a commodity source.** it runs Core for BTC and the Knots build for XBT
   with `blockfilterindex=basic`, plus a Nostr relay. the gateway dials out to a public
   rendezvous relay; the home network needs no inbound port. any BIP157 peer or local
   full node can replace the Start9.
5. **offers carry terms, not outpoints.** an offer names direction, amount, price, timeout
   policy, secret hash and the maker's pubkeys. the taker supplies their pubkeys and a
   payout address. the wallet selects UTXOs at funding time. the receiver supplies no
   UTXOs; the HTLC address is derived from the terms.
6. **three shells, one core.** the wallet, HTLC and swap state machine are Go. the desktop
   shell, the web shell and the mobile shell are replaceable. no shell holds a second
   wallet implementation.
7. **no wallet RPC in the client paths.** the client does not call `listunspent`,
   `getwalletinfo`, `sendtoaddress`, `walletprocesspsbt` or `getnewaddress` on a node
   wallet. it uses chain data and broadcast only. the CLI and the regtest rig may keep a
   node-backed adapter for compatibility, behind the same wallet seam.

## phase A: native Neutrino client (the strong custody path)

the native client is the first deliverable. target Linux first; Windows, macOS and mobile
follow through the same core.

the client is a Go process that owns the vault, the wallet and Neutrino. a thin shell
hosts the UI (Wails preferred, or Tauri or Electron with the Go core as a sidecar). the
shell must not hold the seed, sign, or reimplement wallet logic. the UI may reuse the
existing web views with the transport pointed at the local core.

the wallet:

- a BIP39 mnemonic from 32 bytes of entropy. show it once, require a confirmation, store
  it sealed in the vault. the vault backup must include it. the seed, not the wallet
  database, is the recovery artifact.
- BIP32/BIP84 derivation. P2WPKH for deposits and change, P2WSH for the HTLCs. each chain
  gets a distinct account path so the same seed does not derive the same keys on both
  chains.
- Neutrino for each profile. sync headers and compact filters from the configured peer.
  the wallet scans its own scripts and keeps its own UTXO view. the wallet database is a
  cache and must be rebuildable from the seed plus a rescan.
- coin selection at funding time. largest-first or accumulate is enough for a dedicated
  swap wallet. optional coin control. change to a fresh address. fee rate from a setting
  or the peer's fee estimate. dust handling.
- transaction building and signing for P2WPKH and the HTLC claim and refund witnesses.
  the same builder and signer for BTC and XBT. no per-chain transaction code.
- broadcast through Neutrino or the configured peer. only signed bytes leave the client.
- recovery: restore from the mnemonic, rescan both chains, find an already-funded HTLC.
  this is where the current `timestamp: "now"` bug must be fixed.

the swap seam:

    type Wallet interface {
        Profile() Profile
        Balance(ctx) (Amount, error)
        UTXOs(ctx) ([]UTXO, error)
        NewPubkey(ctx) ([]byte, error)
        FundHTLC(ctx, contract Contract, amount Amount, feeRate FeeRate) (Funding, error)
        ClaimHTLC(ctx, contract Contract, outpoint Outpoint, secret Secret, payout Address) (TxID, error)
        RefundHTLC(ctx, contract Contract, outpoint Outpoint, payout Address) (TxID, error)
        Broadcast(ctx, raw []byte) (TxID, error)
    }

the native client implements it with the wallet and Neutrino. the CLI and the regtest rig
keep a node-backed adapter that implements the same seam, so the existing CLI output and
the parity walk stay meaningful. the engine's funding, settlement and recovery code calls
the seam, not `chain.Wallet` directly. `pkg/swap/handshake.go` gets its pubkey from the
seam, not from `getnewaddress`.

the native client must prove:

- XBT Knots serves BIP157, and the Neutrino client syncs XBT filters on regtest.
- a P2WPKH spend and an HTLC fund, claim and refund signed by the client are accepted on
  both chains.
- a swap completes in both directions with the node holding no keys.
- a restored wallet rescans and sees an already-funded HTLC.

## phase B: the Start9 chain-data gateway

the gateway is a Go daemon on the Start9, separate from the relay and the two nodes. it
dials out to one or more public Nostr relays, subscribes to request events addressed to
its pubkey, forwards chain-data requests to the local Core or Knots node, and returns the
responses over the same relay. the home network needs no inbound port.

the gateway API is chain data only:

- `info`: chains, methods, version, limits, a short name.
- `ping`: liveness and the current tip.
- `headers`: block headers by height or locator.
- `cfheaders`: BIP157 filter headers.
- `cfilters`: compact filters by height range.
- `tx`: raw transaction by txid.
- `txout`: a UTXO by txid and vout.
- `fee`: a fee estimate for a target.
- `broadcast`: a signed raw transaction.

it must never expose `listunspent`, `getwalletinfo`, `getnewaddress`, `sendtoaddress`,
`walletprocesspsbt` or any address query. the client matches filters locally. the gateway
sees heights and hashes, not the wallet's addresses.

the nodes run with `blockfilterindex=basic` on both chains. the gateway serves the filter
headers and filters from the nodes. if the XBT Knots build cannot serve BIP157, the
gateway cannot serve XBT and the web app cannot use the XBT side; this is tested before
the web app is built, not discovered later.

the gateway holds no keys and signs nothing. it is read-only plus broadcast. it enforces
a per-client rate limit and an optional token or allowlist. the relay sees ciphertext; the
gateway sees the requested heights and hashes. the gateway can withhold data, but the
client verifies the header and filter chain, so it cannot forge it.

the gateway is a commodity. anyone can run one. the registry below is how clients find
one.

## phase C: registry and rendezvous protocol

the goal is simple: a web app finds a Start9 gateway on a public relay and talks to it,
without the Start9 having a public inbound address. the gateway is already connected to
the relay the client uses. this is not NIP-46 bunker style. the gateway is not a signer;
it is a chain-data service. the client holds the keys and signs.

the shape borrows from NIP-47 Nostr Wallet Connect: a URI with the service pubkey, one or
more relays, and an optional secret; NIP-44 encrypted request and response events over
the relay. the semantics are chain data, not a wallet.

### registry event

the gateway publishes a replaceable event signed by its own key. the proposed kind is
`30078` (application-specific data) with a `d` tag of `blake-swap-chaindata`, or a new
kind if that collides with an existing use. the content is JSON:

    {
      "v": 1,
      "name": "gary-start9",
      "relays": ["wss://relay.example"],
      "endpoints": ["wss://home.example:8443", "http://192.168.1.10:8080"],
      "chains": ["btc", "xbt"],
      "methods": ["info", "ping", "headers", "cfheaders", "cfilters", "tx", "txout", "fee", "broadcast"],
      "limits": {"max_event_bytes": 60000, "max_filter_range": 2000},
      "updated_at": 1792480000
    }

the event is published to the relays the gateway is connected to. the app queries a
configurable set of public rendezvous relays for these events, verifies the signature,
checks the freshness, and lists the gateways that support the requested chain.

### rendezvous URI

    nostr+chaindata://<gateway-pubkey>?relay=<wss-relay>&bridge=<wss-bridge>&endpoint=<optional>&v=1&chains=btc,xbt

the gateway must already be connected to at least one relay in the URI. the app tries the
listed relays in order for discovery and control. the optional `bridge` is a public
WebSocket tunnel: the gateway dials it, the browser connects with the session id, and the
bridge forwards the byte stream to the gateway. the optional `endpoint` is a direct
WebSocket to the gateway (LAN, public host, or onion). the bridge and the endpoint carry
the same Neutrino P2P byte stream; the relay is the rendezvous and control channel, not
the bulk data path. the URI is the "pubkey plus endpoint" form the product needs, not a
bunker string.

the client generates an ephemeral keypair for the session. the control request is
encrypted to the gateway's pubkey with NIP-44. the response is encrypted to the client's
pubkey. the relay sees ciphertext. an optional `connect` request can carry an allowlist or
payment token; the token stays inside the encrypted content, not in the URI. the gateway
enforces the rate limit.

### request and response

these events are the control and fallback channel: discovery, `info`, `ping`, `connect`,
and small requests when the WebSocket bridge is unavailable. the Neutrino data plane uses
the WebSocket stream, not these events.

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
next gateway in the registry. a gateway that is offline is not a protocol failure, it is
an availability failure. multiple relays and multiple gateways remove the single point.

### security properties

- no keys on the gateway; no signing; no wallet RPC.
- the relay sees ciphertext and traffic metadata, not the wallet's addresses.
- the gateway sees the requested heights, hashes and transactions, not the wallet's
  addresses or UTXOs.
- the client verifies the header chain and the filter headers. a gateway can withhold,
  not forge.
- only the funding transaction reveals the exact UTXOs, on-chain, at funding time.
- the client signs every spend. the gateway can refuse to broadcast, but it cannot spend.

## phase D: the web app

the web app is the no-install path. it downloads the wasm bundle, connects to a public
rendezvous relay, discovers a gateway, and syncs compact filters from it. the client
verifies the header and filter chain and matches its own scripts locally. the wallet and
the seed live in the browser vault; the gateway never sees the addresses.

the browser transport is a WebSocket tunnel to the gateway, not a direct TCP socket. the
TinyGo Neutrino port runs the same peer stack over a `net.Conn` shim; the gateway dials
the bridge, the browser connects, and the stream is forwarded to the node's P2P. the
Nostr events are discovery and control; the bulk filters use the WebSocket.

the first sync is the only heavy moment. block headers are about 72 MB for Bitcoin, filter
headers about 29 MB, and the compact filters are GB-scale. a new swap wallet starts near
the tip, so its first sync is small; a restored old wallet is the heavy case. the 20
minute target is for a local or fast bridge, a checkpoint, and a new wallet. the browser
cache must be persistent: use Cache Storage or IndexedDB, call
`navigator.storage.persist()`, and treat eviction as the real limit on mobile.

the wallet and the Neutrino port are the work. `btcwallet` is the native reference, not a
drop-in wasm library; a minimal TinyGo-compatible wallet or a carefully ported subset is
the realistic path. the wallet and swap logic stay one core, not a second implementation.

the web app is not the strong custody path. a browser is a hostile place for a seed. the
native client keeps the seed in a native process and can use the OS keychain. the web app
is for onboarding and small balances. the UI must say so.

the existing market, reverse maker and taker, ignore-match and vault views are reused. the
web app adds the gateway discovery, the sync status, the wallet balance and the native
warning.

## phase E: mobile shell

the same Go core, a mobile shell later. a gomobile library plus a webview, or a Tauri 2
mobile shell with the Go core. mobile constraints are real: filter sync bandwidth,
storage, background limits, NAT and Tor. sync on demand over Wi-Fi; use the Start9 as the
peer. this is not part of the current phase. the core must stay free of desktop-only
dependencies so this shell is possible.

## milestones and estimate

the current phase stops at M0. the next phase is M1 to M4; M5 follows.

- M0, CI green: fix the docs test, the watch-only timestamp and the timeout test. 1 to 2
  days.
- M1, native spikes: XBT Knots serves BIP157 and a Neutrino client syncs it; the client
  signer produces valid P2WPKH and HTLC spends on both chains. 1 week. if either spike
  fails, stop and report; there is no silent fallback to the node wallet.
- M2, native client MVP: native host, wallet, Neutrino sync, swap in both directions,
  recovery, UI. 2 to 3 weeks.
- M3, gateway and rendezvous: the Start9 gateway, the registry event, the URI, the
  WebSocket bridge, the control events, discovery, filters for both chains, no wallet RPC.
  1.5 to 2 weeks.
- M4, web app MVP: the TinyGo Neutrino port, the wasm client, gateway discovery, WebSocket
  sync, wallet, swap in both directions, browser tests, and the 20 minute first sync
  target. 2 to 3 weeks; the port is the swing item.
- M5, mobile shell: after M4. the estimate is refined then; the Go core must stay
  portable so the shell is a packaging job, not a second client.

M1 to M4 is about 5 to 7 weeks; the TinyGo port is the swing item. the current contract is
usd 200 per week for 8 weeks; this fits the next phase with room for M5 or hardening. the
estimates are for planning, not a fixed quote; the spikes decide the risk.

## acceptance

native client:

- create and restore a wallet from the mnemonic on Linux.
- sync BTC and XBT through Neutrino from the configured BIP157 peers.
- receive on both chains, show the balance, select UTXOs at funding.
- complete a swap in both directions with client-side signing.
- the node holds no keys and the client makes no wallet RPC calls.
- only the funding inputs are visible on-chain; the seed backup restores the wallet.
- the CLI and the regtest rig still pass their parity gates through the node-backed
  adapter.

gateway:

- runs on the Start9 as a separate service, dials out to a public relay, no inbound port.
- publishes a signed registry event on the relays it is connected to.
- serves headers, filter headers, filters, tx, txout, fee and broadcast for both chains.
- exposes no wallet RPC and holds no keys.
- enforces a rate limit and an optional token.
- XBT filters work on regtest; if they do not, the web app's XBT side is blocked and the
  native client is the only path.

web app:

- discovers a gateway from a public relay, verifies the registry signature and freshness.
- connects through the rendezvous URI, syncs filters, creates a wallet, swaps in both
  directions.
- the relay sees ciphertext; the gateway sees heights and hashes, not addresses.
- gateway offline, relay offline, wrong chain, tampered filter and wrong passphrase all
  fail without fund loss.
- browser tests cover the discovery, the sync, the swap and the failure modes.

## open questions

- the registry event kind and the URI scheme. reuse NIP-47's shape; do not collide with
  its kinds. the proposal here is `30078` for the registry and `23196`/`23197` for
  requests and responses, marked as a proposal.
- which public rendezvous relays are the default set. how to avoid spam and abusive
  registrations without a central list.
- endpoint hints: public host, LAN address or onion. what the gateway advertises and what
  leaks.
- session authentication and pricing. a token, an allowlist, or a payment. the gateway
  must not become a custodian.
- the browser light client is the TinyGo Neutrino port over the WebSocket bridge. the open
  question is the port scope and the transport shim, not whether to write a second client.
  the checkpoint strategy needs a decision.
- the browser wallet: a minimal TinyGo-compatible wallet or a ported `btcwallet`. size,
  performance and the TinyGo boundary.
- XBT BIP157 support and the scarcity of public XBT filter peers. the Start9 is likely the
  practical peer.
- data volume and latency over the bridge. the WebSocket is the primary data plane; the
  Nostr control events are the fallback. WebRTC or WebTransport is a later optimization,
  not the MVP.
- mobile shell: gomobile plus webview, or Tauri 2 mobile.
- operations: who runs the public rendezvous relays and the gateways; how they are
  funded; how abuse is handled.

## risks

- XBT Knots does not serve BIP157, or the filter index is too heavy for the Start9. the M1
  spike decides; if it fails, the native client needs a local XBT full node.
- the TinyGo Neutrino port and its btcsuite dependencies. this is the main browser risk;
  the 20 minute sync target is secondary.
- browser storage quota and eviction. a full filter set is GB-scale; a new swap wallet is
  small, an old restored wallet is not.
- browser key security. the web app is the no-install path, not the strong custody path;
  the UI must say so.
- bridge bandwidth, relay limits and latency. use the WebSocket as the data plane and
  multiple bridges/relays; a WebRTC data channel later.
- gateway availability. multiple gateways and relays; the client verifies, so a bad
  gateway can withhold but not forge.
- scope creep. the native client and the gateway are separate milestones; do not build
  the web app before the native wallet and signing are proven.

## not in scope

- a custodian, a central indexer or a wallet RPC for the client.
- a browser extension.
- hardware wallets.
- fiat on-ramps.
- a general-purpose wallet, coinjoin or multi-account history.
- a mobile app in the current phase; the core stays portable.

## immediate stopping point

the current phase stops when M0 is green. no new features before that. the spec is added
to `docs/NEXT_PHASE.md`, the tree is clean, the CI is green, and the tag is left where it
is. the client reviews this document and decides whether M1 to M4 is the next contract.
