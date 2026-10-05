#!/usr/bin/env python3

from __future__ import annotations

import json
import os
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SRC = ROOT / "src"
TESTS = ROOT / "tests"

sys.path.insert(0, str(SRC))
sys.path.insert(0, str(TESTS))

import swap_protocol_tests as h  # noqa: E402

from swap_wire import (  # noqa: E402
    WireError,
    encode_message,
    validate_message,
)


PASS = 0
FAIL = 0


def ok(name):
    global PASS
    PASS += 1
    print("PASS ", name)


def fail(name, detail):
    global FAIL
    FAIL += 1
    print("FAIL ", name)
    print("      ", detail)


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def load_message(
    swap_id,
    message_type,
):
    path = h.message_file(
        swap_id,
        message_type,
    )

    require(
        path.exists(),
        f"missing message file: {path}",
    )

    data = json.loads(
        path.read_text()
    )

    #
    # FILE transport wraps the settlement message in a v2 envelope
    # carrying the sender identity. The wire validator is concerned only
    # with the settlement message itself, so unwrap it here exactly as
    # file_receive_message does.
    #
    if (
        isinstance(data, dict)
        and data.get("_file_transport_version") == 2
        and isinstance(data.get("message"), dict)
    ):
        return data["message"]

    return data


def validate_real(
    swap_id,
    message_type,
):
    message = load_message(
        swap_id,
        message_type,
    )

    validated = validate_message(
        message,
        expected_type=message_type,
    )

    canonical = encode_message(
        message,
        expected_type=message_type,
    )

    decoded = json.loads(
        canonical
    )

    require(
        decoded == validated,
        f"{message_type}: canonical round trip changed payload",
    )

    ok(
        f"real {message_type} accepted by independent wire validator"
    )


def make_policy_swap():
    p = h.swap_cmd(
        "new",
        "--blake",
        "0.1",
        "--sats",
        "25000",
        role="alice",
    )

    return h.parse_swap_id(
        p.stdout
    )


def main():
    h.preflight()
    ok("preflight")

    #
    # Generate one REAL v1.5 swap using the engine.
    #
    swap_id = make_policy_swap()

    alice = h.load_state(
        swap_id,
        "alice",
    )

    require(
        alice.get("timeout_policy_version")
        == "1.5",
        "engine did not create v1.5 policy swap",
    )

    ok("engine created real v1.5 swap")

    #
    # Negotiation.
    #
    h.swap_cmd(
        "publish-offer",
        swap_id,
        role="alice",
    )

    validate_real(
        swap_id,
        "offer",
    )

    h.swap_cmd(
        "accept-offer",
        swap_id,
        role="bob",
    )

    validate_real(
        swap_id,
        "accept",
    )

    h.swap_cmd(
        "receive-accept",
        swap_id,
        role="alice",
    )

    #
    # BLAKE funding announcement.
    #
    h.swap_cmd(
        "fund-blake",
        swap_id,
        role="alice",
    )

    h.swap_cmd(
        "publish-blake-funded",
        swap_id,
        role="alice",
    )

    validate_real(
        swap_id,
        "blake-funded",
    )

    h.swap_cmd(
        "receive-blake-funded",
        swap_id,
        role="bob",
    )

    #
    # BTC funding announcement.
    #
    h.swap_cmd(
        "fund-btc",
        swap_id,
        role="bob",
    )

    h.swap_cmd(
        "publish-btc-funded",
        swap_id,
        role="bob",
    )

    validate_real(
        swap_id,
        "btc-funded",
    )

    h.swap_cmd(
        "receive-btc-funded",
        swap_id,
        role="alice",
    )

    #
    # Alice claims BTC.
    #
    h.swap_cmd(
        "claim-btc",
        swap_id,
        role="alice",
    )

    h.swap_cmd(
        "publish-btc-claimed",
        swap_id,
        role="alice",
    )

    validate_real(
        swap_id,
        "btc-claimed",
    )

    h.swap_cmd(
        "receive-btc-claimed",
        swap_id,
        role="bob",
    )

    #
    # Bob learns S from Bitcoin, not wire.
    #
    bob_before = h.load_state(
        swap_id,
        "bob",
    )

    require(
        bob_before.get("secret_hex") is None,
        "Bob received Alice secret through protocol state",
    )

    h.swap_cmd(
        "extract-secret",
        swap_id,
        role="bob",
    )

    bob_after = h.load_state(
        swap_id,
        "bob",
    )

    require(
        bob_after.get("revealed_secret_hex"),
        "Bob failed to recover secret from Bitcoin",
    )

    ok(
        "real wire messages still transported no secret"
    )

    #
    # Bob claims BLAKE.
    #
    h.swap_cmd(
        "claim-blake",
        swap_id,
        role="bob",
    )

    h.swap_cmd(
        "publish-blake-claimed",
        swap_id,
        role="bob",
    )

    validate_real(
        swap_id,
        "blake-claimed",
    )

    h.swap_cmd(
        "receive-blake-claimed",
        swap_id,
        role="alice",
    )

    print()
    print("=" * 72)
    print("WIRE COMPATIBILITY SUMMARY")
    print("=" * 72)
    print("passes:  ", PASS)
    print("failures:", FAIL)
    print("swap:    ", swap_id)

    if FAIL:
        raise SystemExit(1)


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:
        fail(
            "wire compatibility",
            str(exc),
        )

        print()
        print("=" * 72)
        print("WIRE COMPATIBILITY SUMMARY")
        print("=" * 72)
        print("passes:  ", PASS)
        print("failures:", FAIL)

        raise
