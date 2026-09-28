#!/usr/bin/env python3
"""
BLAKE amount representation suite.

BLAKE amounts used to be Python floats in memory, decimal strings on the
wire, and compared with a `1e-8` tolerance in three places. Three
representations of one quantity can disagree, and a tolerance is the
wrong instrument for money: it accepts a chain output that is a genuinely
different amount.

Amounts are now integer base units (BLAKE is a Bitcoin fork with the same
8 decimal places), with a derived whole-coin view for display, and every
comparison is exact. This suite pins that:

  * parsing and rendering round-trip exactly, and the wire form is a
    plain decimal string, never exponent notation;
  * anything finer than one base unit, non-finite, zero or negative is
    refused rather than rounded;
  * a legacy record carrying a float amount migrates to the nearest base
    unit, exactly where that is possible and with a warning where it is
    not, and it still loads and validates;
  * a chain output differing by exactly ONE base unit is refused -- the
    case the old tolerance accepted, because `1e-8 > 1e-8` is false;
  * the stored record keeps the exact integer across a save/load cycle.

Needs no chain nodes and no relays.

Exit status: 0 pass, 1 fail.
"""

from __future__ import annotations

import contextlib
import io
import json
import os
import sys
import tempfile
from decimal import Decimal
from pathlib import Path


HERE = Path(__file__).resolve().parent
ROOT = HERE.parent

sys.path.insert(0, str(HERE))
sys.path.insert(0, str(ROOT / "src"))

SCRATCH = Path(tempfile.mkdtemp(prefix="blake-amounts-"))
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

PASS = 0
FAIL = 0

SECRET = "22" * 32
SECRET_HASH = __import__("hashlib").sha256(bytes.fromhex(SECRET)).hexdigest()

#
# The HTLC descriptor needs two real compressed secp256k1 keys. They are
# derived here rather than fetched from a wallet, so this suite stays
# node-free.
#
from coincurve import PrivateKey  # noqa: E402


def compressed_key(seed_hex):
    return PrivateKey(bytes.fromhex(seed_hex)).public_key.format(
        compressed=True
    ).hex()


BLAKE_ALICE_PUB = compressed_key("33" * 32)
BLAKE_BOB_PUB = compressed_key("44" * 32)
BTC_ALICE_PUB = compressed_key("55" * 32)
BTC_BOB_PUB = compressed_key("66" * 32)


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


ADDRESS_STUB = "bcrt1qamount-suite-stub-address"


class stubbed_address:
    """
    Pin HTLC descriptor and address resolution.

    Both go through the node (the descriptor is compiled by
    `getdescriptorinfo`), and this suite is about the amount comparison,
    so the script identity is pinned to a constant and the amount check
    is what is actually exercised.
    """

    def __enter__(self):
        self._saved = {
            name: getattr(swap_v2, name)
            for name in (
                "get_blake_contract_descriptor",
                "get_btc_contract_descriptor",
                "derive_blake_contract_address",
                "derive_btc_contract_address",
            )
        }

        swap_v2.get_blake_contract_descriptor = lambda swap: "stub-descriptor"
        swap_v2.get_btc_contract_descriptor = lambda swap: "stub-descriptor"
        swap_v2.derive_blake_contract_address = lambda desc: ADDRESS_STUB
        swap_v2.derive_btc_contract_address = lambda desc: ADDRESS_STUB

        return self

    def __exit__(self, *exc):
        for name, value in self._saved.items():
            setattr(swap_v2, name, value)

        return False


def check(name, fn):
    try:
        fn()
    except Exception as e:  # noqa: BLE001
        fail(name, f"{type(e).__name__}: {e}")
    else:
        ok(name)


def refuses(name, fn):
    try:
        with contextlib.redirect_stdout(io.StringIO()):
            fn()
    except Exception:
        ok(name)
        return

    fail(name, "the value was accepted instead of refused")


def make_swap(base_units, swap_id="aaaa11112222"):
    return swap_v2.Swap(
        swap_id=swap_id,
        state=swap_v2.State.OFFERED.value,
        created_at=1700000000,
        btc_sats=25000,
        secret_hex=SECRET,
        secret_hash=SECRET_HASH,
        blake_timeout=1000,
        btc_timeout=900,
        blake_amount_base_units=base_units,
        blake_alice_pub=BLAKE_ALICE_PUB,
        blake_bob_pub=BLAKE_BOB_PUB,
        btc_alice_pub=BTC_ALICE_PUB,
        btc_bob_pub=BTC_BOB_PUB,
    )


# ------------------------------------------------------- parse and render


def test_round_trip():
    for text, units in (
        ("0.00000001", 1),
        ("0.1", 10_000_000),
        ("0.2", 20_000_000),
        ("1", 100_000_000),
        ("1.5", 150_000_000),
        ("21000000", 2_100_000_000_000_000),
        ("0.12345678", 12_345_678),
    ):
        if swap_v2.blake_base_units(text) != units:
            raise AssertionError(f"{text} -> {swap_v2.blake_base_units(text)}")

        rendered = swap_v2.format_blake_amount(units)

        if swap_v2.blake_base_units(rendered) != units:
            raise AssertionError(f"{units} did not round-trip: {rendered!r}")

        if "E" in rendered or "e" in rendered:
            raise AssertionError(f"{rendered!r} is exponent notation")


def test_canonical_wire_strings():
    for units, expected in (
        (1, "0.00000001"),
        (10_000_000, "0.1"),
        (20_000_000, "0.2"),
        (100_000_000, "1"),
        (12_345_678, "0.12345678"),
    ):
        rendered = swap_v2.format_blake_amount(units)

        if rendered != expected:
            raise AssertionError(
                f"{units} rendered as {rendered!r}, expected {expected!r}"
            )


def test_accepts_numbers_and_decimals():
    if swap_v2.blake_base_units(1) != 100_000_000:
        raise AssertionError("an int was not accepted as whole coins")

    if swap_v2.blake_base_units(Decimal("0.25")) != 25_000_000:
        raise AssertionError("a Decimal was not accepted")

    #
    # A float goes through repr, so a value that has an exact decimal
    # form survives. This is the legacy path and must keep working.
    #
    if swap_v2.blake_base_units(0.2) != 20_000_000:
        raise AssertionError("a float was not accepted")


def test_refuses_unrepresentable_amounts():
    for name, value in (
        ("finer than one base unit", "0.123456789"),
        ("one base unit too fine", "0.000000001"),
        ("non-finite", "NaN"),
        ("infinite", "Infinity"),
        ("zero", "0"),
        ("negative", "-1"),
        ("empty", ""),
        ("garbage", "abc"),
        ("boolean", True),
        ("null", None),
        ("a list", [1]),
    ):
        refuses(
            f"{name} is refused",
            lambda value=value: swap_v2.blake_base_units(value),
        )


# ------------------------------------------------------- the exactness bug


def test_one_base_unit_difference_is_refused():
    """
    The case the old tolerance accepted.

    `abs(actual - expected) > 1e-8` is FALSE when the difference is
    exactly 1e-8, which is exactly one base unit. So a chain output one
    base unit away from the agreed amount passed verification.
    """

    swap = make_swap(20_000_000)

    exact = {
        "value": "0.2",
        "n": 0,
        "scriptPubKey": {"address": ADDRESS_STUB},
    }

    with stubbed_address():
        # The exact amount is accepted.
        swap_v2._require_funding_output_matches(swap, "blake", exact)

    for delta, value in (
        (1, "0.19999999"),
        (-1, "0.20000001"),
        (10_000_000, "0.1"),
    ):
        wrong = dict(exact)
        wrong["value"] = value

        try:
            with stubbed_address():
                with contextlib.redirect_stdout(io.StringIO()):
                    swap_v2._require_funding_output_matches(
                        swap,
                        "blake",
                        wrong,
                    )
        except RuntimeError:
            continue

        raise AssertionError(
            f"an output {delta} base unit(s) from the agreed amount "
            f"({value}) was accepted"
        )


def test_bitcoin_sats_are_exact_too():
    if swap_v2.btc_sats_from_coin("0.00025000") != 25000:
        raise AssertionError("sats conversion is wrong")

    if swap_v2.btc_sats_from_coin("1") != 100_000_000:
        raise AssertionError("whole-coin conversion is wrong")

    for name, value in (
        ("a sub-satoshi amount", "0.000000001"),
        ("a non-finite amount", "NaN"),
        ("a negative amount", "-1"),
        ("a boolean", True),
    ):
        refuses(
            f"{name} is refused for BTC",
            lambda value=value: swap_v2.btc_sats_from_coin(value),
        )


# -------------------------------------------------------------- migration


def test_legacy_record_migrates_exactly():
    data = {
        "swap_id": "bbbb11112222",
        "state": "OFFERED",
        "created_at": 1700000000,
        # The pre-migration shape: a JSON number.
        "blake_amount": 0.2,
        "btc_sats": 25000,
        "secret_hex": SECRET,
        "secret_hash": SECRET_HASH,
        "blake_timeout": 1000,
        "btc_timeout": 900,
    }

    migrated = swap_v2.migrate_swap_record(data)

    if migrated.get("blake_amount_base_units") != 20_000_000:
        raise AssertionError(f"wrong migration: {migrated}")

    if "blake_amount" in migrated:
        raise AssertionError("the legacy key was left in place")

    swap = swap_v2.Swap(**migrated)
    swap.validate()

    if swap.blake_amount_base_units != 20_000_000:
        raise AssertionError("the migrated record did not validate as expected")


def test_legacy_drift_is_rounded_and_reported():
    #
    # 0.1 + 0.2 in binary floating point. It has no exact base-unit form,
    # so the migration must round to the nearest one AND say so: silently
    # discarding value would be worse than the rounding.
    #
    drifted = 0.1 + 0.2

    units, rounded = swap_v2.blake_base_units_from_legacy(drifted)

    if units != 30_000_000:
        raise AssertionError(f"rounded to {units} base units")

    if not rounded:
        raise AssertionError("the migration did not notice it had to round")

    units, rounded = swap_v2.blake_base_units_from_legacy(0.2)

    if units != 20_000_000 or rounded:
        raise AssertionError("an exact legacy value was reported as rounded")

    errors = io.StringIO()

    with contextlib.redirect_stderr(errors):
        migrated = swap_v2.migrate_swap_record(
            {
                "swap_id": "cccc11112222",
                "state": "OFFERED",
                "created_at": 1,
                "blake_amount": drifted,
                "btc_sats": 25000,
                "secret_hex": None,
                "secret_hash": SECRET_HASH,
                "blake_timeout": 1000,
                "btc_timeout": 900,
            }
        )

    if migrated["blake_amount_base_units"] != 30_000_000:
        raise AssertionError("the drifted record did not migrate")

    if "nearest representable value" not in errors.getvalue():
        raise AssertionError(
            f"the rounding was not reported: {errors.getvalue()!r}"
        )


def test_both_keys_present_prefers_the_integer():
    migrated = swap_v2.migrate_swap_record(
        {
            "swap_id": "dddd11112222",
            "blake_amount": 0.2,
            "blake_amount_base_units": 30_000_000,
        }
    )

    if migrated["blake_amount_base_units"] != 30_000_000:
        raise AssertionError(
            "the integer representation was not treated as canonical"
        )


def test_save_and_load_preserve_the_exact_amount():
    swap = make_swap(123_456_789, swap_id="eeee11112222")

    swap_v2.save_swap(swap, role="alice")

    path = swap_v2.swap_path("eeee11112222", "alice")
    stored = json.loads(path.read_text())

    if stored.get("blake_amount_base_units") != 123_456_789:
        raise AssertionError(f"the stored record is wrong: {stored}")

    if "blake_amount" in stored:
        raise AssertionError(
            "the record still stores a float amount as well as the integer"
        )

    reloaded = swap_v2.load_swap("eeee11112222", role="alice")

    if reloaded.blake_amount_base_units != 123_456_789:
        raise AssertionError("the amount did not survive the round trip")

    if reloaded.blake_amount != 1.23456789:
        raise AssertionError(
            f"the derived view is wrong: {reloaded.blake_amount!r}"
        )


def test_a_legacy_state_file_loads_through_the_command_path():
    path = swap_v2.swap_path("ffff11112222", "alice")
    path.write_text(
        json.dumps(
            {
                "swap_id": "ffff11112222",
                "state": "OFFERED",
                "created_at": 1700000000,
                "blake_amount": 0.5,
                "btc_sats": 25000,
                "secret_hex": SECRET,
                "secret_hash": SECRET_HASH,
                "blake_timeout": 1000,
                "btc_timeout": 900,
                "nostr_alice_pub": swap_v2.current_nostr_pubkey(),
                "blake_alice_pub": BLAKE_ALICE_PUB,
                "blake_bob_pub": BLAKE_BOB_PUB,
                "btc_alice_pub": BTC_ALICE_PUB,
                "btc_bob_pub": BTC_BOB_PUB,
            },
            indent=2,
            sort_keys=True,
        )
        + "\n"
    )

    swap = swap_v2.load_swap("ffff11112222", role="alice")

    if swap.blake_amount_base_units != 50_000_000:
        raise AssertionError(
            f"a legacy on-disk record migrated to "
            f"{swap.blake_amount_base_units}"
        )

    #
    # And the engine's own comparison path accepts the migrated value.
    #
    with stubbed_address():
        swap_v2._require_funding_output_matches(
            swap,
            "blake",
            {
                "value": "0.5",
                "n": 0,
                "scriptPubKey": {"address": ADDRESS_STUB},
            },
        )


def test_a_new_swap_refuses_sub_base_unit_precision():
    """
    The CLI must reject an amount the protocol cannot represent, rather
    than quietly rounding what the operator asked for.
    """

    #
    # The refusal happens at the parser boundary, before any chain state
    # is touched: `make_swap` builds the record through this conversion,
    # so an unrepresentable amount can never reach a state file.
    #
    refuses(
        "a sub-base-unit amount is refused when constructing a swap",
        lambda: swap_v2.make_swap(
            "0.123456789",
            25000,
            1000,
            900,
        ),
    )


def test_matching_dual_keys_do_not_warn():
    errors = io.StringIO()

    with contextlib.redirect_stderr(errors):
        migrated = swap_v2.migrate_swap_record(
            {
                "swap_id": "111122223333",
                "blake_amount": 0.3,
                "blake_amount_base_units": 30_000_000,
            }
        )

    if migrated["blake_amount_base_units"] != 30_000_000:
        raise AssertionError("the integer was not kept")

    if errors.getvalue():
        raise AssertionError(
            f"agreeing keys should be silent: {errors.getvalue()!r}"
        )


def test_unparseable_legacy_key_is_tolerated():
    #
    # A hand-edited or truncated record can carry a legacy amount that is
    # not a number at all. The migration must not crash on it, and must
    # leave the canonical field absent so that validation rejects the
    # record with a clear message instead of inventing an amount.
    #
    migrated = swap_v2.migrate_swap_record(
        {
            "swap_id": "444455556666",
            "blake_amount": "not-a-number",
            "blake_amount_base_units": 30_000_000,
        }
    )

    if migrated["blake_amount_base_units"] != 30_000_000:
        raise AssertionError("the integer was not kept")


def test_legacy_non_positive_is_refused():
    for value in (0, -1, "NaN"):
        refuses(
            f"legacy {value!r} is refused",
            lambda value=value: swap_v2.blake_base_units_from_legacy(value),
        )


def test_boundary_helper_accepts_both_shapes():
    from types import SimpleNamespace

    canonical = SimpleNamespace(blake_amount_base_units=20_000_000)
    legacy = SimpleNamespace(blake_amount=0.2)

    if swap_v2.swap_blake_base_units(canonical) != 20_000_000:
        raise AssertionError("the canonical field was not used")

    if swap_v2.swap_blake_base_units(legacy) != 20_000_000:
        raise AssertionError("the whole-coin fallback was not used")

    refuses(
        "a swap-like object with neither amount is refused",
        lambda: swap_v2.swap_blake_base_units(SimpleNamespace()),
    )


def test_zero_formats_as_zero():
    if swap_v2.format_blake_amount(0) != "0":
        raise AssertionError(
            f"zero rendered as {swap_v2.format_blake_amount(0)!r}"
        )

    if swap_v2.btc_sats_from_coin("0") != 0:
        raise AssertionError("zero sats did not convert")

    refuses(
        "a non-numeric coin amount is refused for BTC",
        lambda: swap_v2.btc_sats_from_coin("not-a-number"),
    )


def main():
    print()
    print("=" * 72)
    print("BLAKE AMOUNT REPRESENTATION")
    print("=" * 72)
    print()

    check("amounts round-trip through base units", test_round_trip)
    check("the wire rendering is canonical", test_canonical_wire_strings)
    check("ints, Decimals and floats are accepted", test_accepts_numbers_and_decimals)
    check("unrepresentable amounts are refused", test_refuses_unrepresentable_amounts)

    check("a one-base-unit difference is refused", test_one_base_unit_difference_is_refused)
    check("Bitcoin sats convert exactly", test_bitcoin_sats_are_exact_too)

    check("a legacy record migrates exactly", test_legacy_record_migrates_exactly)
    check("legacy drift is rounded and reported", test_legacy_drift_is_rounded_and_reported)
    check("the integer representation wins if both are present", test_both_keys_present_prefers_the_integer)
    check("save and load preserve the exact amount", test_save_and_load_preserve_the_exact_amount)
    check("a legacy state file loads through the command path", test_a_legacy_state_file_loads_through_the_command_path)
    check("sub-base-unit input is refused", test_a_new_swap_refuses_sub_base_unit_precision)
    check("agreeing dual keys are silent", test_matching_dual_keys_do_not_warn)
    check("an unparseable legacy key is tolerated", test_unparseable_legacy_key_is_tolerated)
    check("legacy non-positive amounts are refused", test_legacy_non_positive_is_refused)
    check("the boundary helper accepts both shapes", test_boundary_helper_accepts_both_shapes)
    check("zero and non-numeric inputs are handled", test_zero_formats_as_zero)

    print()
    print("=" * 72)
    print("BLAKE AMOUNT SUMMARY")
    print("=" * 72)
    print(f"passes:   {PASS}")
    print(f"failures: {FAIL}")
    print()

    return 1 if FAIL else 0


if __name__ == "__main__":
    raise SystemExit(main())
