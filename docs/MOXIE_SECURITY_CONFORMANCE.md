# BLAKE Swap — Moxie Security Conformance Checklist

**Reference implementation:** Python `v1.8-two-host`  
**Reference checkpoint:** `9b513c3`  
**Python protocol suite at checkpoint:** 97 passes / 0 failures

## Purpose

The Python implementation is the reference protocol model and security oracle.

The Moxie implementation does **not** need to reproduce Python's internal architecture, function boundaries, exception types, or error strings.

It must reproduce the same **security invariants and observable economic behavior**.

For every rejected adversarial input:

- durable protocol state must not advance;
- an attacker-controlled transaction identity must not replace an already trusted identity;
- no secret may be learned except from the authorized blockchain witness;
- no competing economic transaction may be created or broadcast when chain state is ambiguous;
- peer messages are coordination data, never settlement authority.

---

# 1. Fundamental role invariants

## C1 — Alice is the BLAKE funder and secret holder

**Invariant**

Protocol Alice:

- funds/sells BLAKE;
- creates secret `S`;
- knows `S` from swap creation.

Protocol Bob must not receive Alice's original secret through coordination transport.

**Adversarial scenario**

Bob receives protocol messages, retries, duplicates, funding announcements, or settlement announcements containing or attempting to encode `secret_hex`.

**Required behavior**

- reject the message;
- Bob's original-secret field remains empty;
- durable state does not advance because of the rejected message.

---

## C2 — Bob learns S only from the Bitcoin claim witness

**Invariant**

Bob learns `S` only by independently examining the confirmed Bitcoin claim transaction that spends the negotiated BTC HTLC.

**Adversarial scenario**

A peer announcement supplies a purported secret directly.

**Required behavior**

- reject transported secret material;
- do not persist it;
- do not permit BLAKE claim based solely on peer-provided secret data.

---

## C3 — Learned secret is knowledge, not settlement authority

**Invariant**

Once Bob legitimately learns `S`, knowledge may remain durable even after a reorg.

Possession of `S` alone does not authorize spending BLAKE.

**Adversarial scenario**

The Bitcoin claim revealing `S` is later reorged from the active chain.

**Required behavior**

- Bob may retain knowledge of `S`;
- any later BLAKE claim attempt must independently revalidate that the required Bitcoin claim is currently confirmed on the active chain;
- if that proof is absent, refuse the economic action.

---

# 2. State-machine invariants

## C4 — Only legal state transitions are permitted

Normal progression:

`OFFERED -> BLAKE_FUNDED -> BTC_FUNDED -> BTC_CLAIMED -> BLAKE_CLAIMED -> COMPLETE`

Refund progression:

`BLAKE_FUNDED -> BLAKE_REFUNDED`

or

`BTC_FUNDED -> BTC_REFUNDED -> BLAKE_REFUNDED`

`SECRET_REVEALED` may exist as transient processing state but must not become an unsupported durable authority state.

**Adversarial scenario**

Attempt to jump directly from an earlier state to a later economic state.

**Required behavior**

- reject transition;
- persistent state remains unchanged.

---

## C5 — Production peers cannot manually force state

**Invariant**

A command or API that directly advances state is test-only.

**Required behavior**

Production execution must not expose a mechanism allowing a participant to assert settlement state without chain proof.

---

# 3. Funding authority

## C6 — Funding announcements are not authority

**Invariant**

A received funding announcement must be independently proven against the blockchain.

**Proof must include**

- exact funding txid;
- exact vout;
- expected HTLC contract output;
- expected amount;
- confirmed transaction;
- active-chain block.

**Adversarial scenario**

Peer sends fabricated or conflicting funding metadata.

**Required behavior**

- reject;
- do not advance funding state;
- do not persist attacker-controlled funding anchors.

---

## C7 — Funding anchors must be structurally valid

Every durable funding txid and funding blockhash must be valid 32-byte hexadecimal values.

**Required behavior**

Malformed identity data must never enter durable state.

---

## C8 — Wrong protocol role cannot perform funding actions

**Adversarial scenario**

Bob attempts Alice-only BLAKE funding behavior, Alice attempts Bob-only BTC funding behavior, or the wrong side attempts to consume the other role's announcement.

**Required behavior**

- reject before economic mutation;
- state remains unchanged;
- no unintended transaction is created.

---

# 4. Settlement announcements are untrusted hints

## C9 — Every settlement must be independently verified

For an announced claim or refund, the receiver must independently establish that the transaction:

1. exists in the announced block;
2. is confirmed;
3. remains on the active chain;
4. spends this swap's **exact HTLC outpoint**;
5. uses the expected claim/refund branch.

Peer-supplied txid/blockhash values are lookup hints, not authority.

---

## C10 — Settlement txids and blockhashes must be structurally valid

Every durable settlement txid and blockhash must be valid 32-byte hex.

Malformed anchors must be rejected before persistence.

---

# 5. Settlement branch classification

## C11 — Claim branch must prove the correct preimage

A claim branch must:

- spend the exact HTLC input;
- select the claim branch;
- contain a preimage whose SHA-256 equals this swap's negotiated `secret_hash`.

Anything inconsistent is not a claim.

---

## C12 — Refund branch must prove the refund branch

A refund branch must:

- spend the exact HTLC input;
- select the refund branch;
- not masquerade as a claim carrying a matching preimage.

---

## C13 — Unknown or malformed settlement branch fails closed

**Adversarial scenario**

Confirmed HTLC spender has malformed witness data, unknown selector, inconsistent preimage/selector combination, or otherwise cannot be confidently classified.

**Required behavior**

- do not classify it as claim;
- do not classify it as refund;
- do not create a competing spend;
- require manual/recovery attention rather than guessing.

---

# 6. Replay and identity invariants

## C14 — Exact duplicate settlement announcement is idempotent

**Adversarial scenario**

The same authenticated claim/refund announcement arrives multiple times.

**Required behavior**

- independently reverify it against the active chain;
- accept as harmless duplicate;
- do not create another economic transaction;
- do not corrupt state.

---

## C15 — Conflicting BTC claim replay is rejected

After a BTC claim identity has been durably recorded, a different BTC claim txid/blockhash presented for that settlement must not replace it.

**Required behavior**

- reject;
- retain original durable claim identity;
- retain existing state.

---

## C16 — Conflicting BLAKE claim replay is rejected

Same requirement as C15 for BLAKE claim identity.

---

## C17 — Conflicting BTC refund replay is rejected

Same requirement as C15 for BTC refund identity.

---

# 7. Cross-swap binding

## C18 — Settlement message must match the exact swap ID

Changing `swap_id` to another swap must be rejected before durable settlement mutation.

---

## C19 — Settlement message must match the negotiated hashlock

Changing `secret_hash` must be rejected.

No settlement identity or secret knowledge may be persisted as a consequence.

---

## C20 — Genuine BTC claim from another swap is not authority

**Adversarial scenario**

Take a real, confirmed BTC claim from swap A.

Construct an otherwise internally valid settlement announcement for swap B using:

- B's valid `swap_id`;
- B's valid `secret_hash`;
- A's genuine claim txid/blockhash.

**Required behavior**

Reject because A's transaction does not spend B's exact BTC HTLC outpoint.

B remains `BTC_FUNDED`.

No BTC claim identity or revealed secret is persisted.

---

## C21 — Genuine BLAKE claim from another swap is not authority

Same attack as C20 using a real confirmed BLAKE claim from swap A.

**Required behavior**

Reject because it does not spend B's exact BLAKE HTLC.

B's Alice remains at the pre-BLAKE-claim state.

No BLAKE claim identity is persisted.

---

## C22 — Genuine BTC refund from another swap is not authority

Same attack using a real confirmed BTC refund from swap A.

**Required behavior**

Reject because it does not spend B's BTC HTLC.

B remains `BTC_FUNDED`.

No refund identity is persisted.

---

# 8. Transport invariants

## C23 — Secret material never travels over coordination transport

Settlement schema must prohibit `secret_hex`.

This must hold regardless of transport implementation.

Nostr, FILE transport, WebSocket, Tauri IPC, or future transports must not change this rule.

---

## C24 — Unknown settlement wire fields fail closed

The canonical settlement schema should reject fields outside the permitted protocol shape.

This prevents secret material or future engine-internal data from being smuggled under renamed/nested fields.

---

## C25 — Engine-internal metadata is not wire protocol data

Authenticated transport metadata such as verified sender identity may be attached locally after verification.

It must not become signed protocol payload merely because the engine uses it internally.

---

# 9. Participant identity

## C26 — Counterparty identity is pinned

Once Alice and Bob's participant identities are established, later protocol messages must come from the expected participant.

---

## C27 — Wrong signer cannot advance economic state

A valid-looking funding or settlement message signed by the wrong participant must be rejected.

State and durable economic identities remain unchanged.

---

## C28 — Authentication does not replace blockchain verification

A correctly signed message can still be false economically.

Therefore:

**valid signature + invalid chain fact = reject**

Never treat authenticated peer metadata as sufficient settlement proof.

---

# 10. Confirmation and pending-state safety

## C29 — Unconfirmed spend does not authorize settlement

A settlement transaction present only in the mempool must not advance durable settlement state.

---

## C30 — Unconfirmed spender prevents unsafe rebroadcast

If the HTLC appears spent but the spender cannot yet be safely classified/confirmed, the engine must not blindly rebroadcast a competing claim/refund.

Fail closed.

---

## C31 — Settlement becomes authoritative only after active-chain confirmation

At least one active-chain confirmation is required under the current reference protocol.

---

# 11. Reorg safety

## C32 — Stale BTC claim announcement is rejected

If the announced claim's confirming block has been invalidated before receipt:

- reject;
- do not advance state;
- do not persist its settlement identity as active-chain authority.

---

## C33 — Stale BLAKE claim announcement is rejected

Same rule for BLAKE claim.

---

## C34 — Stale BTC refund announcement is rejected

Same rule for BTC refund.

---

## C35 — Reorged settlement can roll durable state back conservatively

Supported rollback examples:

`BLAKE_CLAIMED -> BTC_CLAIMED`

`BLAKE_REFUNDED -> BTC_REFUNDED`

Rollback must be driven by loss of active-chain proof, not by peer assertion.

---

## C36 — Durable signed transaction may be reused after safe rollback

When the protocol has already durably constructed the correct signed settlement transaction, recovery may rebroadcast the same transaction after a reorg when chain conditions permit.

It must not create a conflicting economic transaction unnecessarily.

---

# 12. Crash/restart safety

## C37 — Signed settlement transaction survives crash

Once a settlement transaction is constructed for economic use, enough durable information must exist to recover safely after process or machine failure.

---

## C38 — Retry is idempotent

Restarting/retrying the same operation must not:

- spend the HTLC twice;
- create competing settlement transactions;
- replace trusted settlement identity;
- advance state based solely on local assumptions.

---

## C39 — Mempool loss does not cause transaction mutation

If a previously constructed signed transaction disappears from the mempool, recovery should reuse the durable transaction when it remains the correct economic action.

---

# 13. Timeout safety

## C40 — Negotiated timeout policy is mandatory

Offers must carry the agreed timeout-policy metadata.

Unknown policy versions fail closed.

---

## C41 — BTC timeout must leave sufficient BLAKE recovery window

A proposal whose BTC/BLAKE deadlines leave insufficient recovery time must be rejected.

---

## C42 — Expired or internally dishonest timeout metadata is rejected

Examples:

- already-expired BTC timeout;
- already-expired BLAKE timeout;
- one-block-short recovery gap;
- advertised timeout metadata inconsistent with actual contract heights.

---

## C43 — Early refund cannot bypass timelock

A correctly constructed refund attempted before its timeout must not settle.

State remains unchanged.

---

# 14. Recovery / reconciliation

## C44 — Chain truth repairs local state

Recovery commands may reconstruct missing funding or settlement state from independently observed blockchain facts.

---

## C45 — Recovery cannot invent authority

If chain observation is:

- ambiguous;
- malformed;
- unknown branch;
- possibly unconfirmed;
- inconsistent with recorded HTLC identity;

recovery must fail closed.

---

## C46 — Missing settlement identity anchors fail closed

A recovery result lacking necessary txid/blockhash identity cannot be treated as confirmed settlement authority.

---

# 15. Completion invariant

## C47 — COMPLETE requires final BLAKE claim proof

A swap must not become `COMPLETE` merely because both peers say settlement happened.

The final BLAKE claim must still be independently proven on the active chain.

---

# 16. Reference differential-testing rule

For each conformance case, the Python implementation and Moxie implementation should be driven from equivalent initial state and equivalent chain/transport stimuli.

Compare **observable protocol effects**, not implementation details.

For each case compare:

- resulting durable state;
- durable funding identities;
- durable settlement identities;
- secret knowledge;
- blockchain transactions created/broadcast;
- whether an action was accepted or rejected;
- whether retry changes the economic result.

Differences in:

- error text;
- exception classes;
- internal state-machine implementation;
- concurrency primitives;
- file layout;
- function names;

are not conformance failures unless they change protocol/security behavior.

---

# 17. Minimum Moxie handoff gate

Before treating Moxie as economically equivalent to the Python reference, require at minimum:

- all fundamental role/secret invariants;
- exact HTLC funding verification;
- exact HTLC settlement verification;
- claim/refund branch classification;
- unknown branch fail-closed behavior;
- signer pinning;
- duplicate idempotence;
- conflicting replay rejection;
- BTC/BLAKE cross-swap claim rejection;
- BTC cross-swap refund rejection;
- pending/unconfirmed safety;
- timeout-policy validation;
- reorg rollback/revalidation;
- crash/retry idempotence;
- final-chain proof before COMPLETE.

After that baseline passes, move to differential fuzzing and autonomous adversarial testing.

---

# 18. Next testing phase

Once both implementations satisfy this deterministic matrix:

1. Generate malformed and boundary wire messages automatically.
2. Generate randomized legal/illegal state-transition sequences.
3. Randomly interrupt processes between transaction construction, broadcast, confirmation, persistence, and message delivery.
4. Randomly invalidate/reconsider confirmation blocks.
5. Replay authentic messages across swaps and participants.
6. Compare Python and Moxie outcomes automatically.
7. Introduce two economically motivated adversarial agents only after deterministic conformance remains stable.

The goal is not for Moxie to behave like Python internally.

The goal is:

> **No peer message, retry, restart, reorg, malformed witness, wrong identity, or cross-swap transaction can cause the Moxie implementation to accept an economic fact that it cannot independently prove from the negotiated contract and active blockchain.**
