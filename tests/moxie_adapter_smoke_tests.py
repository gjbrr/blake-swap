#!/usr/bin/env python3

import argparse
import json
import subprocess
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]

FIXTURE = (
    ROOT
    / "tests"
    / "moxie_adapter_fixture.json"
)


def require(
    condition,
    message,
):
    if not condition:
        raise AssertionError(
            message
        )


def load_fixture():
    return json.loads(
        FIXTURE.read_text()
    )


def validate_fixture(data):
    require(
        data.get("protocol")
        == "blake-swap-adapter-v1",
        "unexpected adapter protocol",
    )

    require(
        set(
            data.get(
                "required_operations",
                [],
            )
        )
        == {
            "capabilities",
            "command",
            "state",
        },
        "fixture operation set is wrong",
    )

    require(
        set(
            data.get(
                "required_roles",
                [],
            )
        )
        == {
            "alice",
            "bob",
        },
        "fixture role set is wrong",
    )

    fields = set(
        data.get(
            "portable_state_fields_used_by_seeded_corpus",
            [],
        )
    )

    for field in (
        "state",
        "secret_hex",
        "revealed_secret_hex",
        "blake_funding_txid",
        "btc_funding_txid",
        "btc_claim_txid",
        "btc_refund_txid",
        "blake_claim_txid",
        "blake_refund_txid",
    ):
        require(
            field in fields,
            (
                "fixture missing portable "
                f"state field: {field}"
            ),
        )


def query_executable(
    executable,
    request,
):
    p = subprocess.run(
        [executable],
        input=(
            json.dumps(
                request,
                sort_keys=True,
            )
            + "\n"
        ),
        text=True,
        capture_output=True,
    )

    require(
        p.returncode == 0,
        (
            "adapter executable itself exited "
            f"{p.returncode}\n"
            f"stdout:\n{p.stdout}\n"
            f"stderr:\n{p.stderr}"
        ),
    )

    try:
        return json.loads(
            p.stdout
        )
    except json.JSONDecodeError as e:
        raise AssertionError(
            (
                "adapter executable returned "
                "non-JSON stdout:\n"
                + p.stdout
            )
        ) from e


def validate_capabilities(
    fixture,
    response,
):
    require(
        response.get("ok") is True,
        "capabilities request was refused",
    )

    require(
        response.get("protocol")
        == fixture["protocol"],
        "adapter protocol version mismatch",
    )

    operations = set(
        response.get(
            "operations",
            [],
        )
    )

    for operation in (
        fixture[
            "required_operations"
        ]
    ):
        require(
            operation in operations,
            (
                "adapter missing operation: "
                + operation
            ),
        )

    roles = set(
        response.get(
            "roles",
            [],
        )
    )

    for role in (
        fixture[
            "required_roles"
        ]
    ):
        require(
            role in roles,
            (
                "adapter missing role: "
                + role
            ),
        )


def main():
    parser = argparse.ArgumentParser()

    parser.add_argument(
        "--executable",
        default=None,
        help=(
            "Adapter executable to test. "
            "If omitted, only the fixture "
            "contract is validated."
        ),
    )

    args = parser.parse_args()

    fixture = load_fixture()

    validate_fixture(
        fixture
    )

    print(
        "PASS  adapter fixture schema"
    )

    print(
        "PASS  portable seeded-corpus "
        "state fields declared"
    )

    target = fixture[
        "first_cross_language_target"
    ]

    require(
        target["seed"] == 4,
        "unexpected first integration seed",
    )

    require(
        target[
            "expected_portable_digest_prefix"
        ]
        == "e22e03aa7289b52e",
        "seed-4 oracle drifted",
    )

    print(
        "PASS  first Python/Moxie "
        "target pins seed 4 oracle"
    )

    if args.executable:
        response = query_executable(
            args.executable,
            fixture[
                "capabilities_request"
            ],
        )

        validate_capabilities(
            fixture,
            response,
        )

        print(
            "PASS  executable speaks "
            "blake-swap-adapter-v1"
        )

    print()
    print("MOXIE ADAPTER HANDOFF")
    print("=====================")
    print(
        "protocol:",
        fixture["protocol"],
    )

    print(
        "first target: Python Alice / "
        "Moxie Bob / seed 4"
    )


if __name__ == "__main__":
    main()
