#!/usr/bin/env python3

from swap_v2 import Swap, State


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def test_cross_chain_timeout_heights_are_not_ordered():
    swap = Swap(
        swap_id="0123456789ab",
        state=State.OFFERED.value,
        created_at=1,

        blake_amount=0.0001,
        btc_sats=2730,

        secret_hex=None,
        secret_hash="11" * 32,

        # Deliberately incomparable absolute heights:
        # BLAKE is numerically far behind Bitcoin.
        blake_timeout=100,
        btc_timeout=900_000,

        timeout_policy_version="1.5",
        btc_timeout_window_seconds=12_600,
        blake_timeout_window_seconds=27_600,
        bob_recovery_seconds=14_700,
    )

    swap.validate()

    require(
        swap.blake_timeout < swap.btc_timeout,
        "test fixture did not preserve intentionally inverted raw heights",
    )

    print(
        "PASS: Swap.validate does not compare BTC and BLAKE "
        "timeout heights as one shared number line"
    )


def main():
    test_cross_chain_timeout_heights_are_not_ordered()

    print()
    print(
        "PASS: swap model timeout invariant regression suite"
    )


if __name__ == "__main__":
    main()
