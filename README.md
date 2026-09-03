# BTC ↔ BLAKE Atomic Swap Prototype

Experimental peer-to-peer atomic swaps between Bitcoin and the BLAKE
Bitcoin fork using Bitcoin-style HTLCs.

**Status:** protocol-hardened regtest prototype (v1.5)

**Do not use with valuable funds.**

## What this project demonstrates

The prototype implements and tests a BTC ↔ BLAKE atomic swap in which:

1. Alice offers BLAKE in exchange for BTC.
2. Alice chooses a secret `S` and publishes `H = SHA256(S)`.
3. Alice locks BLAKE in an HTLC.
4. Bob independently verifies the BLAKE contract.
5. Bob locks BTC in an HTLC using the same hash.
6. Alice claims the BTC using `S`.
7. Bitcoin reveals `S` in the claim witness.
8. Bob independently discovers `S` from Bitcoin.
9. Bob verifies `SHA256(S) == H`.
10. Bob claims the BLAKE.

If settlement does not complete, the two contracts provide timeout refund
paths.

The secret is never transported through Nostr or the protocol messaging
layer.

## Design model

> Relays provide availability. Keys provide identity. Blockchains provide
> settlement. Each participant provides their own judgment of safety.

Nostr is used for discovery, identity, and coordination.

Bitcoin and BLAKE are authoritative for economic state.

After negotiation, protocol messages are conveniences rather than a
requirement for settlement liveness: participants can reconstruct funding
and settlement state from their own nodes.

## Current architecture

```text
                  Nostr relays
               discovery / identity
                       |
          +------------+------------+
          |                         |
       Alice agent                Bob agent
          |                         |
          +-------- protocol -------+
          |                         |
     BLAKE node                Bitcoin node
          \                         /
           \---- atomic HTLCs -----/








'
eof
OEF
