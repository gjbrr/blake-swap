#!/usr/bin/env python3

from __future__ import annotations

from dataclasses import replace
import time

from swap_timeout_policy import (
    TimeoutPlan,
    describe_plan,
    TimeoutPolicy,
    alice_btc_window_seconds,
    bob_recovery_budget_seconds,
    build_timeout_plan,
    seconds_to_blocks,
    validate_timeout_plan,
)


PASS = 0
FAIL = 0


def ok(name):
    global PASS

    PASS += 1

    print(
        f"PASS  {name}"
    )


def fail(name, detail):
    global FAIL

    FAIL += 1

    print(
        f"FAIL  {name}"
    )

    print(
        f"      {detail}"
    )


def require(
    condition,
    message,
):
    if not condition:
        raise AssertionError(
            message
        )


def expect_reject(
    name,
    fn,
):
    try:
        fn()

    except (
        ValueError,
        RuntimeError,
    ):
        ok(
            name
        )

        return

    raise AssertionError(
        f"{name}: unsafe input was accepted"
    )


def banner(name):
    print()
    print(
        "=" * 72
    )

    print(
        name
    )

    print(
        "=" * 72
    )


def test_independent_heights():
    banner(
        "TIME MALLORY: INDEPENDENT CHAIN HEIGHTS"
    )

    policy = TimeoutPolicy()

    plan = build_timeout_plan(
        btc_height=900_000,
        blake_height=17,
        policy=policy,
    )

    validate_timeout_plan(
        plan,
        policy,
    )

    require(
        plan.btc_timeout_height
        > plan.btc_start_height,
        "Bitcoin timeout not in future",
    )

    require(
        plan.blake_timeout_height
        > plan.blake_start_height,
        "BLAKE timeout not in future",
    )

    ok(
        "safe plan works when Bitcoin height is vastly larger"
    )

    plan = build_timeout_plan(
        btc_height=17,
        blake_height=9_000_000,
        policy=policy,
    )

    validate_timeout_plan(
        plan,
        policy,
    )

    ok(
        "safe plan works when BLAKE height is vastly larger"
    )


def test_exact_boundary():
    banner(
        "TIME MALLORY: RECOVERY BOUNDARY"
    )

    policy = TimeoutPolicy()

    plan = build_timeout_plan(
        btc_height=1000,
        blake_height=2000,
        policy=policy,
    )

    required = (
        bob_recovery_budget_seconds(
            policy
        )
    )

    actual_gap = (
        plan.blake_window_seconds
        - plan.btc_window_seconds
    )

    require(
        actual_gap >= required,
        (
            "encoded block deadlines lost part "
            "of the required recovery margin"
        ),
    )

    validate_timeout_plan(
        plan,
        policy,
    )

    ok(
        "encoded block deadlines preserve full recovery margin"
    )

    unsafe = replace(
        plan,
        blake_window_seconds=(
            plan.btc_window_seconds
            + required
            - 1
        ),
    )

    expect_reject(
        "one second below recovery boundary rejected",
        lambda: validate_timeout_plan(
            unsafe,
            policy,
        ),
    )


def test_expired_deadlines():
    banner(
        "TIME MALLORY: EXPIRED DEADLINES"
    )

    policy = TimeoutPolicy()

    plan = build_timeout_plan(
        btc_height=1000,
        blake_height=2000,
        policy=policy,
    )

    bad_btc = replace(
        plan,
        btc_timeout_height=(
            plan.btc_start_height
        ),
    )

    expect_reject(
        "already-expired Bitcoin deadline rejected",
        lambda: validate_timeout_plan(
            bad_btc,
            policy,
        ),
    )

    bad_blake = replace(
        plan,
        blake_timeout_height=(
            plan.blake_start_height
        ),
    )

    expect_reject(
        "already-expired BLAKE deadline rejected",
        lambda: validate_timeout_plan(
            bad_blake,
            policy,
        ),
    )


def test_defaults_and_height_bounds():
    banner(
        "TIMEOUT PLAN DEFAULTS AND BOUNDS"
    )

    # No policy supplied: the defaults must produce a valid plan.
    default_plan = build_timeout_plan(
        btc_height=100,
        blake_height=200,
    )

    require(
        default_plan.btc_blocks > 0,
        "the default plan has no Bitcoin window",
    )

    require(
        default_plan.blake_blocks > 0,
        "the default plan has no BLAKE window",
    )

    ok("a default plan is built and valid")

    for name, kwargs in (
        (
            "a negative Bitcoin height",
            {"btc_height": -1, "blake_height": 10},
        ),
        (
            "a negative BLAKE height",
            {"btc_height": 10, "blake_height": -1},
        ),
    ):
        expect_reject(
            name + " is rejected",
            lambda kwargs=kwargs: build_timeout_plan(**kwargs),
        )

    # describe_plan is the operator-facing rendering; it must describe
    # the plan it was given rather than a generic shape.
    described = describe_plan(default_plan)

    for expected in (
        "TIMEOUT SAFETY PLAN",
        str(default_plan.btc_start_height),
        str(default_plan.btc_timeout_height),
        str(default_plan.blake_start_height),
        str(default_plan.blake_timeout_height),
    ):
        require(
            expected in described,
            f"describe_plan omitted {expected!r}: {described}",
        )

    ok("describe_plan renders the plan's own heights")


def test_bad_policy():
    banner(
        "TIME MALLORY: MALFORMED POLICY"
    )

    expect_reject(
        "zero Bitcoin block interval rejected",
        lambda: build_timeout_plan(
            btc_height=1,
            blake_height=1,
            policy=TimeoutPolicy(
                btc_block_seconds=0,
            ),
        ),
    )

    expect_reject(
        "zero BLAKE block interval rejected",
        lambda: build_timeout_plan(
            btc_height=1,
            blake_height=1,
            policy=TimeoutPolicy(
                blake_block_seconds=0,
            ),
        ),
    )

    expect_reject(
        "zero delay multiplier rejected",
        lambda: build_timeout_plan(
            btc_height=1,
            blake_height=1,
            policy=TimeoutPolicy(
                block_delay_multiplier=0,
            ),
        ),
    )

    expect_reject(
        "zero Bitcoin confirmations rejected",
        lambda: build_timeout_plan(
            btc_height=1,
            blake_height=1,
            policy=TimeoutPolicy(
                btc_claim_confirmations=0,
            ),
        ),
    )


def test_slow_chain_expands_safety():
    banner(
        "TIME MALLORY: SLOW CHAINS"
    )

    normal = TimeoutPolicy()

    slow_btc = TimeoutPolicy(
        btc_block_seconds=1800,
    )

    normal_plan = build_timeout_plan(
        btc_height=1000,
        blake_height=1000,
        policy=normal,
    )

    slow_btc_plan = build_timeout_plan(
        btc_height=1000,
        blake_height=1000,
        policy=slow_btc,
    )

    require(
        slow_btc_plan.btc_window_seconds
        > normal_plan.btc_window_seconds,
        "slow Bitcoin did not expand wall-clock BTC window",
    )

    require(
        slow_btc_plan.bob_recovery_seconds
        > normal_plan.bob_recovery_seconds,
        "slow Bitcoin did not expand Bob recovery budget",
    )

    ok(
        "slow Bitcoin expands safety budgets"
    )

    slow_blake = TimeoutPolicy(
        blake_block_seconds=1800,
    )

    slow_blake_plan = build_timeout_plan(
        btc_height=1000,
        blake_height=1000,
        policy=slow_blake,
    )

    require(
        slow_blake_plan.bob_recovery_seconds
        > normal_plan.bob_recovery_seconds,
        "slow BLAKE did not expand Bob recovery budget",
    )

    ok(
        "slow BLAKE expands Bob recovery budget"
    )


def test_minimum_recovery_floor():
    banner(
        "TIME MALLORY: ABSOLUTE SAFETY FLOOR"
    )

    policy = TimeoutPolicy(
        btc_block_seconds=60,
        blake_block_seconds=60,
        observation_seconds=1,
        restart_seconds=1,
        broadcast_seconds=1,
        btc_claim_confirmations=1,
        blake_claim_confirmations=1,
        cross_chain_safety_seconds=1,
        minimum_recovery_seconds=6 * 3600,
    )

    recovery = (
        bob_recovery_budget_seconds(
            policy
        )
    )

    require(
        recovery == 6 * 3600,
        "minimum recovery floor was not enforced",
    )

    ok(
        "absolute recovery floor overrides optimistic chain assumptions"
    )


def test_rounding():
    banner(
        "TIME MALLORY: BLOCK ROUNDING"
    )

    require(
        seconds_to_blocks(
            601,
            600,
        )
        == 2,
        "601 seconds incorrectly rounded down",
    )

    ok(
        "timeout conversion always rounds upward"
    )


def main():
    started = time.time()

    tests = [
        test_independent_heights,
        test_defaults_and_height_bounds,
        test_exact_boundary,
        test_expired_deadlines,
        test_bad_policy,
        test_slow_chain_expands_safety,
        test_minimum_recovery_floor,
        test_rounding,
    ]

    for test in tests:

        try:
            test()

        except Exception as e:
            fail(
                test.__name__,
                str(e),
            )

    banner(
        "TIMEOUT POLICY SUMMARY"
    )

    print(
        f"passes:   {PASS}"
    )

    print(
        f"failures: {FAIL}"
    )

    print(
        f"elapsed:  {time.time() - started:.2f}s"
    )

    return (
        1
        if FAIL
        else 0
    )


if __name__ == "__main__":
    raise SystemExit(
        main()
    )
