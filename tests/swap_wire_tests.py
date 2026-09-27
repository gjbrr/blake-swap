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
    sha256_hex,
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

    expect_reject(
        "versionless timeout offer rejected",
        legacy,
    )

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

    #
    # Structural refusals. Each of these names the exact contract the
    # validator is responsible for, so a regression points at one rule
    # rather than at "some validation".
    #
    for name, raw in (
        ("a non-object message", "not a dict"),
        ("a list message", []),
        ("a null message", None),
    ):
        expect_reject(name + " is rejected", raw)

    for name, raw in (
        ("invalid UTF-8", b"\xff\xfe"),
        ("a non-text, non-bytes input", 12345),
        ("malformed JSON", "{not json"),
    ):
        try:
            decode_message(raw)
        except WireError:
            ok(name + " is rejected")
        else:
            fail(name + " is rejected")

    #
    # Field types. bool is an int subclass in Python, so a JSON `true`
    # must not be accepted where an integer is required.
    #
    for name, field, value in (
        ("a boolean btc_sats", "btc_sats", True),
        ("a string btc_sats", "btc_sats", "25000"),
        ("a float btc_sats", "btc_sats", 25000.5),
        ("an integer blake_amount", "blake_amount", 10),
    ):
        bad = dict(offer)
        bad[field] = value
        expect_reject(name + " is rejected", bad)

    #
    # Amounts are decimal strings, and they must be finite and positive.
    #
    for name, value in (
        ("a non-numeric amount", "abc"),
        ("a non-finite amount", "NaN"),
        ("an infinite amount", "Infinity"),
        ("a zero amount", "0"),
        ("a negative amount", "-1"),
    ):
        bad = dict(offer)
        bad["blake_amount"] = value
        expect_reject(name + " is rejected", bad)

    #
    # Keys and hashes are lowercase 32-byte hex; pubkeys are compressed
    # secp256k1 points.
    #
    for name, field, value in (
        ("an uppercase secret_hash", "secret_hash", "AA" * 32),
        ("a short secret_hash", "secret_hash", "11" * 31),
        ("an odd-length secret_hash", "secret_hash", "1" * 63),
        ("a non-hex secret_hash", "secret_hash", "zz" * 32),
        ("an uncompressed blake_alice_pub", "blake_alice_pub", "04" + "22" * 32),
        ("a short btc_alice_pub", "btc_alice_pub", "02" + "33" * 31),
        ("a non-hex btc_alice_pub", "btc_alice_pub", "02" + "zz" * 32),
    ):
        bad = dict(offer)
        bad[field] = value
        expect_reject(name + " is rejected", bad)

    #
    # The v1.5 offer windows must be present and positive. Absence is
    # caught by the schema before the v1.5 rules run, so the message
    # differs -- but it is still a rejection.
    #
    for name, mutate in (
        (
            "a missing timeout_policy_version",
            lambda m: m.pop("timeout_policy_version"),
        ),
        (
            "an unsupported timeout_policy_version",
            lambda m: m.update(timeout_policy_version="1.4"),
        ),
        (
            "a missing timeout window",
            lambda m: m.pop("blake_timeout_window_seconds"),
        ),
        (
            "a missing recovery window",
            lambda m: m.pop("bob_recovery_seconds"),
        ),
        (
            "a zero timeout window",
            lambda m: m.update(btc_timeout_window_seconds=0),
        ),
        (
            "a negative timeout window",
            lambda m: m.update(btc_timeout_window_seconds=-1),
        ),
        (
            "a boolean timeout window",
            lambda m: m.update(btc_timeout_window_seconds=True),
        ),
    ):
        bad = dict(offer)
        mutate(bad)
        expect_reject(name + " is rejected", bad)

    #
    # Every settlement type has its own validator, so each one needs at
    # least one valid message and one malformed variant. `btc-refunded`
    # is the one the flow tests never need.
    #
    refunded = {
        "type": "btc-refunded",
        "swap_id": "0123456789ab",
        "secret_hash": "11" * 32,
        "btc_refund_txid": "66" * 32,
        "btc_refund_blockhash": "77" * 32,
    }

    try:
        validate_message(refunded, expected_type="btc-refunded")
    except WireError as e:
        fail(f"a valid btc-refunded message is accepted: {e}")
    else:
        ok("a valid btc-refunded message is accepted")

    for name, field, value in (
        ("a short btc_refund_txid", "btc_refund_txid", "66" * 31),
        ("an uppercase btc_refund_blockhash", "btc_refund_blockhash", "AB" * 32),
        ("a non-hex btc_refund_txid", "btc_refund_txid", "zz" * 32),
    ):
        bad = dict(refunded)
        bad[field] = value
        expect_reject(name + " is rejected", bad)

    #
    # Integer fields reject non-integers as well as booleans.
    #
    for name, value in (
        ("a string timeout window", "3600"),
        ("a float timeout window", 3600.5),
        ("a null timeout window", None),
    ):
        bad = dict(offer)
        bad["btc_timeout_window_seconds"] = value
        expect_reject(name + " is rejected", bad)

    #
    # An offer with an unknown type, and one whose declared type does not
    # match the type the caller expects.
    #
    unknown = dict(offer)
    unknown["type"] = "teleport"
    expect_reject("an unsupported message type is rejected", unknown)

    try:
        validate_message(offer, expected_type="accept")
    except WireError:
        ok("a mismatched expected_type is rejected")
    else:
        fail("a mismatched expected_type is rejected")

    #
    # decode_message must not accept a duplicate key anywhere, including
    # inside the nested amount fields.
    #
    nested_duplicate = (
        '{"type":"offer",'
        '"swap_id":"0123456789ab",'
        '"blake_amount":"1.0","blake_amount":"2.0"}'
    )

    try:
        decode_message(nested_duplicate)
    except WireError:
        ok("a duplicate nested key is rejected")
    else:
        fail("a duplicate nested key is rejected")

    #
    # decode_message is the only entry point that accepts bytes.
    #
    if decode_message(encode_message(offer).encode("utf-8")) == offer:
        ok("bytes input round-trips")
    else:
        fail("bytes input round-trips")

    #
    # The helper independent implementations use must be plain SHA-256.
    #
    if (
        sha256_hex(b"")
        == "e3b0c44298fc1c149afbf4c8996fb924"
        "27ae41e4649b934ca495991b7852b855"
    ):
        ok("sha256_hex matches the empty-input digest")
    else:
        fail("sha256_hex matches the empty-input digest")

    if (
        sha256_hex(b"abc")
        == "ba7816bf8f01cfea414140de5dae2223"
        "b00361a396177a9cb410ff61f20015ad"
    ):
        ok("sha256_hex matches the known digest")
    else:
        fail("sha256_hex matches the known digest")

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
