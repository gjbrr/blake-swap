# Moxie Adapter Handoff

## Objective

Implement one executable that speaks:

`blake-swap-adapter-v1`

over stdin/stdout JSON.

The Python security harness remains responsible for:

- adversarial scheduling
- Bitcoin regtest block production
- BLAKE regtest block production
- replay ordering
- crash/reorg orchestration
- portable differential comparison

Moxie remains responsible for its own protocol behavior and durable state.

## Process model

The simplest supported model is:

1. Harness starts adapter executable.
2. Harness writes one JSON object to stdin.
3. Adapter performs exactly one request.
4. Adapter writes exactly one JSON object to stdout.
5. Adapter exits 0.

A persistent daemon may be added later, but is not required.

## Capabilities

Request:

    {"op":"capabilities"}

Minimum response:

    {
      "ok": true,
      "protocol": "blake-swap-adapter-v1",
      "operations": [
        "capabilities",
        "command",
        "state"
      ],
      "roles": [
        "alice",
        "bob"
      ]
    }

## Command

Example:

    {
      "op": "command",
      "role": "bob",
      "args": [
        "accept-offer",
        "abcdef123456"
      ]
    }

Success:

    {
      "ok": true,
      "returncode": 0,
      "stdout": "",
      "stderr": ""
    }

Refusal:

    {
      "ok": false,
      "returncode": 1,
      "stdout": "",
      "stderr": "implementation-defined text"
    }

The test suite does not require Moxie error strings to match Python.

## Durable state

Request:

    {
      "op": "state",
      "role": "bob",
      "swap_id": "abcdef123456"
    }

Response:

    {
      "ok": true,
      "state": {
        "...": "..."
      }
    }

The seeded corpus currently observes:

- state
- secret_hex
- revealed_secret_hex
- blake_funding_txid
- blake_funding_vout
- btc_funding_txid
- btc_funding_vout
- btc_claim_txid
- btc_refund_txid
- blake_claim_txid
- blake_refund_txid

Moxie may store its internal state differently. The adapter simply maps
its durable representation into this observable shape.

## Secret rule

Bob must never obtain Alice's original `secret_hex`.

After a valid Bitcoin claim, Bob may expose `revealed_secret_hex` only
after learning the preimage from the Bitcoin witness.

The adapter must not leak S merely to make a test pass.

## First cross-language milestone

The first target is deliberately narrow:

    Python Alice
        versus
    Moxie Bob

using:

    seed 4

Expected portable result:

    path: claim
    digest prefix: e22e03aa7289b52e

This means:

- BLAKE funding succeeds and Bob verifies it
- Bob funds BTC
- duplicate funding messages remain safe
- Alice claims BTC
- Bob receives the claim
- Bob extracts S from Bitcoin witness
- Bob claims BLAKE
- both sides converge to COMPLETE
- Alice knows original S
- Bob does not know original S
- Bob knows witness-recovered S
- claim identities agree
- no refund branch exists

Concrete txids do not need to match Python.

## Later milestones

After Python Alice / Moxie Bob:

1. Moxie Alice / Python Bob
2. Moxie Alice / Moxie Bob
3. all seeded corpus cases
4. crash matrix
5. reorg matrix
6. concurrent settlement races

The security contract remains `docs/MOXIE_SECURITY_CONFORMANCE.md`.
