#!/usr/bin/env python3

import json
import re
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
DOC = ROOT / "docs" / "MOXIE_SECURITY_CONFORMANCE.md"
OUT = ROOT / "tests" / "security_conformance.json"


#
# File-level proofs deliberately describe portable observable coverage,
# not Python implementation details.
#
SUITES = {
    # Secret ownership / protocol roles.
    1: ["swap_protocol_tests.py", "swap_participant_isolation_tests.py"],
    2: ["swap_protocol_tests.py", "swap_chain_autonomy_tests.py"],
    3: ["swap_protocol_tests.py", "swap_participant_isolation_tests.py"],

    # State machine / production transition authority.
    4: ["swap_protocol_tests.py", "agent_policy_tests.py"],
    5: ["swap_protocol_tests.py", "driver_policy_tests.py"],

    # Funding authority / exact anchors / role safety.
    6: ["swap_protocol_tests.py", "production_funding_pending_tests.py"],
    7: ["swap_protocol_tests.py", "production_funding_pending_tests.py"],
    8: ["swap_protocol_tests.py", "driver_policy_tests.py"],

    # Settlement verification / identities.
    9: ["swap_protocol_tests.py", "swap_settlement_recovery_tests.py"],
    10: ["swap_protocol_tests.py", "schema_equality_tests.py"],

    # HTLC spend classification.
    11: ["swap_protocol_tests.py"],
    12: ["swap_protocol_tests.py"],
    13: ["swap_protocol_tests.py"],

    # Duplicate/conflicting replay.
    14: ["swap_protocol_tests.py"],
    15: ["swap_protocol_tests.py"],
    16: ["swap_protocol_tests.py"],
    17: ["swap_protocol_tests.py"],

    # Swap/message binding.
    18: ["swap_protocol_tests.py"],
    19: ["swap_protocol_tests.py"],
    20: ["swap_protocol_tests.py"],
    21: ["swap_protocol_tests.py"],
    22: ["swap_protocol_tests.py"],

    # Transport schema / secret exclusion / internal annotations.
    23: ["swap_wire_tests.py", "schema_equality_tests.py"],
    24: ["swap_wire_tests.py", "schema_equality_tests.py"],
    25: ["schema_equality_tests.py"],

    # Participant authentication is not chain authority.
    26: ["swap_protocol_tests.py", "swap_participant_isolation_tests.py"],
    27: ["swap_protocol_tests.py", "swap_encrypted_coordination_tests.py"],
    28: ["swap_protocol_tests.py", "swap_chain_autonomy_tests.py"],

    # Pending confirmation.
    29: ["production_funding_pending_tests.py"],
    30: ["production_settlement_pending_tests.py", "swap_protocol_tests.py"],
    31: ["production_settlement_pending_tests.py", "swap_protocol_tests.py"],

    # Stale/reorg safety.
    32: ["swap_reorg_safety_tests.py", "swap_protocol_tests.py"],
    33: ["swap_reorg_safety_tests.py"],
    34: ["swap_reorg_safety_tests.py"],
    35: ["swap_reorg_safety_tests.py"],
    36: ["swap_reorg_safety_tests.py"],

    # Crash/retry/mempool loss / exact durable transaction.
    37: [
        "swap_settlement_recovery_tests.py",
        "production_funding_pending_tests.py",
    ],
    38: [
        "swap_settlement_recovery_tests.py",
        "production_settlement_pending_tests.py",
        "swap_settlement_race_tests.py",
    ],
    39: [
        "swap_settlement_recovery_tests.py",
        "production_settlement_pending_tests.py",
        "swap_settlement_crash_matrix_tests.py",
        "swap_settlement_race_tests.py",
    ],

    # Timeout policy.
    40: ["swap_timeout_policy_tests.py", "swap_timeout_negotiation_tests.py"],
    41: ["swap_timeout_policy_tests.py", "swap_model_timeout_invariant_tests.py"],
    42: ["swap_timeout_negotiation_tests.py", "pre_btc_refund_tests.py"],
    43: ["swap_timeout_policy_tests.py", "pre_btc_refund_tests.py"],

    # Recovery / reconciliation.
    44: [
        "swap_peer_recovery_tests.py",
        "swap_chain_autonomy_tests.py",
        "swap_settlement_recovery_tests.py",
    ],
    45: [
        "swap_peer_recovery_tests.py",
        "swap_bounded_scan_tests.py",
        "swap_settlement_recovery_tests.py",
    ],
    46: [
        "swap_chain_autonomy_tests.py",
        "swap_peer_recovery_tests.py",
        "swap_settlement_recovery_tests.py",
    ],

    # COMPLETE requires active final BLAKE proof.
    47: ["swap_reorg_safety_tests.py", "swap_protocol_tests.py"],
}


def extract_requirements(text):
    """
    Extract the literal C1..C47 contract descriptions from the Markdown.

    Accepts headings/list forms such as:
        ### C1 — ...
        C1: ...
        - **C1** ...
        1. C1 ...
    """
    found = {}

    pattern = re.compile(
        r"""
        ^\s*
        (?:\#{1,6}\s*)?
        (?:[-*]\s*)?
        (?:\d+\.\s*)?
        \**(C(?:[1-9]|[1-3][0-9]|4[0-7]))\**
        (?:\s*[:.\-—–]\s*|\s+)
        (.+?)
        \s*$
        """,
        re.VERBOSE,
    )

    for line in text.splitlines():
        m = pattern.match(line)

        if not m:
            continue

        cid = m.group(1)
        requirement = m.group(2).strip().strip("*").strip()

        #
        # Ignore accidental prose references such as "C1 through C47".
        #
        if len(requirement) < 4:
            continue

        found.setdefault(cid, requirement)

    return found


def main():
    text = DOC.read_text()
    requirements = extract_requirements(text)

    expected = [f"C{i}" for i in range(1, 48)]
    missing = [cid for cid in expected if cid not in requirements]

    if missing:
        print("Could not extract all C1-C47 entries.")
        print("Missing:", ", ".join(missing))
        print()
        print("Lines containing C-identifiers:")
        for line in text.splitlines():
            if re.search(r"\bC(?:[1-9]|[1-3][0-9]|4[0-7])\b", line):
                print(line)
        raise SystemExit(1)

    records = []

    for number in range(1, 48):
        cid = f"C{number}"

        records.append({
            "id": cid,
            "requirement": requirements[cid],
            "proving_suites": SUITES[number],
            "comparison_surface": [
                "durable_state",
                "durable_economic_identities",
                "secret_knowledge",
                "accepted_or_rejected",
                "economic_transactions",
            ],
        })

    manifest = {
        "schema": "blake-swap-security-conformance-v1",
        "source": "docs/MOXIE_SECURITY_CONFORMANCE.md",
        "invariant_count": len(records),
        "implementation_policy": {
            "python": "reference security oracle",
            "moxie": "must match portable observable behavior",
            "ignored_differences": [
                "function names",
                "error text",
                "process model",
                "locking mechanism",
                "state file layout",
                "internal concurrency mechanism",
            ],
        },
        "invariants": records,
    }

    OUT.write_text(
        json.dumps(
            manifest,
            indent=2,
            sort_keys=False,
        )
        + "\n"
    )

    print(f"wrote {OUT}")
    print(f"invariants: {len(records)}")


if __name__ == "__main__":
    main()
