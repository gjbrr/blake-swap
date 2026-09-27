# Contributor Orientation

This project currently has two repositories:

- `blake-swap` — canonical protocol engine and authoritative source for swap logic
- `blake-swap-startos` — StartOS packaging and integration layer

## Canonical source of truth

Protocol behavior lives in `blake-swap`.

The directory:

`blake-swap-startos/upstream-project`

is a vendored copy of selected files from the canonical repository. It should not be treated as an independent implementation and should not be edited independently.

Changes to protocol logic should be made in `blake-swap`, tested there, then synchronized into the StartOS repository.

## Protocol roles

Protocol roles are **Alice** and **Bob**.

They are not equivalent to maker/taker.

For the current protocol:

- Alice sells/funds XBT on Bitcoin Blake2b
- Bob pays/funds BTC
- maker/taker depends on offer direction
- protocol role must be derived from protocol identity and negotiated state, not assumed from who created the offer

Both swap directions must preserve the same Alice/Bob economic roles.

## Core invariants

The following should be treated as protocol invariants:

- Alice is the only party that initially creates secret `S`
- Bob must not receive Alice's original secret through the transport layer
- Bob learns `S` from Bitcoin settlement evidence
- reconciliation is based on observed chain truth
- reconciliation itself is read-only
- initial XBT/BLAKE funding is explicit-only and must not happen automatically
- post-funding settlement and recovery may run automatically when preconditions are proven
- Nostr is used for negotiation and market/discovery transport
- settlement must remain recoverable from chain state even if settlement messages are missing
- unsafe or ambiguous observations must fail closed rather than guess
- retries and restarts must not create duplicate or competing economic transactions

## Important source files

Start with:

- `src/swap_v2.py`
  - main state machine
  - orchestration
  - settlement
  - recovery
  - reconciliation
  - Nostr coordination

- `src/swap_wire.py`
  - wire/message validation
  - required message fields
  - version compatibility

- `src/swap_timeout_policy.py`
  - timeout negotiation and safety policy

- `src/fee_policy.py`
  - dynamic settlement fee policy

The reconciliation command itself lives in `src/swap_v2.py`
(`command_reconcile`, `build_reconcile_report`, `_reconcile_*`).
`tools/historical/` holds spent one-shot source-patching scripts from the
development of that layer; they are not runtime code and must not be run.

The `scripts/` directory contains operator-facing entry points.

## Tests to read first

The tests are an important part of the current protocol definition.

Start with:

- `tests/swap_timeout_policy_tests.py`
- `tests/swap_timeout_negotiation_tests.py`
- `tests/swap_wire_tests.py`
- `tests/swap_wire_compat_tests.py`
- `tests/swap_chain_autonomy_tests.py`
- `tests/swap_settlement_recovery_tests.py`
- `tests/production_funding_pending_tests.py`
- `tests/pre_btc_refund_tests.py`
- `tests/market_state_tests.py`
- `tests/swap_reorg_safety_tests.py`
- `tests/swap_model_timeout_invariant_tests.py`

Then review the `swap_cross_impl_*` tests, which are intended to exercise implementation boundaries.

## Independent implementation goal

One goal of this review is an independent Go implementation of the protocol.

It does not need to mirror the Python code structure.

A headless CLI or daemon is sufficient initially. GUI work is not a priority.

The important interoperability target is:

- Python Alice ↔ Go Bob
- Go Alice ↔ Python Bob

Both directions should complete successfully.

Recovery behavior should also interoperate across implementations, including cases involving:

- missing Nostr settlement messages
- process restart
- pending transactions
- confirmed transactions not yet synchronized into local state
- claim/refund recovery from chain evidence

## Protocol gaps

If the Go implementation needs behavior that is not clearly specified by the protocol documentation or tests, treat that as a protocol-specification gap rather than silently copying Python behavior.

Please surface those gaps explicitly.

The goal is for protocol behavior to be explainable independently of one implementation.

## Suggested review order

A practical sequence is:

1. Run the existing Python implementation and test suite
2. Read `docs/PROTOCOL.md`
3. Read `docs/WIRE_PROTOCOL.md`
4. Review the state machine in `src/swap_v2.py`
5. Review wire validation and timeout policy
6. Review recovery/reconciliation behavior
7. Review the cross-implementation tests
8. Identify ambiguities or underspecified behavior
9. Begin the independent Go implementation

## StartOS repository

The StartOS repository is primarily useful for understanding:

- packaging
- StartOS actions
- RPC adapters
- persistent state layout
- Bitcoin/XBT service integration
- operator UX

It should not be used as a second source of protocol truth.

When in doubt, compare `upstream-project` against the canonical `blake-swap` repository.
