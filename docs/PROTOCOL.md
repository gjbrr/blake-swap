# BTC ↔ BLAKE Atomic Swap Protocol

**Status:** Experimental Draft 0.2  
**Reference implementation checkpoint:** `v1.5-regtest`

This document specifies the protocol behavior implemented and tested by the
BTC ↔ BLAKE atomic-swap prototype.

The key words **MUST**, **MUST NOT**, **SHOULD**, **SHOULD NOT**, and **MAY**
describe protocol requirements.

This specification describes experimental software. It is not a claim of
production readiness or a formal security proof.

---

## 1. Scope

The protocol enables two mutually distrustful participants to exchange BTC
and BLAKE without requiring a trusted custodian to control both assets.

The protocol separates three functions:

- discovery and coordination,
- participant identity,
- economic settlement.

Nostr may provide discovery and coordination.

Cryptographic signing keys provide participant identity.

Bitcoin and BLAKE provide authoritative settlement state.

After negotiation, the blockchains MUST contain sufficient economic evidence
for participants to recover settlement progress without depending upon
continued delivery of protocol messages.

---

## 2. Roles

### 2.1 Alice

Alice sells BLAKE and buys BTC.

Alice:

1. generates the swap secret,
2. commits to its hash,
3. funds the BLAKE HTLC first,
4. verifies Bob's BTC funding,
5. claims BTC by revealing the secret,
6. refunds BLAKE if the swap fails before Bob funds BTC.

### 2.2 Bob

Bob sells BTC and buys BLAKE.

Bob:

1. receives the hash commitment but MUST NOT receive Alice's secret during
   negotiation,
2. verifies Alice's BLAKE funding,
3. funds the BTC HTLC,
4. observes Alice's BTC claim,
5. extracts the secret from Bitcoin,
6. verifies the extracted secret,
7. claims BLAKE,
8. refunds BTC if Alice does not claim it before the BTC timeout.

---

## 3. Cryptographic Commitment

Alice generates a secret value:

```text
S


