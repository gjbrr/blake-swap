#!/usr/bin/env python3

from __future__ import annotations

import json
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(
    0,
    str(ROOT / "src"),
)

from swap_wire import (  # noqa: E402
    WireError,
    decode_message,
    encode_message,
    validate_message,
)


PASS_COUNT = 0
FAIL_COUNT = 0


def ok(name):
    global PASS_COUNT
    PASS_COUNT += 1
    print("PASS ", name)


def fail(name):
    global FAIL_COUNT
    FAIL_COUNT += 1
    print("FAIL ", name)


def expect_reject(name, message):
    try:
        validate_message(message)
    except WireError:
        ok(name)
        return

    fail(name)


def main():
    offer = {
        "type": "offer",
        "swap_id": "0123456789ab",
        "blake_amount": "10.0",
        "btc_sats": 25000,
        "secret_hash": "11" * 32,
        "btc_timeout": 1000,
        "blake_timeout": 1200,
        "created_at": 1700000000,
        "blake_alice_pub": "02" + "22" * 32,
        "btc_alice_pub": "03" + "33" * 32,

        "timeout_policy_version": "1.5",
        "btc_timeout_window_seconds": 12600,
        "blake_timeout_window_seconds": 27600,
        "bob_recovery_seconds": 14700,
    }

    # 27,600 - 12,600 = 15,000 >= 14,700.

    validate_message(offer)
    ok("valid v1.5 offer")

    encoded = encode_message(offer)

    require_sorted = (
        encoded
        == json.dumps(
            offer,
            separators=(",", ":"),
            sort_keys=True,
            ensure_ascii=False,
        )
    )

    if require_sorted:
        ok("canonical encoding")
    else:
        fail("canonical encoding")

    decoded = decode_message(encoded)

    if decoded == offer:
        ok("encode/decode round trip")
    else:
        fail("encode/decode round trip")

    bad = dict(offer)
    bad["secret_hex"] = "aa" * 32
    expect_reject(
        "secret_hex rejected",
        bad,
    )

    bad = dict(offer)
    bad["swap_id"] = "not-a-swap"
    expect_reject(
        "malformed swap_id rejected",
        bad,
    )

    bad = dict(offer)
    bad["secret_hash"] = "xyz"
    expect_reject(
        "malformed secret hash rejected",
        bad,
    )

    bad = dict(offer)
    bad["btc_sats"] = True
    expect_reject(
        "boolean integer rejected",
        bad,
    )

    bad = dict(offer)
    bad["btc_sats"] = 0
    expect_reject(
        "zero BTC amount rejected",
        bad,
    )

    bad = dict(offer)
    bad["timeout_policy_version"] = "9.9"
    expect_reject(
        "unknown timeout policy rejected",
        bad,
    )

    bad = dict(offer)
    del bad["bob_recovery_seconds"]
    expect_reject(
        "incomplete v1.5 metadata rejected",
        bad,
    )

    bad = dict(offer)
    bad["bob_recovery_seconds"] = 16000
    expect_reject(
        "incoherent recovery budget rejected",
        bad,
    )

    bad = dict(offer)
    bad["surprise"] = "field"
    expect_reject(
        "unknown field rejected",
        bad,
    )

    legacy = dict(offer)

    for field in (
        "timeout_policy_version",
        "btc_timeout_window_seconds",
        "blake_timeout_window_seconds",
        "bob_recovery_seconds",
    ):
        del legacy[field]

    validate_message(legacy)
    ok("legacy offer accepted structurally")

    partial_legacy = dict(legacy)
    partial_legacy[
        "btc_timeout_window_seconds"
    ] = 100

    expect_reject(
        "orphan timeout metadata rejected",
        partial_legacy,
    )

    accept = {
        "type": "accept",
        "swap_id": "0123456789ab",
        "secret_hash": "11" * 32,
        "blake_bob_pub": "02" + "44" * 32,
        "btc_bob_pub": "03" + "55" * 32,
    }

    validate_message(accept)
    ok("valid accept")

    funded = {
        "type": "btc-funded",
        "swap_id": "0123456789ab",
        "secret_hash": "11" * 32,
        "btc_sats": 25000,
        "btc_timeout": 1000,
        "btc_funding_txid": "66" * 32,
        "btc_funding_vout": 0,
    }

    validate_message(funded)
    ok("valid BTC funding announcement")

    duplicate_json = (
        '{"type":"accept",'
        '"type":"offer",'
        '"swap_id":"0123456789ab"}'
    )

    try:
        decode_message(duplicate_json)
    except WireError:
        ok("duplicate JSON key rejected")
    else:
        fail("duplicate JSON key rejected")

    print()
    print("=" * 72)
    print("WIRE VALIDATION SUMMARY")
    print("=" * 72)
    print("passes:  ", PASS_COUNT)
    print("failures:", FAIL_COUNT)

    if FAIL_COUNT:
        raise SystemExit(1)


if __name__ == "__main__":
    main()
