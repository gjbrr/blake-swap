#!/usr/bin/env python3

import json
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
TESTS = ROOT / "tests"
MANIFEST = TESTS / "security_conformance.json"


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def main():
    data = json.loads(
        MANIFEST.read_text()
    )

    require(
        data.get("schema")
        == "blake-swap-security-conformance-v1",
        "wrong manifest schema",
    )

    records = data.get("invariants", [])

    require(
        len(records) == 47,
        f"expected 47 invariants, got {len(records)}",
    )

    ids = [r["id"] for r in records]

    require(
        ids == [f"C{i}" for i in range(1, 48)],
        "C1-C47 are missing, duplicated, or out of order",
    )

    for record in records:
        cid = record["id"]

        require(
            bool(record.get("requirement")),
            f"{cid}: empty requirement",
        )

        suites = record.get("proving_suites", [])

        require(
            suites,
            f"{cid}: has no proving suite",
        )

        for suite in suites:
            path = TESTS / suite

            require(
                path.exists(),
                f"{cid}: proving suite does not exist: {suite}",
            )

        surface = set(
            record.get(
                "comparison_surface",
                [],
            )
        )

        require(
            "durable_state" in surface,
            f"{cid}: missing durable-state comparison surface",
        )

        require(
            "accepted_or_rejected" in surface,
            f"{cid}: missing accept/reject comparison surface",
        )

    print("PASS  manifest contains exactly C1-C47")
    print("PASS  every invariant has literal contract text")
    print("PASS  every invariant maps to existing proving suites")
    print("PASS  portable comparison surface is declared")
    print()
    print("SECURITY CONFORMANCE")
    print("====================")
    print("invariants: 47")
    print("status:     machine-readable")


if __name__ == "__main__":
    main()
