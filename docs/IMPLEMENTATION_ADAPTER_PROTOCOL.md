# Atomic Swap Implementation Adapter

The security corpus tests protocol behavior rather than Python internals.

## Participant boundary

Alice and Bob are separate implementation adapters.

Each participant exposes:

- `command(...)`
- `state(swap_id)`
- `assert_state(swap_id, expected)`

The participant's protocol role is fixed when the adapter is created.
A participant command therefore does not accept a second role argument.

## Pair composition

A scenario is run with an independently supplied Alice adapter and Bob
adapter.

This permits:

| Alice | Bob |
|---|---|
| Python | Python |
| Python | Moxie |
| Moxie | Python |
| Moxie | Moxie |

The adversarial scheduler must not need different scenario logic for any
of these combinations.

## Explicit two-party orchestration

Handshake is:

1. Alice publishes offer.
2. Bob accepts offer.
3. Alice receives acceptance.

BLAKE funding is:

1. Alice funds BLAKE.
2. Alice publishes public funding identity.
3. Bob receives and independently validates funding.
4. Duplicate receipt is safe.

BTC funding is:

1. Bob funds Bitcoin.
2. Bob publishes public funding identity.
3. Alice receives and independently validates funding.
4. Duplicate receipt is safe.

No helper may transfer private secret S between implementations.

## Harness-owned services

The security laboratory, not either participant implementation, controls:

- Bitcoin regtest block production
- BLAKE regtest block production
- shared transport inspection
- adversarial scheduling
- crash injection
- reorganization injection
- final differential comparison

## Portable comparison

Implementations need not match:

- internal function names
- exception wording
- state storage format
- lock mechanism
- process architecture
- generated transaction IDs
- generated output indexes

They must match portable security and economic behavior.
