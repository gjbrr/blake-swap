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

## Fee policy against real fee markets

A settlement spend is small: on this rig, a **claim** measures 139–140 vB
(it carries the preimage as well as the signature) and a **refund** 131 vB,
both one P2WSH input to one P2WPKH output. What decides whether such a spend
can be paid for is not the estimator but the **safety cap**, which is a
percentage of the HTLC input (`BLAKE_SWAP_MAX_SETTLEMENT_FEE_PERCENT`,
10% by default).

    required_fee  = ceil(vsize * rate)
    cap           = ceil(input * percent / 100)
    affordable    = cap / vsize          # sat/vB this HTLC can pay
    input_minimum = smallest input whose cap reaches required_fee

The cap rounds up, so the boundary is exact rather than approximate:
`fee_policy.minimum_input_for_rate()` computes it, and
`tests/fee_policy_tests.py` checks the documented table against the enforced
cap in both directions.

**What the real market looked like.** Per-block fee percentiles from
mempool.space, `/api/v1/mining/blocks/fee-rates/{period}`, fetched
2026-09-28. `marginal` is `avgFee_0`, the lowest feerate that block actually
included — the rate a settlement has to beat to land in a block like it:

| Period | Blocks | marginal p50 | p90 | p99 | max |
|---|---|---|---|---|---|
| last month | 1 425 | 0.0 | 0.0 | 2.0 | 5.0 |
| last 6 months | 1 473 | 0.0 | 1.0 | 2.0 | 4.0 |
| last year | 1 096 | 0.0 | 1.0 | 2.0 | 3.0 |
| last 2 years | 2 191 | 1.0 | 4.0 | 15.0 | 57.0 |
| last 3 years | 2 193 | 2.0 | 33.8 | 171.08 | 889.0 |

**What that means for swap sizes**, at the 10% default and a 140 vB claim:

| HTLC input | Fee cap | Can afford | Share of the last 3 years needing more |
|---|---|---|---|
| 2 600 sats | 260 sats | 1.857 sat/vB | 55.1% |
| 10 000 sats | 1 000 sats | 7.143 sat/vB | 26.8% |
| 25 000 sats | 2 500 sats | 17.857 sat/vB | 16.3% |
| 100 000 sats | 10 000 sats | 71.429 sat/vB | 4.2% |
| 1 000 000 sats | 100 000 sats | 714.286 sat/vB | 0.0% |

Read the other way round, the smallest HTLC that can settle a 140 vB claim:

| Fee environment | Minimum input |
|---|---|
| 3-year median (2 sat/vB) | 2 791 sats |
| 3-year p90 (33.8 sat/vB) | 47 311 sats |
| 3-year p99 (171.08 sat/vB) | 239 511 sats |
| 3-year maximum (889 sat/vB) | 1 244 591 sats |

The practical consequence, and the reason this section exists: the small
sats amounts used in the regtest suites and the examples are chosen for
test speed, not for fee-market viability. A 2 600 satoshi HTLC can only be
settled while fees are under ~1.86 sat/vB, which was true in 44.9% of the
last three years' blocks. The engine **refuses** rather than paying a
larger share of the HTLC — that is the cap doing its job, not a bug — and
the refusal now says which rate the cap permits, how small the HTLC is
against the current rate, and which knob to change.

The controls, in the order to reach for them:

| Knob | Effect |
|---|---|
| `--fee-rate` | the client's explicit settlement rate in sat/vB, on `claim-btc`, `claim-blake`, `refund-btc`, `refund-blake`, `run` and `resume`. **A parameter outranks the environment**, including the exact-satoshis variable: the environment is the host's default, and this is the instruction for this invocation. An unusable value is a usage error, refused before any state is read |
| `BLAKE_SWAP_MAX_SETTLEMENT_FEE_PERCENT` | the cap as a share of the HTLC input; raising it makes small swaps settleable at the cost of the fee being a larger share of the swap |
| `BLAKE_SWAP_MAX_SETTLEMENT_FEE_SATS` | a flat ceiling, applied in addition to the percentage. It can only make the cap stricter, and it is the right control for a *large* HTLC in a spike, where a percentage can permit far more than an operator wants to pay |
| `BLAKE_SWAP_FEE_TARGET_BLOCKS`, `BLAKE_SWAP_FEE_RATE_MULTIPLIER` | what the estimator is asked for, and how much above it to pay; neither helps if the cap is the binding constraint |
| `BLAKE_SWAP_BTC_SPEND_FEE_RATE` / `BLAKE_SWAP_BLAKE_SPEND_FEE_RATE` | an explicit **rate** in sat/vB, for a client that reads the fee market itself; takes precedence over the estimator, and over `FEE_TARGET_BLOCKS`/`FEE_RATE_MULTIPLIER`, which it makes irrelevant |
| `BLAKE_SWAP_BTC_SPEND_FEE_SATS` / `BLAKE_SWAP_BLAKE_SPEND_FEE_SATS` | an explicit **fee** in sats, for an emergency; the most specific *environment* knob, so it wins over the rate variable; it is still subject to both caps |

The rate is available two ways, and they are not equal. `--fee-rate` is the
parameter a client passes on the command that spends the HTLC; the
`BLAKE_SWAP_*_SPEND_FEE_RATE` variables are the host's standing default. When
both are present the parameter wins — and if the exact-satoshis variable is also
set, the run says so on stdout, because an operator's override being silently
passed over is worse than no override at all. Fee policy has no *other* option:
funding transactions let the node wallet choose the fee at
`BLAKE_SWAP_BTC_FUNDING_FEE_TARGET_BLOCKS`, so `--fee-rate` applies to
settlement spends, not to funding.

Precedence, most explicit first: `--fee-rate`, an exact **fee** in sats from the
environment, an exact **rate** in sat/vB from the environment, then the node's
estimator at the configured target with the configured multiplier. Both caps
apply to every path — an override says what to pay, not what may be paid.

The rate knob is the one that matters for the intended clients. This engine
does not know how urgent a particular swap is, and a node's estimator is one
opinion about the next few blocks; a client that subscribes to the fee market
(and can see the mempool, not just a three-block estimate) should compute
`current rate + its own margin`, pass it here, and keep the decision at the
edge where the information is. A rate below the node's acceptance floors
(`minrelaytxfee`, or the higher `mempoolminfee` while the mempool is
congested) is **refused** rather than attempted: a settlement that cannot be
relayed is a stalled swap, and the refusal names the variable, the rate, and
the floor it failed against.

Deliberately **not** done: raising the default cap so that the test-sized
swaps settle in a median fee environment. A 10% fee is already a large
share of a swap to pay, and quietly making it larger to keep small test
amounts working would trade a visible refusal for an invisible cost. The
sizes are a documentation and test-fixture matter; the cap is an economic
safety control.

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

The fee policy has now been reviewed against real fee-market history (see the
section above): the estimator is not the binding constraint, the percentage cap
is, and the numbers that follow from it are documented with their minimum
viable swap sizes. What remains open for a live public deployment is the RPC
timeout and retry posture against a node that is merely slow rather than down,
which needs a failing node rather than historical data.
