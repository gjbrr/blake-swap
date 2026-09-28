# BTC ↔ BLAKE Atomic Swap Wire Protocol

**Status:** Experimental Draft 0.2
**Reference implementation:** `f61eeb285914216ec130fe78d1b8c635d3e28315`
**Canonical validator:** `src/swap_wire.py`
**Inventory tool:** `scripts/wire_inventory.py`

This document specifies the message formats used by the reference
BTC ↔ BLAKE atomic-swap implementation. It describes **implemented
experimental behavior**. It does not claim production readiness or
constitute a formal security proof.

The key words **MUST**, **MUST NOT**, **SHOULD**, **SHOULD NOT**, and
**MAY** describe protocol requirements.

---

## 1. Scope and protocol layers

The protocol separates four concerns:

1. **Marketplace discovery** — public offers on Nostr.
2. **Swap negotiation** — a private offer/accept exchange between two
   specific participants.
3. **Settlement notification** — announcements that a specific
   transaction was broadcast or confirmed.
4. **Authoritative blockchain verification** — the only source of
   economic truth.

A transport message is **not** proof that an economic action occurred.
Every settlement message identifies evidence that the recipient **MUST**
independently verify against Bitcoin or BLAKE before changing local
state. Section 8 states the verification obligations.

---

## 2. Identity model

Two identities are involved and they are deliberately independent:

- **Nostr identity** — a secp256k1 key used to sign coordination
  events. It is a participant's durable identity.
- **Chain keys** — per-swap compressed secp256k1 public keys embedded
  in the HTLC descriptors. They are obtained from the local wallet
  (`getnewaddress` → `getaddressinfo.pubkey`) and are therefore
  per-swap, not per-participant.

A participant's **Nostr identity is not its protocol role.** The same
Nostr identity can be protocol Alice in one swap and protocol Bob in
another. Protocol role is derived from the identities pinned into a
specific swap record (see `docs/PROTOCOL.md` §2).

---

## 3. Transport bindings

The implementation supports two transports selected by
`BLAKE_SWAP_TRANSPORT`. The engine default is `file`.

### 3.1 Common requirements

Every settlement message:

- MUST contain `type` and `swap_id`.
- MUST have `type` equal to the transport-level message type.
- MUST have `swap_id` equal to the transport-level swap ID.
- MUST contain exactly the fields its type declares, and no others.
- MUST NOT contain `secret_hex`, at any layer.

`validate_transport_message()` (`src/swap_v2.py`) enforces these on both
send and receive. It applies the canonical schema in `src/swap_wire.py`,
which means:

- an **unknown field is rejected**, so the `secret_hex` prohibition is
  enforced by the shape of the message rather than by one key name: a
  nested, renamed or extra field cannot carry it past validation;
- every required field, its type, and the amount precision rule are
  checked by the same code that independent implementations use;
- the transport's own bookkeeping (the authenticated sender identity,
  attached to a received message as `_nostr_pubkey`) is **not** wire
  content: it is tolerated on receipt and refused on send, so an engine
  detail cannot leak into a message.

A top-level `secret_hex` key is additionally rejected explicitly, with
`SECURITY FAILURE: refusing to transport secret_hex`, as defense in
depth.

> **Implementation note.** Producers **MUST NOT** place the secret
> anywhere in a message, under any name. The engine's own builders never
> do, and `tests/schema_equality_tests.py` fails if the engine's
> acceptance boundary and the schema ever stop agreeing.

### 3.2 FILE transport

Intended for local and regtest operation. It is **not** a
cryptographically authenticated transport.

- Messages are stored at
  `$BLAKE_SWAP_MESSAGE_DIR/<swap_id>.<message_type>.json`.
  The filename **is** the address; exactly one message per
  `(swap_id, message_type)` pair can exist, and sending overwrites it.
- The stored document is a v2 envelope:

  ```json
  {
    "_file_transport_version": 2,
    "sender_pubkey": "<64-hex x-only secp256k1 public key>",
    "message": { "...": "the settlement message" }
  }
  ```

- On receive, `sender_pubkey` MUST be a 64-character hex string. It is
  copied onto the message as the local metadata field `_nostr_pubkey`,
  mirroring the metadata attached after Nostr signature verification.
- A bare settlement message with no envelope (pre-v2 file) is accepted
  for backward compatibility.
- The sender identity in FILE mode is **unauthenticated** — it is
  asserted by the writing process, not proven. FILE mode still requires
  a configured Nostr key so that identity-binding code paths are
  exercised identically to Nostr mode.

### 3.3 Nostr transport

Settlement messages are published as **kind 1** events.

**Event construction** (all fields below are covered by the signature):

| Field | Value |
|---|---|
| `pubkey` | x-only public key, 32-byte lowercase hex (no `02`/`03` prefix) |
| `created_at` | Unix seconds |
| `kind` | `1` |
| `tags` | see below |
| `content` | canonical JSON of the settlement message |
| `id` | `sha256(JSON([0, pubkey, created_at, kind, tags, content]))` |
| `sig` | BIP-340 Schnorr signature over the 32-byte `id` |

`content` MUST be the canonical serialization

```python
json.dumps(message, separators=(",", ":"), sort_keys=True)
```

and the id preimage MUST be

```python
json.dumps([0, pubkey, created_at, kind, tags, content],
           separators=(",", ":"), ensure_ascii=False)
```

**Settlement tags:**

```
["t", "blake-swap"]
["d", "<swap_id>"]
["type", "<message-type>"]
```

**Subscription filter:**

```json
{"kinds": [1], "#d": ["<swap_id>"], "limit": 20}
```

with `"authors": ["<expected-pubkey>"]` added when the counterparty
identity is already pinned. The `type` tag is **not** part of the relay
filter; the type is re-checked locally from `content`.

**Retrieval rules.** The client queries all configured relays
concurrently, deduplicates by event id, and then:

1. MUST verify the event signature and recompute the event id.
2. MUST reject events whose `content.swap_id` differs from the
   requested swap ID.
3. MUST reject events whose `content.type` differs from the requested
   message type.
4. MUST reject events whose `pubkey` differs from the expected author
   when an author is pinned.
5. Selects the surviving event with the greatest `(created_at, id)`.

Relay-side `authors` filtering is an optimization only. The local
signature and field checks are the security boundary.

**Per-type author pinning.** After the handshake, incoming settlement
messages have an already-pinned expected author:

| Message type | Recipient role | Expected signer field |
|---|---|---|
| `blake-funded` | Bob | `swap.nostr_alice_pub` |
| `btc-funded` | Alice | `swap.nostr_bob_pub` |
| `btc-claimed` | Bob | `swap.nostr_alice_pub` |
| `blake-claimed` | Alice | `swap.nostr_bob_pub` |
| `btc-refunded` | Alice | `swap.nostr_bob_pub` |

`offer` and `accept` are **not** pinned at the transport layer; their
author is bound by command-level logic. See §8.2 for the consequence.

---

## 4. Settlement message types

There are seven settlement message types. Each carries a fixed signer
role.

### 4.1 `offer`

Sent by protocol **Alice**.

| Field | Type | Notes |
|---|---|---|
| `type` | string | `"offer"` |
| `swap_id` | string | 12 lowercase hex characters |
| `blake_amount` | decimal string | Alice sells this much BLAKE; > 0 |
| `btc_sats` | integer | Bob pays this many satoshis; > 0 |
| `secret_hash` | hex string | 32-byte SHA256 commitment `H` |
| `btc_timeout` | integer | absolute Bitcoin CLTV height; > 0 |
| `blake_timeout` | integer | absolute BLAKE CLTV height; > 0 |
| `created_at` | integer | Unix seconds; ≥ 0 |
| `blake_alice_pub` | pubkey | compressed secp256k1 (`02`/`03` + 32 bytes) |
| `btc_alice_pub` | pubkey | compressed secp256k1 |
| `timeout_policy_version` | string | MUST be `"1.5"` |
| `btc_timeout_window_seconds` | integer | > 0 |
| `blake_timeout_window_seconds` | integer | > 0 |
| `bob_recovery_seconds` | integer | > 0 |

The four timeout-policy fields are **mandatory**. There is no legacy
fallback: `validate_v15_offer_timeouts()` raises
`SECURITY FAILURE: timeout policy version is required` if
`timeout_policy_version` is absent.

Known fields only. Unknown fields are rejected by the canonical
validator.

`blake_amount` is a JSON **string** containing a decimal value with at
most **8 decimal places**, which is the protocol's smallest unit (BLAKE is
a Bitcoin fork, so one base unit is `0.00000001` BLAKE). It is rendered
from an integer base-unit amount, so an integer amount appears as `"10"`,
not `"10.0"`, and an amount can never be transmitted in a form the chain
cannot represent. A value finer than one base unit is rejected rather than
rounded, at both ends: the engine refuses to construct it, and the
canonical validator refuses to accept it.

The engine stores, compares and transmits base units; the whole-coin
decimal is derived for the wire and for display. Earlier builds stored a
Python float in the swap record and compared amounts with a `1e-8`
tolerance, which accepted an output exactly one base unit away from the
agreed amount. Records written by those builds are migrated on load: an
amount with an exact base-unit form is converted exactly, and one without
(it was written from a float) is rounded to the nearest base unit *and the
rounding is reported* rather than applied silently.

### 4.2 `accept`

Sent by protocol **Bob**.

| Field | Type | Notes |
|---|---|---|
| `type` | string | `"accept"` |
| `swap_id` | string | 12 hex characters |
| `secret_hash` | hex string | MUST equal the offer's `H` |
| `blake_bob_pub` | pubkey | compressed secp256k1 |
| `btc_bob_pub` | pubkey | compressed secp256k1 |

Bob **MUST NOT** possess or transmit the secret at this point. The
reference implementation asserts this locally and prints
`PASS: Bob does NOT have secret S`.

### 4.3 `blake-funded`

Sent by protocol **Alice**.

| Field | Type | Notes |
|---|---|---|
| `type` | string | `"blake-funded"` |
| `swap_id` | string | 12 hex characters |
| `secret_hash` | hex string | MUST equal `H` |
| `blake_amount` | decimal string | MUST equal the offer's amount |
| `blake_timeout` | integer | MUST equal the offer's BLAKE timeout |
| `blake_funding_txid` | hex string | 32-byte txid |
| `blake_funding_vout` | integer | ≥ 0 |

### 4.4 `btc-funded`

Sent by protocol **Bob**.

| Field | Type | Notes |
|---|---|---|
| `type` | string | `"btc-funded"` |
| `swap_id` | string | 12 hex characters |
| `secret_hash` | hex string | MUST equal `H` |
| `btc_sats` | integer | MUST equal the offer's amount |
| `btc_timeout` | integer | MUST equal the offer's BTC timeout |
| `btc_funding_txid` | hex string | 32-byte txid |
| `btc_funding_vout` | integer | ≥ 0 |

### 4.5 `btc-claimed`

Sent by protocol **Alice**. Publication of this message is a
notification; the authoritative evidence is the confirmed transaction.

| Field | Type | Notes |
|---|---|---|
| `type` | string | `"btc-claimed"` |
| `swap_id` | string | 12 hex characters |
| `secret_hash` | hex string | MUST equal `H` |
| `btc_claim_txid` | hex string | 32-byte txid |
| `btc_claim_blockhash` | hex string | 32-byte block hash |

This message **MUST NOT** contain the secret. Bob recovers the secret
from the confirmed Bitcoin transaction itself (§8.3).

### 4.6 `blake-claimed`

Sent by protocol **Bob**.

| Field | Type | Notes |
|---|---|---|
| `type` | string | `"blake-claimed"` |
| `swap_id` | string | 12 hex characters |
| `secret_hash` | hex string | MUST equal `H` |
| `blake_claim_txid` | hex string | 32-byte txid |
| `blake_claim_blockhash` | hex string | 32-byte block hash |

### 4.7 `btc-refunded`

Sent by protocol **Bob**.

| Field | Type | Notes |
|---|---|---|
| `type` | string | `"btc-refunded"` |
| `swap_id` | string | 12 hex characters |
| `secret_hash` | hex string | MUST equal `H` |
| `btc_refund_txid` | hex string | 32-byte txid |
| `btc_refund_blockhash` | hex string | 32-byte block hash |

### 4.8 Absence of `blake-refunded`

There is deliberately **no** `blake-refunded` message type. Alice's
BLAKE refund is a unilateral on-chain action that Bob does not need to
be notified of. The local `BLAKE_REFUNDED` state is terminal for
Alice's record and is not announced over the coordination layer.

---

## 5. Marketplace message types

Marketplace messages are Nostr kind-1 events whose `content` carries a
market message. They are identified by the `t` tag, not by `d`.

The economic pair is canonical: `base` is always `"BLAKE"` and `quote`
is always `"BTC"`.

### 5.1 Direction and derived roles

| `direction` | Market meaning | Maker role | Taker role |
|---|---|---|---|
| `blake-for-btc` | maker gives BLAKE, wants BTC | `alice` | `bob` |
| `btc-for-blake` | maker gives BTC, wants BLAKE | `bob` | `alice` |

`maker_role` and `taker_role` are **derived** from `direction`. A
message that states roles inconsistent with its direction is invalid.

### 5.2 `market-offer`

Tags: `["t","blake-swap-market"]`, `["base","BLAKE"]`,
`["quote","BTC"]`, and for v2 `["direction",<direction>]`.

**Version 2 (current):**

| Field | Type | Notes |
|---|---|---|
| `type` | string | `"market-offer"` |
| `version` | integer | `2` |
| `base` | string | `"BLAKE"` |
| `quote` | string | `"BTC"` |
| `direction` | string | one of the two directions |
| `blake_amount` | decimal string | > 0 |
| `btc_sats` | integer | > 0 |
| `created_at` | integer | MUST equal the event's `created_at` |
| `expires_at` | integer | MUST be > `created_at` |
| `nonce` | hex string | 8 random bytes, 16 hex chars |

**Version 1 (legacy, parsed but no longer produced):** identical except
it carries `side: "sell"` instead of `direction`, has no `direction`
tag, and implies `blake-for-btc`.

**Forbidden fields.** A market offer MUST NOT contain any of
`secret_hex`, `secret_hash`, `blake_alice_pub`, `blake_bob_pub`,
`btc_alice_pub`, `btc_bob_pub`, `blake_funding_txid`, or
`btc_funding_txid`. A market advertisement carries no settlement
material.

**Expiry boundary.** At `created_at >= expires_at` the offer is
`EXPIRED`. A market match is valid only if
`match.created_at < offer.expires_at`.

### 5.3 `market-take`

Tags: `["t","blake-swap-market-take"]`, `["e",<offer_id>]`,
`["p"]`→maker, and for v2 `["direction",<direction>]`.

| Field | Type | v1 | v2 |
|---|---|---|---|
| `type` | string | `"market-take"` | `"market-take"` |
| `version` | integer | `1` | `2` |
| `offer_id` | hex string | 64-hex event id | same |
| `maker` | hex string | offer author pubkey | same |
| `taker` | hex string | — | MUST equal the event `pubkey` |
| `base` / `quote` | string | `"BLAKE"` / `"BTC"` | same |
| `direction` | string | — | MUST equal the offer's direction |
| `maker_role` | string | — | MUST equal the derived role |
| `taker_role` | string | — | MUST equal the derived role |
| `blake_amount` | decimal string | MUST equal the offer's | same |
| `btc_sats` | integer | MUST equal the offer's | same |
| `created_at` | integer | MUST equal the event's | same |
| `nonce` | hex string | 16 hex chars | same |

**Version lockstep.** A take's `version` MUST equal the referenced
offer's `version`. A v2 offer MUST NOT be answered with a v1 take.

### 5.4 `market-match`

Tags: `["t","blake-swap-market-match"]`, `["e",<offer_id>]`,
`["e",<take_id>]`, `["p",<taker>]`, `["d",<swap_id>]`, and for v2
`["direction",<direction>]`.

| Field | Type | v1 | v2 |
|---|---|---|---|
| `type` | string | `"market-match"` | `"market-match"` |
| `version` | integer | `1` | `2` |
| `offer_id` | hex string | 64-hex | same |
| `take_id` | hex string | 64-hex | same |
| `swap_id` | hex string | 12-hex | same |
| `maker` | hex string | offer author | same |
| `taker` | hex string | take taker | same |
| `base` / `quote` | string | `"BLAKE"` / `"BTC"` | same |
| `direction` | string | — | offer direction |
| `maker_role` / `taker_role` | string | — | derived roles |
| `blake_amount` | decimal string | MUST equal the offer's | same |
| `btc_sats` | integer | MUST equal the offer's | same |
| `created_at` | integer | event `created_at` | same |

A market match **MUST** be signed by the original offer maker. It binds
a specific take to a specific atomic-swap ID.

**Temporal rules.** A match is invalid if
`match.created_at < take.created_at` (it would predate the take it
selects) or if `match.created_at >= offer.expires_at`.

### 5.5 `market-cancel`

Tags: `["t","blake-swap-market-cancel"]`, `["e",<offer_id>]`.

| Field | Type | Notes |
|---|---|---|
| `type` | string | `"market-cancel"` |
| `version` | integer | `1` |
| `offer_id` | hex string | the offer being cancelled |
| `created_at` | integer | MUST equal the event's `created_at` |

A cancellation is valid only if the event `pubkey` is the original
offer maker. **Only the maker can cancel.**

### 5.6 Derived market state

An offer's state is derived from the offer event plus the set of
candidate market events:

- **`CANCELLED`** — a valid `market-cancel` for this offer exists.
- **`MATCHED`** — a valid `market-match` for this offer exists, the
  referenced take event is present in the candidate set, and full
  evidence validation passes.
- **`EXPIRED`** — otherwise, if `expires_at <= now`.
- **`OPEN`** — otherwise.

Any event with `created_at >= expires_at` is **ignored** as a terminal
event: an offer that had already expired cannot be cancelled or matched
retroactively. `MATCHED` and `CANCELLED` are both terminal; if two
terminal events are valid, the earliest by `(created_at, id)` wins.

---

## 6. Canonical validation library

`src/swap_wire.py` is a dependency-free reference implementation of the
settlement-message schema. It deliberately knows nothing about wallets,
RPC, relays, local state, or the protocol state machine. It provides:

- `SETTLEMENT_TYPES` — the seven settlement types.
- `SCHEMAS` — required and optional field sets per type.
- `validate_message(message, *, expected_type=None)`
- `decode_message(raw, *, expected_type=None)` — rejects duplicate JSON
  object keys.
- `encode_message(message, *, expected_type=None)` — canonical JSON.
- `sha256_hex(data)`

`src/swap_v2.py` **does not import** `src/swap_wire.py`; it embeds
equivalent per-command checks. `tests/swap_wire_compat_tests.py`
round-trips every engine-produced message through `swap_wire` to detect
drift between the two. `scripts/wire_inventory.py` re-derives the
message inventory directly from the engine source with `ast`.

Independent producers that emit messages using only `swap_wire.py` live
in `tools/minimal_*.py`:

| Tool | Emits |
|---|---|
| `minimal_alice_offer.py` | `offer` |
| `minimal_bob_accept.py` | `accept` |
| `minimal_blake_funded.py` | `blake-funded` |
| `minimal_btc_funded.py` | `btc-funded` |
| `minimal_btc_claimed.py` | `btc-claimed` |
| `minimal_blake_claimed.py` | `blake-claimed` |
| `minimal_wire_write.py` | any type (generic) |

---

## 7. Versioning

- Settlement messages carry no version field. Their schema is fixed by
  `type`. The v1.5 label refers to the **timeout policy**, not to the
  settlement message schema.
- Market messages carry an integer `version`. Versions 1 and 2 are
  accepted on parse. Current code **produces version 2 only**. A take's
  version MUST equal its offer's version.
- An offer's `timeout_policy_version` MUST be `"1.5"`. Any other value
  is rejected.

---

## 8. Verification obligations

Receiving a message is never sufficient. Each settlement message
imposes an independent on-chain verification duty.

### 8.1 Funding announcements

- `blake-funded` (received by Bob): Bob MUST re-derive the expected BLAKE
  HTLC descriptor and address from `H`, the settlement pubkeys and the
  timeout, and MUST confirm that the announced transaction pays that
  exact address the exact amount with at least one confirmation. Bob
  MUST also confirm the authenticated signer is Alice.
- `btc-funded` (received by Alice): Alice MUST perform the equivalent
  checks against the Bitcoin HTLC. Alice MUST also confirm the
  authenticated signer is Bob.

A duplicate announcement of the **same** outpoint is a no-op. A
conflicting announcement is a security failure.

### 8.2 Offer and acceptance

- `offer` (received by Bob): Bob MUST resolve the offer's author to a
  Nostr identity and pin it. This is the step that closes the
  unauthenticated first-contact window. Bob MUST independently validate
  the advertised timeout policy against **his own** chain tips and
  **his own** `TimeoutPolicy`; the signed metadata is authenticated
  evidence but is not the authority. Bob MUST NOT create settlement keys
  or accept until that validation passes.
- `accept` (received by Alice): Alice MUST confirm the signer is the Bob
  identity pinned in the swap, and MUST confirm that the announced
  `blake_bob_pub` and `btc_bob_pub` are present.

> **Security note — first contact.** `offer` is not author-pinned at the
> transport layer. The marketplace path supplies
> `expected_alice_pub` (derived from a maker-signed `market-match`), so
> the low-level `accept-offer` command pins the author. An invocation
> that does **not** supply `expected_alice_pub` accepts the latest valid
> signed offer from any author and pins that author as counterparty.
> See `docs/PROTOCOL.md` §9.

### 8.3 Settlement announcements

- `btc-claimed` (received by Bob): the message only tells Bob to look.
  Bob MUST locate the confirmed Bitcoin transaction, verify that it
  spends the exact Bitcoin HTLC outpoint, verify inclusion in the
  announced block on the active chain, and **recover the secret `S`**
  from the transaction witness by finding a 32-byte value whose SHA256
  equals `H`. Bob MUST verify the recovered secret against `H` before
  using it.
- `blake-claimed` (received by Alice): Alice MUST verify the confirmed
  BLAKE transaction spends the exact BLAKE HTLC and is on the active
  chain.
- `btc-refunded` (received by Alice): Alice MUST verify the confirmed
  Bitcoin refund spends the exact Bitcoin HTLC outbox by the refund
  branch, on the active chain. Only then may she proceed to refund
  BLAKE.

A transaction is treated as spent only when it has at least one
confirmation, a known block hash, and is not marked as off the active
chain. If the HTLC outpoint is missing but no confirmed spender can be
found, the implementation **refuses to rebroadcast** rather than risk
racing an unknown spend.

### 8.4 The secret

The secret `S`:

- MUST be generated by Alice.
- MUST never appear in any coordination message.
- MUST be revealed only by Alice's confirmed Bitcoin claim.
- MUST be recovered by Bob only from the confirmed Bitcoin
  transaction witness.

---

## 9. Message flow summary

```
Alice                                Bob
  |  offer  ------------------------>
  |  <------------------------  accept
  |  blake-funded  ---------------->      (BLAKE HTLC funded)
  |  <---------------------  btc-funded   (BTC HTLC funded)
  |  btc-claimed  ---------------->       (S revealed on Bitcoin)
  |  <-------------------  blake-claimed  (BLAKE HTLC claimed)
```

Refund path:

```
  |  <-------------------  btc-refunded   (Bob refunds Bitcoin)
  |  (Alice refunds BLAKE locally; no message)
```

---

## 10. Out of scope

This document does not specify:

- the HTLC scripts, timeouts, or state machine (see `docs/PROTOCOL.md`),
- operator procedures and configuration (see
  `docs/REGTEST_TWO_HOST.md`),
- relay availability, retention, or ordering guarantees,
- any formal security proof.

This remains experimental software. **Do not use with valuable funds.**
