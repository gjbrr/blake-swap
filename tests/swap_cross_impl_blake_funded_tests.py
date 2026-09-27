#!/usr/bin/env python3

from __future__ import annotations

import os
import subprocess
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SRC = ROOT / "src"
TOOLS = ROOT / "tools"
TESTS = ROOT / "tests"

sys.path.insert(0, str(SRC))
sys.path.insert(0, str(TESTS))

import swap_protocol_tests as h


PASS = 0
FAIL = 0


def ok(name):
    global PASS
    PASS += 1
    print("PASS ", name)


def fail(name, detail):
    global FAIL
    FAIL += 1
    print("FAIL ", name)
    print("      ", detail)


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def main():
    h.preflight()
    ok("preflight")

    #
    # Create a real v1.5 swap and complete negotiation
    # with the reference implementation.
    #
    p = h.swap_cmd(
        "new",
        "--blake",
        "10",
        "--sats",
        "25000",
        role="alice",
    )

    swap_id = h.parse_swap_id(
        p.stdout
    )

    h.swap_cmd(
        "publish-offer",
        swap_id,
        role="alice",
    )

    h.swap_cmd(
        "accept-offer",
        swap_id,
        role="bob",
    )

    h.swap_cmd(
        "receive-accept",
        swap_id,
        role="alice",
    )

    ok("reference negotiation completed")

    #
    # Alice performs the REAL economic action:
    # fund the BLAKE HTLC.
    #
    # Critically, DO NOT call publish-blake-funded.
    #
    h.swap_cmd(
        "fund-blake",
        swap_id,
        role="alice",
    )

    alice = h.load_state(
        swap_id,
        "alice",
    )

    require(
        alice.get("blake_funding_txid"),
        "Alice funding produced no txid",
    )

    require(
        alice.get("blake_funding_vout")
        is not None,
        "Alice funding produced no vout",
    )

    ok("real BLAKE HTLC funded")

    #
    # Ensure the reference implementation has not
    # produced the announcement we're about to replace.
    #
    msg_path = h.message_file(
        swap_id,
        "blake-funded",
    )

    if msg_path.exists():
        msg_path.unlink()

    #
    # Independent implementation creates ONLY the wire
    # announcement.
    #
    cmd = [
        sys.executable,
        str(
            TOOLS
            / "minimal_blake_funded.py"
        ),
        "--swap-id",
        swap_id,
        "--secret-hash",
        alice["secret_hash"],
        "--blake-amount",
        str(alice["blake_amount"]),
        "--blake-timeout",
        str(alice["blake_timeout"]),
        "--txid",
        alice["blake_funding_txid"],
        "--vout",
        str(alice["blake_funding_vout"]),
    ]

    env = h.tool_env()

    result = subprocess.run(
        cmd,
        text=True,
        capture_output=True,
        env=env,
    )

    if result.returncode != 0:
        raise RuntimeError(
            result.stderr
            or result.stdout
        )

    require(
        msg_path.exists(),
        "independent producer created no blake-funded message",
    )

    ok(
        "independent implementation wrote blake-funded"
    )

    #
    # Bob's reference engine receives the independent
    # announcement.
    #
    # Bob must NOT trust it blindly: command_receive_blake_funded
    # independently verifies the actual BLAKE HTLC before state
    # advances.
    #
    h.swap_cmd(
        "receive-blake-funded",
        swap_id,
        role="bob",
    )

    bob = h.load_state(
        swap_id,
        "bob",
    )

    require(
        bob["state"] == "BLAKE_FUNDED",
        (
            "Bob did not advance to BLAKE_FUNDED "
            "after independent chain verification"
        ),
    )

    require(
        bob["blake_funding_txid"]
        == alice["blake_funding_txid"],
        "Bob recorded different BLAKE funding txid",
    )

    require(
        int(bob["blake_funding_vout"])
        == int(alice["blake_funding_vout"]),
        "Bob recorded different BLAKE funding vout",
    )

    ok(
        "Bob accepted independent funding announcement after chain verification"
    )

    #
    # Prove Bob can now continue economically.
    #
    h.swap_cmd(
        "fund-btc",
        swap_id,
        role="bob",
    )

    bob = h.load_state(
        swap_id,
        "bob",
    )

    require(
        bob["state"] == "BTC_FUNDED",
        "Bob could not continue to Bitcoin funding",
    )

    ok(
        "cross-implementation settlement message preserved protocol progress"
    )

    print()
    print("=" * 72)
    print("CROSS IMPLEMENTATION BLAKE FUNDING SUMMARY")
    print("=" * 72)
    print("passes:  ", PASS)
    print("failures:", FAIL)
    print("swap:    ", swap_id)

    if FAIL:
        raise SystemExit(1)


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:
        fail(
            "cross implementation BLAKE funding",
            str(exc),
        )
        raise
