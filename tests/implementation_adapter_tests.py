#!/usr/bin/env python3

import inspect

import swap_protocol_tests as h

from swap_impl_adapter import (
    PARTICIPANT_ADAPTER_SURFACE,
    PythonParticipantAdapter,
    SwapImplementationPair,
    make_python_reference_pair,
)


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def check_participant(
    participant,
    expected_role,
):
    require(
        participant.role
        == expected_role,
        (
            f"participant role mismatch: "
            f"{participant.role}"
        ),
    )

    for method in (
        PARTICIPANT_ADAPTER_SURFACE
    ):
        require(
            callable(
                getattr(
                    participant,
                    method,
                    None,
                )
            ),
            (
                f"{expected_role} adapter "
                f"missing {method}"
            ),
        )

    sig = inspect.signature(
        participant.command
    )

    #
    # A role-specific participant command MUST NOT need another role
    # argument. Its identity is already pinned by the adapter.
    #
    require(
        "role" not in sig.parameters,
        (
            f"{expected_role} participant "
            "command unexpectedly accepts role"
        ),
    )


def main():
    alice = PythonParticipantAdapter(
        h,
        "alice",
    )

    bob = PythonParticipantAdapter(
        h,
        "bob",
    )

    check_participant(
        alice,
        "alice",
    )

    check_participant(
        bob,
        "bob",
    )

    require(
        alice is not bob,
        "Alice and Bob adapters are not independent objects",
    )

    pair = SwapImplementationPair(
        h,
        alice,
        bob,
    )

    require(
        pair.alice is alice,
        "pair did not retain Alice adapter",
    )

    require(
        pair.bob is bob,
        "pair did not retain Bob adapter",
    )

    default_pair = (
        make_python_reference_pair(h)
    )

    require(
        default_pair.alice.role
        == "alice",
        "default pair Alice role wrong",
    )

    require(
        default_pair.bob.role
        == "bob",
        "default pair Bob role wrong",
    )

    print(
        "PASS  Alice and Bob have independent "
        "participant adapters"
    )

    print(
        "PASS  participant commands are role-pinned"
    )

    print(
        "PASS  pair accepts independently supplied "
        "participant implementations"
    )

    print(
        "PASS  regtest chain control remains outside "
        "participant implementations"
    )

    print()
    print("ROLE-SPLIT ADAPTER")
    print("==================")
    print(
        "Alice:",
        default_pair.alice.name,
    )
    print(
        "Bob:  ",
        default_pair.bob.name,
    )


if __name__ == "__main__":
    main()
