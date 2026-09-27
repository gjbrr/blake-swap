# BTC ↔ BLAKE Atomic Swap Protocol

**Status:** Experimental Draft 0.3
**Reference implementation:** `f61eeb285914216ec130fe78d1b8c635d3e28315`
**Clean-room reproduction baseline:** `3ac62777eb3e47a5ed99f7000f4f4015a0a6c318`

This document specifies the protocol behavior implemented and tested by
the BTC ↔ BLAKE atomic-swap prototype. It describes **implemented
experimental behavior**. It is not a claim of production readiness or a
formal security proof.

The key words **MUST**, **MUST NOT**, **SHOULD**, **SHOULD NOT**, and
**MAY** describe protocol requirements.

Message formats are specified separately in `docs/WIRE_PROTOCOL.md`.

---

## 1. Scope

The protocol enables two mutually distrustful participants to exchange
BTC and BLAKE without a trusted custodian, using Bitcoin-style HTLCs on
both chains and Nostr for coordination.

The protocol separates three functions:

- **discovery and coordination** — Nostr,
- **participant identity** — secp256k1 signing keys,
- **economic settlement** — Bitcoin and BLAKE.

After negotiation, the blockchains **MUST** contain sufficient economic
evidence for participants to recover settlement progress without
depending on continued delivery of protocol messages. Nostr is a
convenience, not a dependency of last resort.

---

## 2. Roles and identity

### 2.1 Protocol Alice

Alice sells BLAKE and buys BTC. Alice:

1. generates the swap secret `S`,
2. commits to `H = SHA256(S)`,
3. funds the BLAKE HTLC first,
4. verifies Bob's BTC funding,
5. claims BTC by revealing `S` on Bitcoin,
6. refunds BLAKE if the swap fails before Bob funds BTC.

### 2.2 Protocol Bob

Bob sells BTC and buys BLAKE. Bob:

1. receives `H` but MUST NOT receive `S` during negotiation,
2. verifies Alice's BLAKE funding,
3. funds the BTC HTLC,
4. observes Alice's BTC claim,
5. extracts `S` from the confirmed Bitcoin transaction,
6. verifies the extracted secret against `H`,
7. claims BLAKE,
8. refunds BTC if Alice does not claim before the BTC timeout.

### 2.3 Protocol role is not participant identity

A participant's **Nostr identity** is durable and independent of
protocol role. The same identity can be protocol Alice in one swap and
protocol Bob in another.

Protocol role in a specific swap is derived from the identities pinned
into that swap record:

- if the current participant's Nostr public key equals
  `swap.nostr_alice_pub`, the local role is **Alice**;
- if it equals `swap.nostr_bob_pub`, the local role is **Bob**;
- if it matches **both**, the implementation raises
  `SECURITY FAILURE: current participant is pinned as both protocol roles`;
- if it matches **neither**, it raises
  `SECURITY FAILURE: current Nostr identity is not pinned to this swap`.

`BLAKE_SWAP_ROLE` selects a local **profile** (state directory, wallet,
key location). It is a development convenience, not protocol state.
Business logic derives the protocol role from the swap record.

---

## 3. Cryptographic commitment

Alice generates a 32-byte secret:

```
S  ∈ {0,1}^256
H  = SHA256(S)
```

`H` is published in the `offer`. `S` never leaves Alice's local state
and is revealed only through Alice's confirmed Bitcoin claim
transaction. Bob recovers `S` from that transaction's witness.

The same `H` is embedded in both HTLCs. That equality is the atomicity
link: the only way to take the BTC HTLC is to reveal a preimage of `H`,
and revealing it lets Bob take the BLAKE HTLC.

---

## 4. HTLC contracts

### 4.1 Construction

Scripts are Miniscript descriptor strings assembled by the
implementation and compiled by each chain's own node. There is no local
script builder.

**BLAKE HTLC** (`swap.blake_bob_pub`, `swap.blake_alice_pub`):

```
wsh(or_i(
      and_v(v:pk(<blake_bob_pub>), sha256(<H>)),
      and_v(v:after(<blake_timeout>), pk(<blake_alice_pub>))
    ))
```

**Bitcoin HTLC** (`swap.btc_alice_pub`, `swap.btc_bob_pub`):

```
wsh(or_i(
      and_v(v:pk(<btc_alice_pub>), sha256(<H>)),
      and_v(v:after(<btc_timeout>), pk(<btc_bob_pub>))
    ))
```

The parties are swapped between chains, which is what makes each side
incentive-compatible.

### 4.2 Semantics

`or_i` compiles to `OP_IF`/`OP_ELSE`. **The first branch is the claim
branch** and is selected by pushing true; the second is the refund
branch.

- **Claim branch** — requires a signature by the claiming party **and**
  a 32-byte preimage whose `SHA256` equals `H`
  (`OP_SHA256 <H> OP_EQUALVERIFY`).
- **Refund branch** — requires the refund timelock to be satisfied and
  a signature by the refunding party.

On BLAKE, **Bob** claims and **Alice** refunds. On Bitcoin, **Alice**
claims and **Bob** refunds.

### 4.3 Timelocks

Timeouts are **absolute block heights** expressed with Miniscript
`after(T)`, which compiles to `OP_CHECKLOCKTIMEVERIFY`. There is **no**
`older()`/`OP_CHECKSEQUENCEVERIFY` relative timelock anywhere in the
implementation.

Refund spends set transaction `nSequence` to `0xfffffffe` (enabling
locktime) and `nLockTime` to the relevant timeout height.

### 4.4 Contract keys

The HTLC public keys are **per swap**, not per participant. Each side
creates its own chain keys from its local wallet and publishes only its
own half:

- Alice creates and publishes `blake_alice_pub` and `btc_alice_pub`.
- Bob creates and publishes `blake_bob_pub` and `btc_bob_pub`.

Because the keys arrive in signed coordination messages, the chain
trust relationship is anchored in the transport signature plus the
pinned Nostr identity — not in any pre-shared key material.

---

## 5. Timeout policy (v1.5)

### 5.1 Why heights must not be compared

BTC and BLAKE produce blocks independently. Their **heights are not
comparable**, and the protocol deliberately does **not** assert
`blake_timeout > btc_timeout` as raw numbers. `validate_timeout_plan()`
documents this explicitly.

Safety is instead reasoned about in **time**, using explicit worst-case
assumptions.

### 5.2 Default parameters

`TimeoutPolicy` (`src/swap_timeout_policy.py`):

| Parameter | Value |
|---|---|
| `btc_block_seconds` | 600 |
| `blake_block_seconds` | 600 |
| `block_delay_multiplier` | 2.0 |
| `observation_seconds` | 900 |
| `restart_seconds` | 900 |
| `broadcast_seconds` | 900 |
| `btc_claim_confirmations` | 2 |
| `blake_claim_confirmations` | 2 |
| `cross_chain_safety_seconds` | 7200 |
| `minimum_recovery_seconds` | 14400 |

Each block is budgeted at `600 × 2.0 = 1200` seconds, deliberately
assuming blocks may arrive slower than nominal.

### 5.3 Derived budgets

With the default policy:

```
Alice BTC window  = observation + restart + broadcast
                    + (btc_claim_confirmations × 1200)
                    + cross_chain_safety
                  = 900 + 900 + 900 + 2400 + 7200 = 12300 s (3.42 h)

Bob recovery      = observation + restart
                    + (btc_claim_confirmations × 1200)
                    + broadcast
                    + (blake_claim_confirmations × 1200)
                    + cross_chain_safety
                  = 900 + 900 + 2400 + 900 + 2400 + 7200 = 14700 s (4.08 h)
                    (subject to the 14400 s absolute minimum)
```

### 5.4 Encoding into block heights

`build_timeout_plan()` converts these budgets into integer block counts
in two steps so that rounding can never eat into Bob's recovery budget:

1. Encode Alice's BTC window into whole BTC blocks:

   ```
   btc_blocks        = ceil(12300 / 600)      = 21
   encoded_btc_window = 21 × 600              = 12600 s (3.50 h)
   ```

2. Add Bob's **full** recovery budget to the **already-rounded** BTC
   window, then encode into whole BLAKE blocks:

   ```
   required_blake_window = 12600 + 14700      = 27300 s
   blake_blocks          = ceil(27300 / 600)  = 46
   encoded_blake_window  = 46 × 600           = 27600 s (7.67 h)
   ```

The persisted CLTV heights are therefore `btc_height + 21` and
`blake_height + 46`, and the encoded recovery gap is
`27600 − 12600 = 15000 s`, which is ≥ the required 14700 s **after**
rounding.

### 5.5 Validation

`validate_timeout_plan()` requires:

- `btc_timeout_height > btc_start_height`;
- `blake_timeout_height > blake_start_height`;
- `blake_window_seconds − btc_window_seconds ≥ bob_recovery_budget_seconds`;
- the same gap `≥ minimum_recovery_seconds`.

### 5.6 Bob's independent re-validation

Bob **MUST NOT** trust Alice's advertised timeout metadata. On receiving
an `offer`, and before creating settlement keys, saving swap state, or
transmitting an `accept`, Bob:

1. requires `timeout_policy_version == "1.5"` (no legacy fallback);
2. requires all timeout fields to be present and positive integers;
3. reads **his own** current BTC and BLAKE tips;
4. rejects the offer if either advertised timeout has already expired;
5. derives **his own** minimum acceptable policy plan from his own
   `TimeoutPolicy`;
6. converts Alice's **actual** advertised heights into estimated
   remaining wall-clock windows using his own block-interval
   assumptions;
7. requires the actual BTC window to be at least his required BTC
   window;
8. requires the actual cross-chain recovery gap to be at least his
   required recovery budget;
9. additionally requires the signed metadata itself to be coherent:
   advertised windows must meet his policy and must satisfy
   `blake_window − btc_window ≥ advertised_recovery`.

Checks 7–8 are performed from the **actual CLTV heights**, so a
dishonest metadata block cannot substitute for real safety. Checks 3–8
are what defeat the "Time Mallory" attack; the regression suite
`tests/swap_timeout_negotiation_tests.py` covers this.

### 5.7 Timeout refresh

Before acceptance, Alice **MAY** rebuild her timeout plan against
current chain tips with `refresh-timeouts`. This is refused if:

- the swap is not in `OFFERED`;
- any of `blake_bob_pub`, `btc_bob_pub`, or `nostr_bob_pub` is set
  (`SECURITY FAILURE: cannot refresh timeouts after counterparty
  acceptance`);
- any funding evidence field is set;
- a funding intent file exists for either chain.

Refresh preserves `swap_id`, `S`, `H`, and both economic amounts, and
verifies each of those invariants explicitly after rebuilding.

---

## 6. Swap state machine

```
OFFERED
   │
   ▼
BLAKE_FUNDED ──────────────► BLAKE_REFUNDED   (terminal)
   │
   ▼
BTC_FUNDED ────────────────► BTC_REFUNDED
   │                              │
   ▼                              ▼
SECRET_REVEALED              BLAKE_REFUNDED   (terminal)
   │
   ▼
BTC_CLAIMED
   │
   ▼
BLAKE_CLAIMED
   │
   ▼
COMPLETE                                      (terminal)
```

Transitions are enforced by an explicit adjacency table. Any other
transition raises `Illegal transition: <from> -> <to>`.

Two notes:

- `SECRET_REVEALED` is a **transient** intermediate state, used by both
  roles as a proven ordering step when a confirmed Bitcoin claim is
  established. The implementation always advances it immediately to
  `BTC_CLAIMED`. A `SECRET_REVEALED` state persisted on disk is treated
  as a security failure: `sync-settlement` raises
  `SECURITY FAILURE: durable state is unexpectedly SECRET_REVEALED`.
- Both refund paths are legitimate and are **not** symmetric:
  `BLAKE_FUNDED → BLAKE_REFUNDED` is Alice aborting before Bob funds
  BTC; `BTC_FUNDED → BTC_REFUNDED → BLAKE_REFUNDED` is Bob refunding
  after Alice failed to claim, after which Alice refunds BLAKE.

---

## 7. Negotiation and settlement flow

### 7.1 Marketplace path (normal)

```
maker                        taker
  publish-market-offer
                             take-market-offer
  accept-market-take
    -> creates the swap, generates S and H
    -> publishes `offer`
    -> publishes `market-match` (binds take -> swap_id)
                             accept-match / accept-market-match
                               -> binds the match, publishes `accept`
```

From here the settlement sequence is identical for both directions:

```
Alice  publish-blake-funded      Bob   receive-blake-funded
Alice  publish-offer             Bob   accept-offer
Bob    publish-btc-funded        Alice receive-btc-funded
Alice  publish-btc-claimed       Bob   receive-btc-claimed
Bob    extract-secret
Bob    publish-blake-claimed     Alice receive-blake-claimed
Alice  receive-btc-refunded  <-- Bob publish-btc-refunded   (refund path)
```

### 7.2 Direct path

The settlement messages can be exchanged without the marketplace layer
by running `new` → `publish-offer` → `accept-offer` directly. This is
how the FILE-transport test harness operates. In this path there is no
maker-signed `market-match` to authenticate first contact, so
`accept-offer` pins whoever authored the offer (see §9).

### 7.3 Reverse direction

When the market direction is `btc-for-blake`, the **maker is protocol
Bob** and the **taker is protocol Alice**. Secret generation therefore
moves to the taker:

- the maker's `accept-market-take` only reserves a `swap_id` and
  **MUST NOT** create `S`;
- the taker runs `accept-reverse-market-match`, which creates `S` and
  publishes the protocol `offer`;
- the maker imports that offer with `accept-reverse-swap-offer` under
  pinned identities and amounts.

Settlement is then identical. `next` will not surface the reverse import
until the taker's `offer` actually exists on the relay.

---

## 8. Automatic coordination boundaries

`next` is the live work queue. It is **non-economic**: it performs only
unambiguous signed coordination steps and never moves funds.

Automatic import of a market match is limited to a freshness window of
60 minutes by default (`BLAKE_SWAP_NEXT_FRESH_SECONDS`, minimum 60
seconds). Matches outside that window are reported as *historical* and
are not auto-imported; they remain valid and can be imported explicitly.

Offer expiry and automatic-action freshness are **separate concepts**.
An offer may be valid for its whole negotiated lifetime even though an
old match is no longer eligible for automatic import.

Initial BLAKE funding is **never** automatic. It requires an explicit
`fund` command, because it is the first irreversible economic
commitment.

---

## 9. Security properties and known limitations

### 9.1 Properties the implementation enforces

- **Atomicity.** Both HTLCs commit to the same `H`. Alice's Bitcoin
  claim reveals `S`, which is exactly what Bob needs for the BLAKE
  claim, and Bob's BLAKE claim is only possible after `S` is public.
- **Secret confidentiality.** `S` is never transported. It is generated
  by Alice, kept in local state, and revealed only on Bitcoin.
- **Timeout safety.** Bob independently validates the timeout budget
  from actual CLTV heights against his own policy; the protocol never
  compares cross-chain heights as if they were one number line.
- **Chain authority.** No message changes state without independent
  on-chain verification. Missing or ambiguous evidence fails closed.
- **Symbolic replay resistance.** A duplicate announcement of the same
  outpoint is a no-op; a conflicting announcement is a security failure.
- **Refusal to race.** If an HTLC outpoint has disappeared but no
  confirmed spender can be located, the implementation refuses to
  rebroadcast.
- **Role confusion resistance.** Local protocol role is derived from
  pinned identities; being pinned as both roles is a hard failure.
- **No settlement material in advertisements.** Market offers reject
  `secret_hash`, settlement public keys, and funding txids.

### 9.2 Known limitations

- **Public coordination data.** Nostr events are plaintext kind-1
  events. Amounts, timeouts, transaction IDs, and identities are
  publicly readable. No NIP-04/NIP-44 encryption is used.
- **Unauthenticated first contact.** `offer` is not author-pinned at the
  transport layer. The marketplace path pins the author indirectly via
  the maker-signed `market-match`; a bare manual `accept-offer` does
  not, and will accept the latest valid offer from any author.
- **No timestamp bounds on event verification.** Signature verification
  does not check `created_at` against local time. Future-dating is
  rejected only for market matches during automatic import.
- **No relay authentication.** NIP-42 `AUTH` challenges are reported and
  publication fails; there is no authenticated relay support.
- **Confirmation depth.** The implementation requires **at least one**
  confirmation before treating a spend as confirmed. The timeout policy
  *budgets* for two confirmations per claim but does not enforce that
  depth as a gate.
- **No descriptor recovery.** HTLC descriptors are not imported into the
  wallet as watch-only. Signing depends on the wallet still holding the
  keys that generated the addresses; a wallet restore or rename can
  make claim and refund impossible.
- **Unannounced BLAKE refund.** There is no `blake-refunded` message, so
  the counterparty cannot learn of Alice's BLAKE refund through the
  coordination layer.

---

## 10. Failure and recovery

The protocol is designed so that settlement can be reconstructed from
chain state alone.

### 10.1 Recovery anchors

Each participant records a **scan start height** for both chains before
any funding can legitimately occur. These are local recovery hints, not
counterparty-asserted facts. Bob records his own anchors on accept;
Alice records hers at swap creation and again on `refresh-timeouts`.

### 10.2 Observed chain states

Read-only observation classifies each HTLC as one of: `absent`,
`pending`, `funded`, `spent` (with branch), `pending-spend`, `invalid`,
`pending-or-ambiguous`, `unknown`, or not evaluated.

### 10.3 Recommended actions

Reconciliation produces exactly one recommended action from:

```
none · wait · manual-attention · sync-funding · sync-settlement
complete · claim-blake · refund-blake · fund-blake
claim-btc · fund-btc · refund-btc
```

mapped onto decision classes `settled`, `waiting`, `recovery-needed`,
`manual-attention`, and `actionable`. Anything ambiguous becomes
`manual-attention`; the implementation does not guess.

### 10.4 Crash safety

- A **funding intent** file is written before any funding broadcast and
  records the target address, amount, and `H`. The broadcast transaction
  ID and vout are persisted as soon as they are known.
- Swap state files are replaced atomically (temp file, `fsync`,
  `os.replace`).
- Funding operations are serialized per `(swap_id, chain)` with an
  OS-released `flock`, so a crash cannot leave a stale lock.
- On restart, the implementation rescans its own wallet for the intended
  payment rather than blindly re-sending.

### 10.5 Reorganization

A confirmed spend is only treated as authoritative while it is on the
active chain. If a final BLAKE claim or refund is disconnected, recovery
first checks whether the disconnected transaction is still in the
mempool. If it is, state is deliberately **not** rolled back. Only when
the transaction is genuinely gone and the BLAKE HTLC outpoint is
independently proven to have reappeared is state reverted.

---

## 11. Out of scope

This document does not specify:

- byte-level message formats (see `docs/WIRE_PROTOCOL.md`),
- operator setup and reproduction (see `docs/REGTEST_TWO_HOST.md`),
- fee selection and fee safety caps,
- any formal security proof or audit.

This remains experimental software. **Do not use with valuable funds.**
