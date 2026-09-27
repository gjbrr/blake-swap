#!/usr/bin/env python3

from __future__ import annotations

from dataclasses import dataclass
from math import ceil


@dataclass(frozen=True)
class TimeoutPolicy:
    #
    # Expected/nominal chain intervals used to convert
    # wall-clock safety budgets into CLTV block heights.
    #
    btc_block_seconds: int = 600
    blake_block_seconds: int = 600

    #
    # We deliberately do NOT assume blocks arrive exactly
    # at their nominal interval.
    #
    block_delay_multiplier: float = 2.0

    #
    # Operational delays.
    #
    observation_seconds: int = 15 * 60
    restart_seconds: int = 15 * 60
    broadcast_seconds: int = 15 * 60

    #
    # Confirmation requirements used by protocol policy.
    #
    btc_claim_confirmations: int = 2
    blake_claim_confirmations: int = 2

    #
    # Extra time Bob must retain after Alice can still
    # successfully claim Bitcoin.
    #
    cross_chain_safety_seconds: int = 2 * 60 * 60

    #
    # Absolute minimum permitted recovery gap.
    #
    minimum_recovery_seconds: int = 4 * 60 * 60


@dataclass(frozen=True)
class TimeoutPlan:
    btc_start_height: int
    blake_start_height: int

    btc_timeout_height: int
    blake_timeout_height: int

    btc_window_seconds: int
    blake_window_seconds: int

    bob_recovery_seconds: int

    btc_blocks: int
    blake_blocks: int


def require_positive(
    name: str,
    value: int | float,
) -> None:
    if value <= 0:
        raise ValueError(
            f"{name} must be positive"
        )


def delayed_block_seconds(
    nominal_seconds: int,
    multiplier: float,
) -> int:
    require_positive(
        "nominal block interval",
        nominal_seconds,
    )

    require_positive(
        "block delay multiplier",
        multiplier,
    )

    return ceil(
        nominal_seconds
        * multiplier
    )


def confirmations_budget_seconds(
    confirmations: int,
    nominal_block_seconds: int,
    multiplier: float,
) -> int:
    if confirmations < 1:
        raise ValueError(
            "confirmations must be at least 1"
        )

    return (
        confirmations
        * delayed_block_seconds(
            nominal_block_seconds,
            multiplier,
        )
    )


def bob_recovery_budget_seconds(
    policy: TimeoutPolicy,
) -> int:
    """
    Worst-case policy budget after Alice's Bitcoin claim
    becomes relevant to Bob.

    Bob may need to:
      observe/restart,
      obtain the Bitcoin confirmations required by policy,
      recover S,
      broadcast his BLAKE claim,
      obtain BLAKE confirmations,
      retain an additional cross-chain safety margin.
    """

    btc_confirmation_budget = (
        confirmations_budget_seconds(
            policy.btc_claim_confirmations,
            policy.btc_block_seconds,
            policy.block_delay_multiplier,
        )
    )

    blake_confirmation_budget = (
        confirmations_budget_seconds(
            policy.blake_claim_confirmations,
            policy.blake_block_seconds,
            policy.block_delay_multiplier,
        )
    )

    calculated = (
        policy.observation_seconds
        + policy.restart_seconds
        + btc_confirmation_budget
        + policy.broadcast_seconds
        + blake_confirmation_budget
        + policy.cross_chain_safety_seconds
    )

    return max(
        calculated,
        policy.minimum_recovery_seconds,
    )


def alice_btc_window_seconds(
    policy: TimeoutPolicy,
) -> int:
    """
    Time budget after Bob funds Bitcoin during which Alice
    can notice the funding and safely get her claim confirmed.
    """

    confirmation_budget = (
        confirmations_budget_seconds(
            policy.btc_claim_confirmations,
            policy.btc_block_seconds,
            policy.block_delay_multiplier,
        )
    )

    return (
        policy.observation_seconds
        + policy.restart_seconds
        + policy.broadcast_seconds
        + confirmation_budget
        + policy.cross_chain_safety_seconds
    )


def seconds_to_blocks(
    seconds: int,
    nominal_block_seconds: int,
) -> int:
    require_positive(
        "seconds",
        seconds,
    )

    require_positive(
        "nominal block interval",
        nominal_block_seconds,
    )

    return max(
        1,
        ceil(
            seconds
            / nominal_block_seconds
        ),
    )


def build_timeout_plan(
    *,
    btc_height: int,
    blake_height: int,
    policy: TimeoutPolicy | None = None,
) -> TimeoutPlan:

    if policy is None:
        policy = TimeoutPolicy()

    if btc_height < 0:
        raise ValueError(
            "Bitcoin height cannot be negative"
        )

    if blake_height < 0:
        raise ValueError(
            "BLAKE height cannot be negative"
        )

    desired_btc_window = (
        alice_btc_window_seconds(
            policy
        )
    )

    recovery_window = (
        bob_recovery_budget_seconds(
            policy
        )
    )

    #
    # First encode Alice's Bitcoin safety window into an
    # actual integer number of Bitcoin blocks.
    #
    # This rounding matters. The BLAKE deadline must be
    # constructed relative to the ENCODED Bitcoin window,
    # not merely the unrounded ideal duration.
    #
    btc_blocks = seconds_to_blocks(
        desired_btc_window,
        policy.btc_block_seconds,
    )

    encoded_btc_window = (
        btc_blocks
        * policy.btc_block_seconds
    )

    #
    # Only now add Bob's full recovery budget.
    #
    required_blake_window = (
        encoded_btc_window
        + recovery_window
    )

    blake_blocks = seconds_to_blocks(
        required_blake_window,
        policy.blake_block_seconds,
    )

    encoded_blake_window = (
        blake_blocks
        * policy.blake_block_seconds
    )

    #
    # Persist the windows represented by the actual CLTV
    # block counts. Therefore:
    #
    #     encoded_blake_window
    #       - encoded_btc_window
    #       >= recovery_window
    #
    # even after integer block rounding.
    #
    return TimeoutPlan(
        btc_start_height=btc_height,
        blake_start_height=blake_height,

        btc_timeout_height=(
            btc_height
            + btc_blocks
        ),

        blake_timeout_height=(
            blake_height
            + blake_blocks
        ),

        btc_window_seconds=(
            encoded_btc_window
        ),

        blake_window_seconds=(
            encoded_blake_window
        ),

        bob_recovery_seconds=(
            recovery_window
        ),

        btc_blocks=btc_blocks,
        blake_blocks=blake_blocks,
    )

def validate_timeout_plan(
    plan: TimeoutPlan,
    policy: TimeoutPolicy,
) -> None:
    """
    Validate the economic relationship in TIME.

    Notice what we deliberately do not do:

        blake_timeout_height > btc_timeout_height

    Those heights belong to different blockchains and therefore
    have no useful ordering relationship.
    """

    if (
        plan.btc_timeout_height
        <= plan.btc_start_height
    ):
        raise ValueError(
            "Bitcoin timeout is already expired "
            "or has no future window"
        )

    if (
        plan.blake_timeout_height
        <= plan.blake_start_height
    ):
        raise ValueError(
            "BLAKE timeout is already expired "
            "or has no future window"
        )

    required_recovery = (
        bob_recovery_budget_seconds(
            policy
        )
    )

    actual_recovery = (
        plan.blake_window_seconds
        - plan.btc_window_seconds
    )

    if actual_recovery < required_recovery:
        raise ValueError(
            "Unsafe timeout plan: Bob's post-Bitcoin "
            "recovery window is too short"
        )

    #
    # Defence in depth. This is currently unreachable, because
    # bob_recovery_budget_seconds() already returns
    # max(calculated, policy.minimum_recovery_seconds), so passing the
    # check above implies passing this one. It is kept deliberately: it
    # restates the absolute floor here, where the plan is accepted, so
    # that a future change to the budget helper cannot quietly drop it.
    #
    if (
        actual_recovery
        < policy.minimum_recovery_seconds
    ):
        raise ValueError(
            "Unsafe timeout plan: below absolute "
            "minimum recovery window"
        )


def describe_plan(
    plan: TimeoutPlan,
) -> str:
    return "\n".join(
        [
            "TIMEOUT SAFETY PLAN",
            "===================",
            (
                "Bitcoin: "
                f"{plan.btc_start_height} -> "
                f"{plan.btc_timeout_height} "
                f"({plan.btc_blocks} blocks)"
            ),
            (
                "BLAKE:   "
                f"{plan.blake_start_height} -> "
                f"{plan.blake_timeout_height} "
                f"({plan.blake_blocks} blocks)"
            ),
            "",
            (
                "Alice BTC window: "
                f"{plan.btc_window_seconds / 3600:.2f} h"
            ),
            (
                "Bob recovery gap: "
                f"{plan.bob_recovery_seconds / 3600:.2f} h"
            ),
            (
                "BLAKE total window: "
                f"{plan.blake_window_seconds / 3600:.2f} h"
            ),
        ]
    )


if __name__ == "__main__":
    policy = TimeoutPolicy()

    plan = build_timeout_plan(
        btc_height=100_000,
        blake_height=500_000,
        policy=policy,
    )

    validate_timeout_plan(
        plan,
        policy,
    )

    print(
        describe_plan(
            plan
        )
    )
