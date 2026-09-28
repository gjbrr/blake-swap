# Live Coordination, Recovery, and Performance

## Purpose

The swap engine deliberately separates three concerns:

1. live automatic coordination,
2. exhaustive historical recovery,
3. authoritative blockchain settlement.

Nostr is used for discovery and coordination. Signed Nostr events are
independently verified. Blockchain state remains authoritative for funding,
claims, refunds, and reorg recovery.

## Security principles

### Narrow discovery, preserve verification

Relay queries should return the smallest useful candidate set, but every
candidate that can affect protocol state is still cryptographically and
semantically verified locally.

In particular:

- market-match discovery is scoped to the expected taker identity;
- immutable referenced Nostr evidence may be satisfied from the verified
  local cache;
- cached events are re-verified when loaded;
- cache misses fall back to relay lookup when the evidence is needed.

### Live work is not historical recovery

`scripts/blake next` is a live work queue. It may decline expensive network
reconstruction for a signed event that is already outside the automatic
coordination window.

This does NOT make the historical event invalid.

Explicit historical/recovery commands remain exhaustive and may fetch missing
dependencies from relays.

The current automatic match window is 60 minutes.

Protocol/event expiry and automatic-action freshness are separate concepts.
An offer may remain valid for its entire negotiated lifetime even though an
old match is no longer eligible for automatic import by `next`.

## Durable-state behavior

### Terminal refund fast path

For a locally persisted `BLAKE_REFUNDED` swap, `next` first checks whether the
exact recorded BLAKE refund remains confirmed.

If the exact refund is still confirmed, the swap cannot have resumable
settlement work and `next` may skip full cross-chain reconciliation.

If the refund is missing or ambiguous, normal full reconciliation still runs
so reorg recovery remains conservative.

`reconcile` remains the full read-only forensic view.

### Abandoned incomplete negotiations

An `OFFERED` negotiation that never received protocol Bob's settlement
pubkeys may be locally archived when no funding, claim, or refund transaction
has been recorded.

Archival is local only. It does not publish a Nostr event and does not spend
funds.

## Performance work

The following sources of repeated work were removed:

- broad market-match relay queries when taker-scoped queries are available;
- relay round trips for immutable offer/take evidence already present in the
  verified local cache;
- duplicate open-market scans within one `next` pass;
- relay re-fetch of durable match events already present in the verified
  local cache;
- network reconstruction of missing dependencies for ancient match events
  that are already outside `next`'s automatic-action window.

During development, idle `scripts/blake next` latency on the HP test host
improved approximately:

    ~44 seconds
       -> ~14 seconds
       -> ~12 seconds
       -> ~9 seconds
       -> ~4-5 seconds

The exact numbers are environment-dependent and are not protocol guarantees.

After the stale-match dependency optimization, an idle test run performed
zero exact-event relay fallback fetches for the previously identified
historical matches.

## Public-network implications

For public-network packaging, the design is:

- dedicated Bitcoin wallet/node backend;
- small/pruned validating BLAKE node;
- known transaction/outpoint observation rather than large historical block
  scans;
- conservative real-network timeout and confirmation policy;
- explicit fee policy and fee reserves.

A pruned validating BLAKE node is preferred over trusting a public RPC server
when the goal is fully self-verifying operation.

**Known-outpoint observation is implemented.** Each swap's HTLC descriptor is
imported into a dedicated watch-only wallet before that chain is funded
(`docs/ROADMAP.md` §2), so `sync-funding` finds the output with one
`listunspent` for the HTLC's own address and reads its block hash from the
watch-only wallet's `gettransaction`. No block scan, and no transaction index:
the wallet already holds the script, and it is the wallet that knows the
transaction. `docs/KEY_RECOVERY.md` §6 documents the wallets and the explicit
`watch-htlc` command, `dashboard` reports a live swap whose HTLC no loaded
wallet can solve, and `tests/htlc_key_recovery_tests.py` proves the scan-free
observation on the rig by capping the fallback scan at one block, so a run that
could not observe would have to refuse.

The bounded verbosity-2 scan remains the fallback for two cases it is still the
only answer to: a swap funded before the descriptor was imported, and an output
that was already spent before this participant looked (a wallet's `listunspent`
reports unspent outputs, and a spent HTLC is not one). Both are bounded by the
recorded scan anchor and refuse to run unbounded; see
`BLAKE_SWAP_MAX_FUNDING_SCAN_BLOCKS` and
`BLAKE_SWAP_ALLOW_UNANCHORED_FUNDING_SCAN` in `tests/README.md`.

What is still open for a live public deployment, and cannot be settled against
a regtest rig: reviewing the fee policy against real mempool conditions, and
the RPC timeout and retry posture against a node that is merely slow rather
than down.
