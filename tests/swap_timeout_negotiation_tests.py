#!/usr/bin/env python3

import json
import math
import time

import swap_protocol_tests as h

from swap_timeout_policy import (
    TimeoutPolicy,
    build_timeout_plan,
)


def banner(text):
    print()
    print("=" * 72)
    print(text)
    print("=" * 72)


def make_policy_swap(
    *,
    blake=10.0,
    sats=25_000,
):
    """
    Unlike h.make_fresh_swap(), deliberately omit explicit
    timeout heights so swap_v2 invokes v1.5 TimeoutPolicy.
    """

    p = h.swap_cmd(
        "new",
        "--blake",
        str(blake),
        "--sats",
        str(sats),
        role="alice",
    )

    swap_id = h.parse_swap_id(
        p.stdout
    )

    state = h.load_state(
        swap_id,
        "alice",
    )

    h.require(
        state.get(
            "timeout_policy_version"
        ) == "1.5",
        (
            "fresh policy swap did not persist "
            "timeout_policy_version=1.5"
        ),
    )

    return swap_id


def publish_policy_offer():
    swap_id = make_policy_swap()

    h.swap_cmd(
        "publish-offer",
        swap_id,
        role="alice",
    )

    path = h.message_file(
        swap_id,
        "offer",
    )

    h.require(
        path.exists(),
        f"offer message missing: {path}",
    )

    return swap_id, path


def require_rejected_cleanly(
    swap_id,
    *,
    contains,
):
    """
    Bob must fail before establishing durable participant
    state or transmitting an acceptance.
    """

    proc = h.swap_cmd(
        "accept-offer",
        swap_id,
        role="bob",
        expect_ok=False,
    )

    h.expect_failure(
        "unsafe timeout offer",
        proc,
        contains,
    )

    bob_state = h.state_file(
        swap_id,
        "bob",
    )

    accept_message = h.message_file(
        swap_id,
        "accept",
    )

    h.require(
        not bob_state.exists(),
        (
            "Bob created durable swap state "
            "after rejecting unsafe offer"
        ),
    )

    h.require(
        not accept_message.exists(),
        (
            "Bob transmitted acceptance "
            "after rejecting unsafe offer"
        ),
    )


def protocol_payload(data):
    if (
        isinstance(data, dict)
        and data.get("_file_transport_version") == 2
        and isinstance(data.get("message"), dict)
    ):
        return data["message"]

    return data


def mutate_offer(
    path,
    mutator,
):
    data = json.loads(
        path.read_text()
    )

    payload = protocol_payload(data)

    mutator(payload)

    path.write_text(
        json.dumps(
            data,
            indent=2,
            sort_keys=True,
        )
        + "\n"
    )


# =====================================================================
# CASE 1
# Valid policy offer
# =====================================================================

def test_valid_offer():
    banner(
        "TIME MALLORY 1: VALID V1.5 OFFER"
    )

    swap_id, _ = publish_policy_offer()

    h.swap_cmd(
        "accept-offer",
        swap_id,
        role="bob",
    )

    bob = h.load_state(
        swap_id,
        "bob",
    )

    h.require(
        bob.get(
            "timeout_policy_version"
        ) == "1.5",
        "Bob did not persist v1.5 policy metadata",
    )

    h.require(
        h.message_file(
            swap_id,
            "accept",
        ).exists(),
        "valid offer produced no Bob acceptance",
    )

    h.ok(
        "valid v1.5 timeout offer accepted"
    )


# =====================================================================
# CASE 2
# Unknown policy version
# =====================================================================

def test_unknown_policy():
    banner(
        "TIME MALLORY 2: UNKNOWN POLICY"
    )

    swap_id, path = publish_policy_offer()

    mutate_offer(
        path,
        lambda x: x.__setitem__(
            "timeout_policy_version",
            "666",
        ),
    )

    require_rejected_cleanly(
        swap_id,
        contains=(
            "unsupported timeout policy version"
        ),
    )

    h.ok(
        "unknown timeout policy rejected before acceptance"
    )


# =====================================================================
# CASE 3
# Versionless offer / timeout-policy downgrade attempt
# =====================================================================

def test_versionless_offer():
    banner(
        "TIME MALLORY 3: VERSIONLESS TIMEOUT OFFER"
    )

    swap_id, path = publish_policy_offer()

    def strip_policy_metadata(message):
        for field in (
            "timeout_policy_version",
            "btc_timeout_window_seconds",
            "blake_timeout_window_seconds",
            "bob_recovery_seconds",
        ):
            message.pop(
                field,
                None,
            )

    mutate_offer(
        path,
        strip_policy_metadata,
    )

    require_rejected_cleanly(
        swap_id,
        contains=(
            "timeout policy version"
        ),
    )

    h.ok(
        "versionless timeout-policy downgrade rejected"
    )


# =====================================================================
# CASE 4
# Missing recovery metadata
# =====================================================================

def test_missing_recovery():
    banner(
        "TIME MALLORY 3: MISSING RECOVERY METADATA"
    )

    swap_id, path = publish_policy_offer()

    mutate_offer(
        path,
        lambda x: x.pop(
            "bob_recovery_seconds",
            None,
        ),
    )

    require_rejected_cleanly(
        swap_id,
        contains=(
            "missing bob_recovery_seconds"
        ),
    )

    h.ok(
        "missing recovery metadata rejected"
    )


# =====================================================================
# CASE 5
# Expired Bitcoin deadline
# =====================================================================

def test_expired_btc():
    banner(
        "TIME MALLORY 4: EXPIRED BITCOIN TIMEOUT"
    )

    swap_id, path = publish_policy_offer()

    btc_height = int(
        h.btc_cli(
            "getblockcount"
        )
    )

    mutate_offer(
        path,
        lambda x: x.__setitem__(
            "btc_timeout",
            btc_height,
        ),
    )

    require_rejected_cleanly(
        swap_id,
        contains=(
            "Bitcoin timeout is already expired"
        ),
    )

    h.ok(
        "expired Bitcoin timeout rejected"
    )


# =====================================================================
# CASE 6
# Expired BLAKE deadline
# =====================================================================

def test_expired_blake():
    banner(
        "TIME MALLORY 5: EXPIRED BLAKE TIMEOUT"
    )

    swap_id, path = publish_policy_offer()

    blake_height = int(
        h.blake_cli(
            "getblockcount"
        )
    )

    mutate_offer(
        path,
        lambda x: x.__setitem__(
            "blake_timeout",
            blake_height,
        ),
    )

    require_rejected_cleanly(
        swap_id,
        contains=(
            "BLAKE timeout is already expired"
        ),
    )

    h.ok(
        "expired BLAKE timeout rejected"
    )


# =====================================================================
# CASE 7
# Bitcoin deadline still future, but far too short
# =====================================================================

def test_short_btc_window():
    banner(
        "TIME MALLORY 6: BTC WINDOW TOO SHORT"
    )

    swap_id, path = publish_policy_offer()

    btc_height = int(
        h.btc_cli(
            "getblockcount"
        )
    )

    mutate_offer(
        path,
        lambda x: x.__setitem__(
            "btc_timeout",
            btc_height + 1,
        ),
    )

    require_rejected_cleanly(
        swap_id,
        contains=(
            "does not leave Bob's locally required "
            "BTC safety window"
        ),
    )

    h.ok(
        "future-but-too-short Bitcoin timeout rejected"
    )


# =====================================================================
# CASE 8
# Actual BLAKE recovery gap one block below Bob's requirement.
# =====================================================================

def test_short_recovery_gap():
    banner(
        "TIME MALLORY 7: RECOVERY GAP ONE BLOCK SHORT"
    )

    swap_id, path = publish_policy_offer()

    message = json.loads(
        path.read_text()
    )
    message = protocol_payload(message)

    policy = TimeoutPolicy()

    btc_height = int(
        h.btc_cli(
            "getblockcount"
        )
    )

    blake_height = int(
        h.blake_cli(
            "getblockcount"
        )
    )

    local_plan = build_timeout_plan(
        btc_height=btc_height,
        blake_height=blake_height,
        policy=policy,
    )

    btc_remaining = (
        int(message["btc_timeout"])
        - btc_height
    ) * policy.btc_block_seconds

    required_recovery = (
        local_plan.bob_recovery_seconds
    )

    #
    # Minimum BLAKE blocks needed to preserve the complete
    # recovery interval from Bob's present chain tips.
    #
    required_blake_blocks = math.ceil(
        (
            btc_remaining
            + required_recovery
        )
        / policy.blake_block_seconds
    )

    #
    # Mallory removes exactly one BLAKE block.
    #
    unsafe_blake_timeout = (
        blake_height
        + required_blake_blocks
        - 1
    )

    mutate_offer(
        path,
        lambda x: x.__setitem__(
            "blake_timeout",
            unsafe_blake_timeout,
        ),
    )

    require_rejected_cleanly(
        swap_id,
        contains=(
            "does not leave Bob's locally required "
            "cross-chain recovery window"
        ),
    )

    h.ok(
        "one-block-short cross-chain recovery gap rejected"
    )


# =====================================================================
# CASE 9
# Metadata claims an internally impossible safety relationship.
# Actual heights may still be safe; signed description is dishonest.
# =====================================================================

def test_dishonest_metadata():
    banner(
        "TIME MALLORY 8: DISHONEST POLICY METADATA"
    )

    swap_id, path = publish_policy_offer()

    message = json.loads(
        path.read_text()
    )
    message = protocol_payload(message)

    btc_window = int(
        message[
            "btc_timeout_window_seconds"
        ]
    )

    recovery = int(
        message[
            "bob_recovery_seconds"
        ]
    )

    #
    # Advertise one second less than the claimed recovery
    # relationship, while leaving the actual CLTVs untouched.
    #
    dishonest_blake_window = (
        btc_window
        + recovery
        - 1
    )

    mutate_offer(
        path,
        lambda x: x.__setitem__(
            "blake_timeout_window_seconds",
            dishonest_blake_window,
        ),
    )

    require_rejected_cleanly(
        swap_id,
        contains=(
            "advertised timeout metadata "
            "does not preserve its own "
            "recovery budget"
        ),
    )

    h.ok(
        "internally dishonest safety metadata rejected"
    )


def main():
    started = time.time()

    h.preflight()
    h.ok(
        "preflight"
    )

    tests = [
        test_valid_offer,
        test_unknown_policy,
        test_versionless_offer,
        test_missing_recovery,
        test_expired_btc,
        test_expired_blake,
        test_short_btc_window,
        test_short_recovery_gap,
        test_dishonest_metadata,
    ]

    failures = []

    for test in tests:
        try:
            test()

        except Exception as exc:
            failures.append(
                (
                    test.__name__,
                    exc,
                )
            )

            h.fail(
                test.__name__,
                str(exc),
            )

    print()
    print("=" * 72)
    print("TIME MALLORY SUMMARY")
    print("=" * 72)

    print(
        "passes:  ",
        h.PASS_COUNT,
    )

    print(
        "failures:",
        len(failures),
    )

    print(
        "elapsed: ",
        f"{time.time() - started:.1f}s",
    )

    if failures:
        print()

        for name, exc in failures:
            print(
                "FAILED:",
                name,
            )
            print(
                "       ",
                exc,
            )

        return 1

    print()
    print(
        "PASS: Bob rejected every unsafe v1.5 "
        "timeout proposal before durable acceptance."
    )

    return 0


if __name__ == "__main__":
    raise SystemExit(
        main()
    )
