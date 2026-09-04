# BTC ↔ BLAKE Atomic Swap Wire Protocol

**Status:** Experimental Draft 0.1  
**Reference implementation:** `v1.5-regtest`

This document specifies the message formats used by the reference
BTC ↔ BLAKE atomic-swap implementation.

The key words **MUST**, **MUST NOT**, **SHOULD**, **SHOULD NOT**, and **MAY**
describe protocol requirements.

This document describes implemented experimental behavior. It does not
claim production readiness or constitute a formal security proof.

---

## 1. Protocol Layers

The protocol separates:

1. marketplace discovery,
2. swap negotiation,
3. settlement notification,
4. authoritative blockchain verification.

A transport message is not itself proof that an economic action occurred.

Settlement messages identify evidence that the recipient MUST independently
verify against Bitcoin or BLAKE.

---

## 2. Common Swap Message Requirements

Swap messages contain:

```json
{
  "type": "<message-type>",
  "swap_id": "<swap-id>"
}
