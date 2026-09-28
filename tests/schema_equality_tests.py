#!/usr/bin/env python3
"""
Schema-equality suite.

The engine used to re-implement its own field and type checks instead of
using `src/swap_wire.py`, so the two could drift apart and only an ad-hoc
check would notice. `swap_wire` is now the canonical schema and the engine
runs it at the transport boundary; this suite is the guard that keeps it
that way.

For **every** settlement type it verifies that the engine's acceptance
boundary *is* the schema:

  * a message accepted by the schema is accepted by the engine, field for
    field, including the integer and hex shapes;
  * every schema-required field is required by the engine;
  * an unknown field is refused by both, which is what closes the
    "nested or renamed `secret_hex`" hole -- the invariant is enforced by
    shape rather than by one key name;
  * a nested `secret_hex` is refused by both;
  * the engine never transmits an engine-internal annotation
    (`_nostr_pubkey` and friends) on the wire.

It also checks the two directions statically:

  * every message type the engine hands to the transport has a schema, so
    a new message type cannot be added to the engine alone;
  * `SETTLEMENT_TYPES` and `SCHEMAS` cannot disagree.

Needs no chain nodes and no relays: `validate_transport_message` is pure.

Exit status: 0 pass, 1 fail.
"""

from __future__ import annotations

import ast
import copy
import contextlib
import io
import os
import sys
import tempfile
from pathlib import Path


HERE = Path(__file__).resolve().parent
ROOT = HERE.parent

sys.path.insert(0, str(HERE))
sys.path.insert(0, str(ROOT / "src"))

SCRATCH = Path(tempfile.mkdtemp(prefix="blake-schema-"))
KEY_FILE = SCRATCH / "participant.key"
KEY_FILE.write_text("11" * 32 + "\n")
KEY_FILE.chmod(0o600)

os.environ["BLAKE_SWAP_STATE_DIR"] = str(SCRATCH)
os.environ["BLAKE_SWAP_TRANSPORT"] = "file"
os.environ["BLAKE_SWAP_NOSTR_RELAYS"] = "ws://127.0.0.1:1"
os.environ["BLAKE_SWAP_PARTICIPANT_KEY_FILE"] = str(KEY_FILE)
os.environ.pop("BLAKE_SWAP_NOSTR_KEY_FILE", None)
os.environ.pop("BLAKE_SWAP_NOSTR_KEY_DIR", None)

import swap_v2  # noqa: E402
import swap_wire  # noqa: E402

PASS = 0
FAIL = 0

SWAP = "0123456789ab"

HASH = "11" * 32
TXID = "66" * 32
BLOCKHASH = "77" * 32
PUBKEY_A = "02" + "22" * 32
PUBKEY_B = "03" + "33" * 32


def ok(name):
    global PASS
    PASS += 1
    print(f"PASS  {name}")


def fail(name, detail):
    global FAIL
    FAIL += 1
    print(f"FAIL  {name}")
    for line in str(detail).splitlines():
        print(f"      {line}")


def check(name, fn):
    try:
        fn()
    except Exception as e:  # noqa: BLE001
        fail(name, f"{type(e).__name__}: {e}")
    else:
        ok(name)


def sample(type_name):
    """
    One valid message per settlement type.

    Built from the schema itself where possible, so this suite cannot
    quietly disagree with the schema it is checking. Types whose fields
    the schema does not describe (no per-field types exist in SCHEMAS,
    only required/optional names) carry hand-written values that match
    the canonical validator's own rules.
    """

    values = {
        "type": type_name,
        "swap_id": SWAP,
        "blake_amount": "0.2",
        "btc_sats": 25000,
        "secret_hash": HASH,
        "btc_timeout": 1000,
        "blake_timeout": 1200,
        "created_at": 1700000000,
        "blake_alice_pub": PUBKEY_A,
        "btc_alice_pub": PUBKEY_B,
        "blake_bob_pub": PUBKEY_A,
        "btc_bob_pub": PUBKEY_B,
        "timeout_policy_version": "1.5",
        "btc_timeout_window_seconds": 12600,
        "blake_timeout_window_seconds": 27600,
        "bob_recovery_seconds": 14700,
        "blake_funding_txid": TXID,
        "blake_funding_vout": 0,
        "btc_funding_txid": TXID,
        "btc_funding_vout": 1,
        "btc_claim_txid": TXID,
        "btc_claim_blockhash": BLOCKHASH,
        "blake_claim_txid": TXID,
        "blake_claim_blockhash": BLOCKHASH,
        "btc_refund_txid": TXID,
        "btc_refund_blockhash": BLOCKHASH,
    }

    schema = swap_wire.SCHEMAS[type_name]

    fields = set(schema["required"]) | set(schema.get("optional") or [])

    return {name: values[name] for name in fields}


def engine_accepts(message):
    """Run one message through the engine's transport boundary."""

    with contextlib.redirect_stdout(io.StringIO()):
        swap_v2.validate_transport_message(
            message["swap_id"],
            message["type"],
            message,
            allow_internal=True,
        )


def engine_refuses(message):
    try:
        engine_accepts(message)
    except Exception:
        return True

    return False


# ------------------------------------------------------------ equivalence


def test_every_type_is_accepted_by_both():
    problems = []

    for type_name in sorted(swap_wire.SCHEMAS):
        message = sample(type_name)

        try:
            swap_wire.validate_message(message, expected_type=type_name)
        except swap_wire.WireError as e:
            problems.append(f"{type_name}: the schema rejected its own sample: {e}")
            continue

        try:
            engine_accepts(message)
        except Exception as e:  # noqa: BLE001
            problems.append(
                f"{type_name}: the engine rejected a schema-valid message: "
                f"{type(e).__name__}: {e}"
            )

    if problems:
        raise AssertionError("\n".join(problems))


def test_every_required_field_is_required_by_the_engine():
    problems = []

    for type_name in sorted(swap_wire.SCHEMAS):
        for field in sorted(swap_wire.SCHEMAS[type_name]["required"]):
            message = sample(type_name)
            message.pop(field)

            if not engine_refuses(message):
                problems.append(
                    f"{type_name}: the engine accepted a message with no "
                    f"{field!r}"
                )

    if problems:
        raise AssertionError("\n".join(problems))


def test_unknown_fields_are_refused_by_both():
    problems = []

    for type_name in sorted(swap_wire.SCHEMAS):
        message = sample(type_name)
        message["unexpected_field"] = "x"

        try:
            with contextlib.redirect_stdout(io.StringIO()):
                swap_wire.validate_message(message, expected_type=type_name)
        except swap_wire.WireError:
            pass
        else:
            problems.append(f"{type_name}: the schema accepted an unknown field")

        if not engine_refuses(message):
            problems.append(
                f"{type_name}: the engine accepted an unknown field"
            )

    if problems:
        raise AssertionError("\n".join(problems))


def test_nested_secret_hex_is_refused_everywhere():
    """
    The original prohibition was an exact top-level key check, so a
    renamed or nested field would have passed. The schema closes that by
    rejecting unknown fields; this pins the case that motivated it.
    """

    problems = []

    for type_name in sorted(swap_wire.SCHEMAS):
        for shape in (
            {"secret": {"secret_hex": "aa" * 32}},
            {"reveal": {"secret_hex": "aa" * 32}},
            {"payload": [{"secret_hex": "aa" * 32}]},
            {"Secret_Hex": "aa" * 32},
        ):
            message = sample(type_name)
            message.update(shape)

            if not engine_refuses(message):
                problems.append(
                    f"{type_name}: the engine accepted {shape}"
                )

    if problems:
        raise AssertionError("\n".join(problems))


def test_top_level_secret_hex_still_reports_a_security_failure():
    """
    The explicit check survives as defence in depth, and it must keep
    raising the SECURITY FAILURE the rest of the engine looks for rather
    than a generic schema error.
    """

    message = sample("accept")
    message["secret_hex"] = "aa" * 32

    try:
        engine_accepts(message)
    except RuntimeError as e:
        if "SECURITY FAILURE" not in str(e) or "secret_hex" not in str(e):
            raise AssertionError(f"unhelpful refusal: {e}") from e
    else:
        raise AssertionError("a top-level secret_hex was accepted")


# --------------------------------------------------------------- hygiene


def test_internal_annotations_are_never_transmitted():
    """
    The receive path annotates a message with the authenticated sender.
    That annotation must be tolerated on receipt and refused on send, or
    it would leak an engine detail onto the wire.
    """

    message = sample("accept")
    annotated = dict(message, _nostr_pubkey="aa" * 32)

    # Receipt tolerates it.
    with contextlib.redirect_stdout(io.StringIO()):
        swap_v2.validate_transport_message(
            annotated["swap_id"],
            "accept",
            annotated,
            allow_internal=True,
        )

    # Sending it does not.
    try:
        with contextlib.redirect_stdout(io.StringIO()):
            swap_v2.validate_transport_message(
                annotated["swap_id"],
                "accept",
                annotated,
            )
    except RuntimeError as e:
        if "engine-internal" not in str(e):
            raise AssertionError(f"unhelpful refusal: {e}") from e
    else:
        raise AssertionError(
            "an engine-internal annotation was accepted for transmission"
        )


def test_the_wire_view_is_exactly_the_wire_fields():
    message = sample("btc-claimed")
    annotated = dict(message, _nostr_pubkey="aa" * 32, _other=1)

    view = swap_v2._wire_view(annotated)

    if view != message:
        raise AssertionError(
            "the wire view is not the message without its internal keys: "
            f"{sorted(view)} vs {sorted(message)}"
        )


def test_engine_message_types_are_all_in_the_schema():
    """
    The other direction of the equality: a message type the engine hands
    to the transport must have a schema. This reads the engine source, so
    a new type cannot be added without one.
    """

    source = (ROOT / "src" / "swap_v2.py").read_text()
    tree = ast.parse(source)

    transport_calls = {
        "receive_message",
        "send_message",
        "file_send_message",
        "file_receive_message",
        "nostr_send_message",
        "nostr_receive_message",
    }

    used = {}

    for node in ast.walk(tree):
        if not isinstance(node, ast.Call):
            continue

        if not isinstance(node.func, ast.Name):
            continue

        if node.func.id not in transport_calls:
            continue

        for arg in node.args:
            if isinstance(arg, ast.Constant) and isinstance(arg.value, str):
                used.setdefault(arg.value, set()).add(node.func.id)

    if not used:
        raise AssertionError(
            "no transport message types were found; this check needs updating"
        )

    unknown = sorted(set(used) - set(swap_wire.SETTLEMENT_TYPES))

    if unknown:
        raise AssertionError(
            "the engine hands these types to the transport but they have no "
            f"schema: {unknown}"
        )

    #
    # And every schema type must actually be used, or the schema is
    # carrying a type the engine does not implement.
    #
    unused = sorted(set(swap_wire.SETTLEMENT_TYPES) - set(used))

    if unused:
        raise AssertionError(
            f"these schema types are never used by the engine: {unused}"
        )


def test_settlement_types_match_the_schema_keys():
    if swap_wire.SETTLEMENT_TYPES != set(swap_wire.SCHEMAS):
        raise AssertionError(
            "SETTLEMENT_TYPES and SCHEMAS disagree: "
            f"{sorted(swap_wire.SETTLEMENT_TYPES)} vs "
            f"{sorted(swap_wire.SCHEMAS)}"
        )


def test_required_and_optional_do_not_overlap():
    problems = []

    for type_name, schema in sorted(swap_wire.SCHEMAS.items()):
        overlap = set(schema["required"]) & set(schema.get("optional") or [])

        if overlap:
            problems.append(f"{type_name}: {sorted(overlap)}")

    if problems:
        raise AssertionError(
            "fields are both required and optional:\n" + "\n".join(problems)
        )


def test_the_schema_round_trips_through_encode_and_decode():
    problems = []

    for type_name in sorted(swap_wire.SCHEMAS):
        message = sample(type_name)

        try:
            encoded = swap_wire.encode_message(message)
            decoded = swap_wire.decode_message(encoded)
        except Exception as e:  # noqa: BLE001
            problems.append(f"{type_name}: {type(e).__name__}: {e}")
            continue

        if decoded != message:
            problems.append(f"{type_name}: the round trip changed the message")

    if problems:
        raise AssertionError("\n".join(problems))


def test_the_engine_enforces_the_amount_precision_rule():
    """
    The schema and the engine must agree about the smallest unit, because
    the engine now builds its amounts as integers.
    """

    message = sample("offer")
    message["blake_amount"] = "0.123456789"

    if not engine_refuses(message):
        raise AssertionError(
            "the engine accepted an amount finer than one base unit"
        )

    message["blake_amount"] = "0.00000001"

    with contextlib.redirect_stdout(io.StringIO()):
        engine_accepts(message)


def main():
    print()
    print("=" * 72)
    print("CANONICAL SCHEMA EQUALITY")
    print("=" * 72)
    print()
    print(f"settlement types: {len(swap_wire.SCHEMAS)}")
    print()

    check("every type is accepted by both", test_every_type_is_accepted_by_both)
    check("every required field is required by the engine", test_every_required_field_is_required_by_the_engine)
    check("unknown fields are refused by both", test_unknown_fields_are_refused_by_both)
    check("a nested secret_hex is refused everywhere", test_nested_secret_hex_is_refused_everywhere)
    check("a top-level secret_hex is a SECURITY FAILURE", test_top_level_secret_hex_still_reports_a_security_failure)

    check("internal annotations are never transmitted", test_internal_annotations_are_never_transmitted)
    check("the wire view is exactly the wire fields", test_the_wire_view_is_exactly_the_wire_fields)
    check("every engine message type has a schema", test_engine_message_types_are_all_in_the_schema)
    check("SETTLEMENT_TYPES matches the schema keys", test_settlement_types_match_the_schema_keys)
    check("required and optional never overlap", test_required_and_optional_do_not_overlap)
    check("the schema round-trips through encode and decode", test_the_schema_round_trips_through_encode_and_decode)
    check("the engine enforces the amount precision rule", test_the_engine_enforces_the_amount_precision_rule)

    print()
    print("=" * 72)
    print("SCHEMA EQUALITY SUMMARY")
    print("=" * 72)
    print(f"passes:   {PASS}")
    print(f"failures: {FAIL}")
    print()

    return 1 if FAIL else 0


if __name__ == "__main__":
    raise SystemExit(main())
